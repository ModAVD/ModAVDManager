//go:build windows

package emu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/win"
)

// createHooks replaces the setup's boot, check and shutdown for one test.
type createHooks struct {
	start   func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error)
	check   error
	stopped int
	opts    []StartOptions
}

func (h *createHooks) install(t *testing.T) {
	t.Helper()
	savedStart, savedStop, savedCheck := createStart, createStop, createCheck
	t.Cleanup(func() { createStart, createStop, createCheck = savedStart, savedStop, savedCheck })
	createStart = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		h.opts = append(h.opts, opt)
		if h.start != nil {
			return h.start(m, ctx, i, opt)
		}
		return &StartResult{HostIME: true, LogFile: `D:\r\logs\mas_6-boot.log`}, nil
	}
	createCheck = func(*Manager, context.Context, int, bool, *StartResult) error { return h.check }
	createStop = func(*Manager, context.Context, int) error { h.stopped++; return nil }
}

// TestCreateSetsUp: create sets the instance up before it returns; meanwhile it is "creating" and every other
// operation on it is refused as busy; afterwards nothing of the creation state is left.
func TestCreateSetsUp(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	var h createHooks
	var during *InstanceInfo
	var refused []error
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		during, _ = m.Info(ctx, i)
		_, e1 := m.Start(ctx, i, StartOptions{})
		_, e2 := m.Update(ctx, i, UpdateRequest{Title: new("x")})
		_, e3 := m.Delete(ctx, i)
		_, e4 := m.Bridge(ctx, i, "verify")
		j := i
		_, e5 := m.Create(ctx, CreateRequest{Index: &j})
		refused = []error{e1, e2, e3, e4, e5}
		return &StartResult{HostIME: true}, nil
	}
	h.install(t)
	var steps []Progress
	i := 6
	info, err := m.Create(ctx, CreateRequest{Index: &i, Progress: func(p Progress) { steps = append(steps, p) }})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if during == nil || during.Status != StatusCreating || during.Busy != "creating" || during.Creation != "creating" {
		t.Fatalf("during the setup: %+v", during)
	}
	for k, e := range refused {
		if errs.CodeOf(e) != errs.Busy {
			t.Errorf("operation %d during the setup: %v", k, e)
		}
	}
	if info.Status != StatusOffline || info.Creation != "" || info.CreationError != "" || h.stopped != 1 {
		t.Fatalf("after: %+v (stopped %d)", info, h.stopped)
	}
	if meta := m.Root.InstanceMeta(6); meta == nil || meta.Get("creationState") != nil || meta.Get("creationError") != nil {
		t.Fatal("creation keys left in mas-avd.json")
	}
	if len(h.opts) != 1 || !h.opts[0].creating || h.opts[0].NoBridge {
		t.Fatalf("setup boot options %+v", h.opts)
	}
	if len(steps) < 3 || steps[0].Step != "create" || steps[len(steps)-1].Step != "done" || steps[len(steps)-1].Done != progressTotal {
		t.Fatalf("progress %+v", steps)
	}
	if _, err := m.Start(ctx, 6, StartOptions{watchQemu: &win.Process{}}); errs.CodeOf(err) == errs.Busy &&
		strings.Contains(err.Error(), "新建") {
		t.Fatal("a created instance is still refused")
	}
}

// TestCreateFailsThenResumes: a failed setup shuts the instance down, leaves create_failed with the reason and the
// log; start / set say not ready, delete may go; --resume (with the bridge setting recorded at create) sets it up.
func TestCreateFailsThenResumes(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	var h createHooks
	h.start = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
		return &StartResult{HostIME: true, LogFile: `D:\r\logs\mas_6-x.log`}, nil
	}
	h.check = errs.New(errs.NotReady, "没做完：宿主输入法")
	h.install(t)
	i := 6
	_, err := m.Create(ctx, CreateRequest{Index: &i, NoBridge: true})
	opsSkipIfNotImplemented(t, err)
	if errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "新建失败") || !strings.Contains(err.Error(), "--resume 6") {
		t.Fatalf("failed create: %v", err)
	}
	if h.stopped != 1 {
		t.Fatalf("a failed setup did not shut down (%d)", h.stopped)
	}
	info, err := m.Info(ctx, 6)
	if err != nil || info.Status != StatusCreateFailed || info.Creation != "failed" ||
		!strings.Contains(info.CreationError, "宿主输入法") || info.CreationLog != `D:\r\logs\mas_6-x.log` {
		t.Fatalf("after the failure: %+v %v", info, err)
	}
	if _, err := m.Start(ctx, 6, StartOptions{}); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("start of a failed instance: %v", err)
	}
	if _, err := m.Update(ctx, 6, UpdateRequest{Title: new("x")}); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("set of a failed instance: %v", err)
	}

	h.check = nil
	h.opts = nil
	info2, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true})
	if err != nil || info2.Status != StatusOffline || info2.Creation != "" {
		t.Fatalf("resume: %+v %v", info2, err)
	}
	if len(h.opts) != 1 || !h.opts[0].NoBridge {
		t.Fatalf("resume did not keep --no-bridge: %+v", h.opts)
	}
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true}); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("resume of a ready instance: %v", err)
	}
	if _, err := m.Create(ctx, CreateRequest{Resume: true}); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("resume without index: %v", err)
	}
	seven := 7
	if _, err := m.Create(ctx, CreateRequest{Index: &seven, Resume: true}); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("resume of a missing instance: %v", err)
	}
	if _, err := m.Delete(ctx, 6); err != nil {
		t.Fatal(err)
	}
}

// TestCreateKilled: "creating" that nobody works on (the create was killed, its lock went with it) reads as failed and
// can be resumed or deleted.
func TestCreateKilled(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	i := 6
	if _, err := m.Create(ctx, CreateRequest{Index: &i, filesOnly: true}); err != nil {
		opsSkipIfNotImplemented(t, err)
		t.Fatal(err)
	}
	if err := m.Root.EditInstanceMeta(6, func(meta *jsonord.Obj) error {
		meta.Set("creationState", jsonord.NewString("creating"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := m.Info(ctx, 6)
	if err != nil || info.Status != StatusCreateFailed || info.CreationError != interruptedCreation {
		t.Fatalf("killed create: %+v %v", info, err)
	}
	if _, err := m.Start(ctx, 6, StartOptions{}); errs.CodeOf(err) != errs.NotReady ||
		!strings.Contains(err.Error(), "新建时初始化失败，不能开机："+interruptedCreation+"。") {
		t.Errorf("start after a killed create: %v", err)
	}
	// while somebody holds the lock it is creating
	l, ok, err := win.TryLock(m.lockPath(6, "create"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	info, _ = m.Info(ctx, 6)
	if _, derr := m.Delete(ctx, 6); errs.CodeOf(derr) != errs.Busy {
		t.Errorf("delete while creating: %v", derr)
	}
	l.Unlock()
	if info.Status != StatusCreating {
		t.Fatalf("with the lock held: %+v", info)
	}
	var h createHooks
	h.install(t)
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true}); err != nil {
		t.Fatal(err)
	}
}

// TestCreateTimeout: --timeout bounds the whole setup; on the deadline the instance is shut down (on a context of its
// own) and marked failed with a timeout.
func TestCreateTimeout(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		<-ctx.Done()
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	h.install(t)
	i := 6
	t0 := time.Now()
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, Timeout: 50 * time.Millisecond})
	opsSkipIfNotImplemented(t, err)
	if errs.CodeOf(err) != errs.Timeout || time.Since(t0) > 5*time.Second || h.stopped != 1 {
		t.Fatalf("timeout: %v after %v, stopped %d", err, time.Since(t0), h.stopped)
	}
	if info, _ := m.Info(context.Background(), 6); info == nil || info.Status != StatusCreateFailed {
		t.Fatalf("after the timeout: %+v", info)
	}

	// Ctrl+C: canceled, shut down all the same
	h.stopped = 0
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Create(cctx, CreateRequest{Index: &i, Resume: true}); errs.CodeOf(err) != errs.Canceled || h.stopped != 1 ||
		errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled: %v, stopped %d", err, h.stopped)
	}
}

// TestCreateProgress: the boots of the setup (the first one, then a cold boot per bridge round) make one bar that
// never goes back and stays below the shutdown step.
func TestCreateProgress(t *testing.T) {
	var got []Progress
	p := &createProgress{fn: func(e Progress) { got = append(got, e) }}
	p.emit("create", "开始", 20)
	for _, ev := range []Progress{{Step: "launch", Done: progressLaunch}, {Step: "boot", Done: progressBooted},
		{Step: "first_boot", Done: progressFirstBoot}, {Step: "done", Done: progressTotal},
		{Step: "bridge", Done: 0}, {Step: "launch", Done: progressLaunch}, {Step: "done", Done: progressTotal},
		{Step: "bridge", Done: 0}, {Step: "launch", Done: progressLaunch}, {Step: "done", Done: progressTotal},
		{Step: "bridge", Done: 0}, {Step: "launch", Done: progressLaunch}, {Step: "done", Done: progressTotal}} {
		p.boot(ev)
	}
	p.emit("create_stop", "关机", 930)
	p.emit("done", "完成", progressTotal)
	last := -1
	for k, e := range got {
		if e.Done <= last || e.Total != progressTotal {
			t.Fatalf("step %d went back or wrong total: %+v", k, got)
		}
		if k > 0 && k < len(got)-2 && (e.Done < 20 || e.Done > 900 || e.Step == "done") {
			t.Fatalf("boot step %d out of 20–900 or named done: %+v", k, e)
		}
		last = e.Done
	}
	if got[len(got)-1].Done != progressTotal || got[len(got)-1].Step != "done" {
		t.Fatalf("end %+v", got[len(got)-1])
	}
}
