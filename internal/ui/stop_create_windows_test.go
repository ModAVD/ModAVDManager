package ui

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

func TestStopCreatingInstance(t *testing.T) {
	h := newHarness(t, func(opt *Options) {
		r := opt.Root
		if _, err := r.EnsureMetadata(); err != nil {
			t.Fatal(err)
		}
		if _, err := avd.Create(r, 3, avd.CreateOptions{MemoryMB: 3072, CPU: 4, DataPartitionGB: 64, FPS: 60, Creating: true}); err != nil {
			t.Fatal(err)
		}
		l, ok, err := win.TryLock(filepath.Join(r.AvdHome(), ".mas_3.create.lock"))
		if err != nil || !ok {
			t.Fatalf("create lock: %v %v", ok, err)
		}
		t.Cleanup(l.Unlock)
		opt.newBackend = func(log *logx.Logger) backend { return managerBackend{emu.New(r, log)} }
	})
	for n, body := range []string{`{}`, `{"force":true}`, `{"cancel_create":true}`} {
		var j jobView
		expect(t, h.do("POST", "/api/instances/3/stop", body), http.StatusAccepted, &j)
		if n < 2 {
			failed := h.waitJob(j.ID, stateIs(stateFailed))
			if failed.Error == nil || failed.Error.Code != errs.Busy || !strings.Contains(failed.Error.Advice, "--cancel-create") {
				t.Fatalf("default stop: %+v", failed)
			}
			if d := failed.Error.Details; d[errs.Reason] != "locked" || d[errs.Lock] != "create" {
				t.Errorf("job error details: %v", d)
			}
		} else {
			finished := h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
			if finished.State != stateDone {
				t.Fatalf("external cancel: %+v", finished)
			}
			var done struct {
				Result emu.StopResult `json:"result"`
			}
			expect(t, h.do("GET", "/api/jobs/"+j.ID, nil), http.StatusOK, &done)
			if !done.Result.InterruptedCreate || done.Result.Method != "already_offline" {
				t.Fatalf("external cancel: %+v", done.Result)
			}
		}
	}
}
