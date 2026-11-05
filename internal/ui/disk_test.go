//go:build windows

package ui

import (
	"net/http"
	"slices"
	"strconv"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

// diskJob runs POST /api/instances/{i}/{kind} to the end and returns the finished job with its result decoded into out.
func diskJob(t *testing.T, h *harness, i int, kind string, out any) jobView {
	t.Helper()
	var j jobView
	expect(t, h.do("POST", "/api/instances/"+strconv.Itoa(i)+"/"+kind, `{}`), http.StatusAccepted, &j)
	if j.Kind != kind || j.Index != i || j.State != stateRunning {
		t.Fatalf("%s job %+v", kind, j)
	}
	h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
	done := struct {
		jobView
		Result any `json:"result"`
	}{Result: out}
	expect(t, h.do("GET", "/api/jobs/"+j.ID, nil), http.StatusOK, &done)
	return done.jobView
}

func TestDiskJobs(t *testing.T) {
	h := newHarness(t, nil)

	// info works while the instance is off: every size is there
	var info emu.DiskInfoResult
	v := diskJob(t, h, 3, kindDiskInfo, &info)
	if v.State != stateDone || v.Error != nil || v.Progress == nil || v.Progress.Step != "done" {
		t.Fatalf("info job %+v", v)
	}
	if info.Index != 3 || info.LogicalBytes != 64<<30 || info.OnDiskBytes == nil || *info.OnDiskBytes != 12<<30 || info.NeedsShutdown ||
		info.AllocatedBytes == nil || *info.AllocatedBytes != 10<<30 || info.ReclaimableBytes == nil || *info.ReclaimableBytes != 2<<30 {
		t.Fatalf("info result %+v", info)
	}

	// ... and while it runs: allocated and reclaimable are left out
	h.f.put(4, emu.StatusOnline)
	var running emu.DiskInfoResult
	v = diskJob(t, h, 4, kindDiskInfo, &running)
	if v.State != stateDone || !running.Running || !running.NeedsShutdown || running.AllocatedBytes != nil || running.ReclaimableBytes != nil {
		t.Fatalf("info of a running instance %+v (%+v)", running, v)
	}

	var trim emu.DiskTrimResult
	v = diskJob(t, h, 4, kindDiskTrim, &trim)
	if v.State != stateDone || trim.Index != 4 || trim.DiscardMaxBytes != 2<<30 || v.Progress == nil || v.Progress.Step != "done" {
		t.Fatalf("trim job %+v (%+v)", v, trim)
	}

	var compact emu.DiskCompactResult
	v = diskJob(t, h, 3, kindDiskCompact, &compact)
	if v.State != stateDone || compact.FreedBytes != 2<<30 || compact.BeforeOnDiskBytes != 12<<30 || compact.AfterOnDiskBytes != 10<<30 {
		t.Fatalf("compact job %+v (%+v)", v, compact)
	}
	if v.Progress == nil || v.Progress.Message != "实例 3 已整理磁盘，释放 2.0 GB" {
		t.Fatalf("compact progress %+v", v.Progress)
	}

	h.f.mu.Lock()
	calls := slices.Clone(h.f.disk)
	h.f.mu.Unlock()
	if !slices.Equal(calls, []string{"info 3", "info 4", "trim 4", "compact 3"}) {
		t.Fatalf("backend calls %v", calls)
	}

	// the result keeps emu's snake_case names (the page reads them)
	var raw map[string]any
	j := diskJob(t, h, 3, kindDiskInfo, &raw)
	for _, k := range []string{"logical_bytes", "on_disk_bytes", "allocated_bytes", "reclaimable_bytes"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("job %s: result has no %q: %v", j.ID, k, raw)
		}
	}
}

// emu refuses a trim of an instance that is not booted and a compact of one that runs; the job fails with its error.
func TestDiskJobError(t *testing.T) {
	h := newHarness(t, nil)
	h.f.diskErr = errs.New(errs.Unsupported, "实例没有在运行").WithAdvice("先开机")

	v := diskJob(t, h, 3, kindDiskTrim, nil)
	if v.State != stateFailed || v.Error == nil || v.Error.Code != errs.Unsupported || v.Error.Message != "实例没有在运行" ||
		v.Error.Advice != "先开机" {
		t.Fatalf("failed trim job %+v (error %+v)", v, v.Error)
	}
}

func TestDiskCompactBusy(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.f.diskGate = gate

	var j jobView
	expect(t, h.do("POST", "/api/instances/3/disk-compact", `{}`), http.StatusAccepted, &j)
	e := expectError(t, h.do("POST", "/api/instances/3/disk-info", `{}`), http.StatusConflict, errs.Busy)
	t.Logf("busy message: %s", e.Message)
	expectError(t, h.do("POST", "/api/instances/3/start", `{}`), http.StatusConflict, errs.Busy)
	expectError(t, h.do("DELETE", "/api/instances/3?confirm=1", nil), http.StatusConflict, errs.Busy)
	// another instance is not affected
	expect(t, h.do("POST", "/api/instances/4/disk-info", `{}`), http.StatusAccepted, nil)

	close(gate)
	h.waitJob(j.ID, stateIs(stateDone))
}

func TestDiskBadRequests(t *testing.T) {
	h := newHarness(t, nil)
	for _, kind := range []string{kindDiskInfo, kindDiskTrim, kindDiskCompact} {
		expectError(t, h.do("POST", "/api/instances/3/"+kind, `{"force":true}`), 400, errs.Usage)
		expectError(t, h.do("POST", "/api/instances/5/"+kind, `{}`), 400, errs.InvalidArgument)
	}
	h.f.mu.Lock()
	n := len(h.f.disk)
	h.f.mu.Unlock()
	if n != 0 {
		t.Fatalf("refused requests still called the backend %d times", n)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 2048: "2 KB", 5 << 20: "5 MB", 3 << 30: "3.0 GB", 3<<30 + 512<<20: "3.5 GB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
