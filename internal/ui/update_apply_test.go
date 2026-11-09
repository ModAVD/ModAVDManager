//go:build windows

package ui

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/testutil"
	"modavd/internal/update"
)

// applyFakes are the update job's three outside steps.
type applyFakes struct {
	mu       sync.Mutex
	prepared *update.Status
	prepErr  error
	prepGate chan struct{} // prepare waits here when set
	prepares int
	version  string // what tools\modavd.exe says it is
	verErr   error
	inUse    []string // other programs of the root running
	inUseErr error
	spawnErr error
	dead     bool       // the started CLI is gone at once
	spawns   [][]string // exe, then the args, then the log path
}

func (a *applyFakes) spawned() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.spawns)
}

const reopenExe = `C:\ModAVD\ModAVD.exe`

// applyHarness: a manager that can update itself (Reopen set, a tools\modavd.exe of our version), with fakes for the
// download, the CLI and the spawn.
func applyHarness(t *testing.T, a *applyFakes) *harness {
	t.Helper()
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t))
	if a.version == "" {
		a.version = buildinfo.Version
	}
	h := newHarness(t, func(o *Options) {
		o.Reopen = &Reopen{Exe: reopenExe, Args: []string{"--root", o.Root.Dir, "--no-window"}}
		o.prepareUpdate = func(ctx context.Context, lg *logx.Logger, progress func(update.Progress)) (*update.Status, error) {
			a.mu.Lock()
			a.prepares++
			gate := a.prepGate
			a.mu.Unlock()
			progress(update.Progress{Stage: "downloading", Message: "从 GitHub 下载 ModAVD-0.40.0-win-x64.zip", Percent: 50,
				Done: 100 << 20, Total: 200 << 20})
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
				}
			}
			return a.prepared, a.prepErr
		}
		o.cliVersion = func(exe string) (string, error) { return a.version, a.verErr }
		o.programsInUse = func() ([]string, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.inUse, a.inUseErr
		}
		o.spawn = func(exe string, args []string, logPath string) (func() bool, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.spawns = append(a.spawns, append(append([]string{exe}, args...), logPath))
			dead := a.dead
			return func() bool { return !dead }, a.spawnErr
		}
		if err := os.MkdirAll(filepath.Join(o.Root.Dir, "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.Root.Dir, "tools", "modavd.exe"), []byte("cli"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	return h
}

func preparedStatus() *update.Status {
	return &update.Status{Updatable: true, CurrentVersion: "v0.39.0", Checked: true, LatestVersion: "v0.40.0",
		LatestSource: "github", UpdateAvailable: true, PreparedVersion: "v0.40.0", Phase: "staged",
		Message: "0.40.0 已下载并核对好，可以安装", Notices: []string{}}
}

func (h *harness) apply() jobView {
	h.t.Helper()
	var v jobView
	expect(h.t, h.do("POST", "/api/update/apply", `{}`), http.StatusAccepted, &v)
	if v.Kind != kindUpdate || v.Index != rootJobIndex {
		h.t.Fatalf("job %+v", v)
	}
	return v
}

func (h *harness) exited(within time.Duration) bool {
	select {
	case <-h.s.exitReq:
		return true
	case <-time.After(within):
		return false
	}
}

// The whole hand-off: download, the running instance stopped (never a creation cancelled), the CLI started with our
// pid and how to start us again, then the manager exits.
func TestUpdateApplyHandsOff(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus()}
	h := applyHarness(t, a)
	h.f.put(3, emu.StatusOnline)
	var v map[string]any
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	if v["can_install"] != true || v["install_by_cli"] != false {
		t.Fatalf("GET /api/update: %v", v)
	}
	j := h.apply()
	done := h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
	if done.State != stateDone {
		t.Fatalf("job %+v", done)
	}
	r, _ := done.Result.(map[string]any)
	if r["handoff"] != true || r["version"] != "v0.40.0" || !strings.Contains(r["message"].(string), "0.40.0") {
		t.Fatalf("result %v", r)
	}
	if st, _ := r["stopped"].([]any); len(st) != 1 || st[0] != float64(3) {
		t.Errorf("stopped %v", r["stopped"])
	}
	h.f.mu.Lock()
	stops := slices.Clone(h.f.stops)
	h.f.mu.Unlock()
	if len(stops) != 1 || stops[0].CancelCreate || stops[0].Force {
		t.Errorf("stops %+v", stops)
	}
	sp := a.spawned()
	if len(sp) != 1 {
		t.Fatalf("spawns %v", sp)
	}
	root := h.s.root.Dir
	want := []string{filepath.Join(root, "tools", "modavd.exe"), "--root", root, "--json", "update", "install",
		"--after-pid", strconv.Itoa(os.Getpid()), "--reopen", reopenExe,
		"--reopen-arg", "--root", "--reopen-arg", root, "--reopen-arg", "--no-window",
		filepath.Join(root, "logs", updateLogName)}
	if !slices.Equal(sp[0], want) {
		t.Fatalf("spawn\n got  %q\n want %q", sp[0], want)
	}
	// from the hand-off on, nothing may start: the manager is about to exit
	e := expectError(t, h.do("POST", "/api/instances/4/start", `{}`), 409, errs.Busy)
	if !strings.Contains(e.Message, "交给命令行") {
		t.Errorf("%+v", e)
	}
	if !h.exited(5 * time.Second) {
		t.Fatal("the manager did not exit after the hand-off")
	}
	// the check the download did is the panel's now
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	if v["latest_version"] != "v0.40.0" || v["checked_at"] == "" {
		t.Errorf("after: %v", v)
	}
}

func TestUpdateApplyUpToDate(t *testing.T) {
	a := &applyFakes{prepared: &update.Status{Updatable: true, CurrentVersion: "v0.40.0", Checked: true,
		LatestVersion: "v0.40.0", Message: "已经是最新版本（0.40.0）", Notices: []string{}}}
	h := applyHarness(t, a)
	h.f.put(3, emu.StatusOnline)
	j := h.apply()
	done := h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
	r, _ := done.Result.(map[string]any)
	if done.State != stateDone || r["handoff"] != false || r["message"] != "已经是最新版本（0.40.0）" {
		t.Fatalf("job %+v", done)
	}
	h.f.mu.Lock()
	n := len(h.f.stops)
	h.f.mu.Unlock()
	if n != 0 || len(a.spawned()) != 0 || h.exited(2*handoffExitDelay) {
		t.Fatalf("stops %d, spawns %v, or exited", n, a.spawned())
	}
}

// A staged version that is not newer than the one installed (the package was copied over by hand since): up to date,
// nothing stopped.
func TestUpdateApplyStagedNotNewer(t *testing.T) {
	st := preparedStatus()
	st.CurrentVersion = "v0.40.0"
	a := &applyFakes{prepared: st}
	h := applyHarness(t, a)
	h.f.put(3, emu.StatusOnline)
	done := h.waitJob(h.apply().ID, func(v jobView) bool { return v.State != stateRunning })
	r, _ := done.Result.(map[string]any)
	if done.State != stateDone || r["handoff"] != false || r["message"] != "已经是最新版本" {
		t.Fatalf("job %+v", done)
	}
	h.f.mu.Lock()
	n := len(h.f.stops)
	h.f.mu.Unlock()
	if n != 0 || len(a.spawned()) != 0 {
		t.Fatalf("stops %d, spawns %v", n, a.spawned())
	}
}

// A step that fails ends the job there: nothing after it runs, the manager stays.
func TestUpdateApplyFailures(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      func(a *applyFakes, f *fake)
		code     string
		stops    int
		spawns   int
		contains string
	}{
		{"download", func(a *applyFakes, f *fake) {
			a.prepared, a.prepErr = &update.Status{Notices: []string{}}, errs.New(errs.UpdateUnavailable, "查不了最新版本")
		}, errs.UpdateUnavailable, 0, 0, "查不了"},
		{"stop", func(a *applyFakes, f *fake) {
			f.stopErr = errs.New(errs.Busy, "实例正在新建")
		}, errs.Busy, 1, 0, "实例 3：实例正在新建"},
		{"spawn", func(a *applyFakes, f *fake) {
			a.spawnErr = errors.New("拒绝访问")
		}, errs.LaunchFailed, 1, 1, "拒绝访问"},
		{"cli gone at once", func(a *applyFakes, f *fake) {
			a.dead = true
		}, errs.LaunchFailed, 1, 1, "马上退出"},
		// checked before any instance is stopped
		{"another program of the root", func(a *applyFakes, f *fake) {
			a.inUse = []string{"modavd.exe（pid 9）"}
		}, errs.Busy, 0, 0, "modavd.exe（pid 9）"},
		{"process scan", func(a *applyFakes, f *fake) {
			a.inUseErr = errors.New("快照失败")
		}, errs.Internal, 0, 0, "快照失败"},
		{"an instance being created", func(a *applyFakes, f *fake) {
			f.put(4, emu.StatusCreating)
		}, errs.Busy, 0, 0, "实例 4 正在新建"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &applyFakes{prepared: preparedStatus()}
			h := applyHarness(t, a)
			h.f.put(3, emu.StatusOnline)
			c.set(a, h.f)
			j := h.apply()
			done := h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
			if done.State != stateFailed || done.Error == nil || done.Error.Code != c.code || !strings.Contains(done.Error.Message, c.contains) {
				t.Fatalf("job %+v %+v", done, done.Error)
			}
			if c.code != errs.UpdateUnavailable && c.code != errs.Internal && !strings.Contains(done.Error.Advice, "不用重新下载") {
				t.Errorf("advice %q", done.Error.Advice)
			}
			h.f.mu.Lock()
			n := len(h.f.stops)
			h.f.mu.Unlock()
			if n != c.stops || len(a.spawned()) != c.spawns || h.exited(2*handoffExitDelay) {
				t.Fatalf("stops %d, spawns %d, or exited", n, len(a.spawned()))
			}
		})
	}
}

// Refused before any job: no tools\modavd.exe, or one of another version (it may not know --after-pid / --reopen).
func TestUpdateApplyRefusedUpFront(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus(), version: "0.0.1"}
	h := applyHarness(t, a)
	e := expectError(t, h.do("POST", "/api/update/apply", `{}`), 422, errs.NotReady)
	if !strings.Contains(e.Message, "0.0.1") || !strings.Contains(e.Advice, "update install") {
		t.Errorf("%+v", e)
	}
	a.mu.Lock()
	a.version, a.verErr = "", errors.New("exit status 1")
	a.mu.Unlock()
	expectError(t, h.do("POST", "/api/update/apply", `{}`), 422, errs.NotReady)
	if err := os.Remove(filepath.Join(h.s.root.Dir, "tools", "modavd.exe")); err != nil {
		t.Fatal(err)
	}
	expectError(t, h.do("POST", "/api/update/apply", `{}`), 422, errs.NotReady)
	if a.prepares != 0 || len(h.s.listJobs()) != 0 {
		t.Fatalf("prepares %d, jobs %v", a.prepares, h.s.listJobs())
	}
}

// The update runs alone: not while another job runs, and no other job while it runs.
func TestUpdateApplyRunsAlone(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus(), prepGate: make(chan struct{})}
	h := applyHarness(t, a)
	j := h.apply()
	expectError(t, h.do("POST", "/api/instances/3/start", `{}`), 409, errs.Busy)
	expectError(t, h.do("POST", "/api/update/apply", `{}`), 409, errs.Busy)
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil), http.StatusOK, nil)
	h.waitJob(j.ID, func(v jobView) bool { return v.State == stateCanceled })
	if len(a.spawned()) != 0 || h.exited(2*handoffExitDelay) {
		t.Fatal("a cancelled update handed off or exited")
	}

	h.f.mu.Lock()
	h.f.startGate = make(chan struct{})
	h.f.mu.Unlock()
	var sj jobView
	expect(t, h.do("POST", "/api/instances/3/start", `{}`), http.StatusAccepted, &sj)
	e := expectError(t, h.do("POST", "/api/update/apply", `{}`), 409, errs.Busy)
	if !strings.Contains(e.Message, "开机") {
		t.Errorf("%+v", e)
	}
	close(h.f.startGate)
	h.waitJob(sj.ID, func(v jobView) bool { return v.State != stateRunning })
}

// What the CLI left after the swap is shown by the manager it started, once.
func TestUpdateHandoffShownAfterRestart(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t))
	h := newHarness(t, func(o *Options) {
		res := update.NewHandoffResult(&update.Status{Updated: true, CurrentVersion: "v0.40.0", Message: "已更新到 0.40.0"}, nil, time.Now())
		if err := update.SaveHandoff(o.Root, res); err != nil {
			t.Fatal(err)
		}
	})
	var v map[string]any
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	hv, _ := v["handoff"].(map[string]any)
	if hv == nil || hv["ok"] != true || hv["message"] != "已更新到 0.40.0" || hv["version"] != "v0.40.0" {
		t.Fatalf("handoff %v", v["handoff"])
	}
	if update.TakeHandoff(h.s.root, time.Now()) != nil {
		t.Error("the file was left for the next start")
	}
}
