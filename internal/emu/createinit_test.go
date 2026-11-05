//go:build windows

package emu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

// createHooks replaces the setup's boot, check and shutdown (and the qemu scan of resume) for one test.
type createHooks struct {
	start   func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error)
	check   error
	stopped int
	stopErr error
	onStop  func(m *Manager, i int)
	running bool     // a qemu of the instance runs (until a stop)
	order   []string // "stop" / "start", as called
	opts    []StartOptions
}

func (h *createHooks) install(t *testing.T) {
	t.Helper()
	savedStart, savedStop, savedCheck, savedRunning := createStart, createStop, createCheck, createRunning
	t.Cleanup(func() {
		createStart, createStop, createCheck, createRunning = savedStart, savedStop, savedCheck, savedRunning
	})
	createStart = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		h.opts = append(h.opts, opt)
		h.order = append(h.order, "start")
		if h.start != nil {
			return h.start(m, ctx, i, opt)
		}
		return &StartResult{HostIME: true, LogFile: `D:\r\logs\mas_6-boot.log`}, nil
	}
	createCheck = func(*Manager, context.Context, int, bool, *StartResult) error { return h.check }
	createStop = func(m *Manager, _ context.Context, i int) error {
		h.stopped++
		h.order = append(h.order, "stop")
		if h.onStop != nil {
			h.onStop(m, i)
		}
		if h.stopErr != nil {
			return h.stopErr
		}
		h.running = false
		return nil
	}
	createRunning = func(*Manager, int) (bool, error) { return h.running, nil }
}

// TestCreateResumeStopsLeftover: --resume shuts down (normally) the emulator the failed setup left running, then sets
// the instance up; when that shutdown fails it changes nothing.
func TestCreateResumeStopsLeftover(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	var h createHooks
	h.check = errs.New(errs.NotReady, "没做完：宿主输入法")
	h.install(t)
	i := 6
	if _, err := m.Create(ctx, CreateRequest{Index: &i, NoBridge: true}); errs.CodeOf(err) != errs.NotReady {
		opsSkipIfNotImplemented(t, err)
		t.Fatalf("create: %v", err)
	}
	h.check, h.running, h.order = nil, true, nil
	h.stopErr = errs.New(errs.Timeout, "关机超时")
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true}); errs.CodeOf(err) != errs.Timeout ||
		!strings.Contains(err.Error(), "modavd stop 6") {
		t.Fatalf("resume with a qemu that does not stop: %v", err)
	}
	if !slices.Equal(h.order, []string{"stop"}) {
		t.Fatalf("calls %v", h.order)
	}
	if info, _ := m.Info(ctx, 6); info == nil || info.Status != StatusCreateFailed {
		t.Fatalf("after a resume that could not stop: %+v", info)
	}
	h.stopErr, h.order = nil, nil
	info, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true})
	if err != nil || info.Status != StatusOffline || info.Creation != "" {
		t.Fatalf("resume: %+v %v", info, err)
	}
	if !slices.Equal(h.order, []string{"stop", "start", "stop"}) {
		t.Fatalf("calls %v, want the leftover stopped first", h.order)
	}
}

// TestCreateResumeDropsOldProgress: --resume of a killed setup does not show the percentage that setup left on disk
// while it shuts the leftover emulator down: from the moment it holds the lock, status reads creating at 0%.
func TestCreateResumeDropsOldProgress(t *testing.T) {
	m, _ := opsTestManager(t)
	other := New(m.Root, nil)
	ctx := context.Background()
	i := 6
	if _, err := m.Create(ctx, CreateRequest{Index: &i, filesOnly: true}); err != nil {
		opsSkipIfNotImplemented(t, err)
		t.Fatal(err)
	}
	// killed at 63%: "creating" recorded, its progress file and its emulator left behind
	if err := m.Root.EditInstanceMeta(6, func(meta *jsonord.Obj) error {
		meta.Set("creationState", jsonord.NewString("creating"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.writeCreateProgress(6, Progress{Step: "boot", Done: 630, Total: progressTotal})
	if got, _ := other.Info(ctx, 6); got == nil || got.Status != StatusCreateFailed {
		t.Fatalf("killed: %+v", got)
	}
	var h createHooks
	var during []*InstanceInfo
	h.running = true
	h.onStop = func(*Manager, int) {
		info, _ := other.Info(ctx, 6)
		during = append(during, info)
	}
	h.install(t)
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true, NoBridge: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.order, []string{"stop", "start", "stop"}) {
		t.Fatalf("calls %v", h.order)
	}
	if left := during[0]; left == nil || left.Status != StatusCreating || left.CreationProgress != 0 {
		t.Fatalf("while the leftover shuts down: %+v", left)
	}
	if fileOrDirExists(m.createProgressPath(6)) {
		t.Fatal("progress file left")
	}
}

// TestStatusCreatingThroughGetprop: a status whose getprop waits while the setup ends (the setup's shutdown is what
// makes it wait) reads creating with the percentage, as at its process snapshot, not a "starting" qemu.
func TestStatusCreatingThroughGetprop(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	i := 6
	if _, err := m.Create(ctx, CreateRequest{Index: &i, filesOnly: true}); err != nil {
		opsSkipIfNotImplemented(t, err)
		t.Fatal(err)
	}
	if err := m.setCreation(6, creationCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	l, ok, err := win.TryLock(m.lockPath(6, "create"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	unlocked := false
	t.Cleanup(func() {
		if !unlocked {
			l.Unlock()
		}
	})
	m.writeCreateProgress(6, Progress{Step: "create_stop", Done: 880, Total: progressTotal})
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: {PID: 4242}} }
	statusBooted = func(m *Manager, _ context.Context, i int) bool {
		// the setup is over while the guest shuts down
		m.removeCreateProgress(i)
		if err := m.setCreation(i, "", "", ""); err != nil {
			t.Error(err)
		}
		l.Unlock()
		unlocked = true
		return false
	}
	info, err := m.Info(ctx, 6)
	if err != nil || !info.Running || info.Status != StatusCreating || info.Busy != "creating" || info.CreationProgress != 88 {
		t.Fatalf("a setup that ends during the getprop: %+v %v", info, err)
	}
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
		_, e6 := m.Stop(ctx, i, StopOptions{})
		refused = []error{e1, e2, e3, e4, e5, e6}
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

// A bridge restart can embed Start's "emulator left running" advice in its error.
// The creation's cleanup changes that fact, so its final error must describe the
// state after cleanup, while retaining the original cause for diagnostics.
func TestCreateCanceledReportsFinalShutdown(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			m, _ := opsTestManager(t)
			startErr := errs.New(errs.Canceled, "装执行桥时关机再开机失败: 已取消。模拟器没有关：稍后再启动")
			h := &createHooks{running: true}
			h.start = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
				return nil, startErr
			}
			if !stopped {
				h.stopErr = errs.New(errs.Timeout, "关机超时")
			}
			h.install(t)
			i := 6
			_, err := m.Create(context.Background(), CreateRequest{Index: &i})
			if errs.CodeOf(err) != errs.Canceled || !errors.Is(err, startErr) || h.stopped != 1 {
				t.Fatalf("create: %v; shutdown calls %d", err, h.stopped)
			}
			info, ierr := m.Info(context.Background(), i)
			if ierr != nil || info.Status != StatusCreateFailed || info.Creation != creationFailed {
				t.Fatalf("persisted state: %+v %v", info, ierr)
			}
			if stopped {
				if strings.Contains(info.CreationError, "模拟器没有关") || !strings.Contains(info.CreationError, "已关机") ||
					!strings.Contains(errs.As(err).Message, "已关机") {
					t.Fatalf("stale startup state after successful cleanup: %q; %v", info.CreationError, err)
				}
			} else if strings.Contains(info.CreationError, "已关机") || info.CreationError != startErr.Message {
				t.Fatalf("must not claim a failed cleanup stopped Android: %q", info.CreationError)
			}
		})
	}
}

// oneBoot is what Start reports for one boot of the setup: the boot wait (polled by the clock), then the steps after
// it. firstBoot adds the first-boot setup.
func oneBoot(firstBoot bool) []Progress {
	evs := []Progress{{Step: "launch", Done: progressLaunch}, {Step: "boot", Done: 150}, {Step: "boot", Done: 200},
		{Step: "boot", Done: progressBooted}, {Step: "tune", Done: progressTune}, {Step: "ime", Done: progressIme}}
	if firstBoot {
		evs = append(evs, Progress{Step: "first_boot", Done: progressFirstBoot})
	}
	return append(evs, Progress{Step: "vulkan_fix", Done: progressVulkanFix},
		Progress{Step: "done", Message: "实例 6 已就绪（共 45.7 秒）", Done: progressTotal})
}

// runCreateProgress feeds a setup's events into a createProgress with plan and returns what it sent. bridgeRounds
// rounds with a cold boot follow the first boot; check adds the bridge round that only finds it installed.
func runCreateProgress(plan []createStage, bridgeRounds int, check bool, bootLog func() string) []Progress {
	var got []Progress
	p := &createProgress{i: 6, plan: plan, bootLog: bootLog, fn: func(e Progress) { got = append(got, e) }}
	p.stage("create", "开始")
	for _, ev := range oneBoot(true) {
		p.boot(ev)
	}
	for range bridgeRounds {
		p.boot(Progress{Step: "bridge", Total: progressTotal})
		for _, ev := range oneBoot(false) {
			p.boot(ev)
		}
	}
	if check {
		p.boot(Progress{Step: "bridge", Total: progressTotal})
	}
	p.stage("create_stop", "关机")
	p.stage("done", "完成")
	return got
}

// checkBar: Done never goes back, stays below 100 % until the last event, which is done at 100 %; the setup's own
// boots never say ready.
func checkBar(t *testing.T, got []Progress) {
	t.Helper()
	last := -1
	for k, e := range got {
		if e.Done < last || e.Total != progressTotal {
			t.Fatalf("event %d went back or has the wrong total: %+v", k, got)
		}
		if k < len(got)-1 && (e.Done >= progressTotal-9 || e.Step == "done") {
			t.Fatalf("event %d at %d before the end: %+v", k, e.Done, e)
		}
		if strings.Contains(e.Message, "已就绪") {
			t.Fatalf("event %d says ready while creating: %+v", k, e)
		}
		last = e.Done
	}
	if end := got[len(got)-1]; end.Done != progressTotal || end.Step != "done" || end.Percent() != 100 {
		t.Fatalf("end %+v", end)
	}
}

// doneAt is the Done of the first event of step.
func doneAt(t *testing.T, got []Progress, step string) int {
	t.Helper()
	for _, e := range got {
		if e.Step == step {
			return e.Done
		}
	}
	t.Fatalf("no %s in %+v", step, got)
	return 0
}

// TestCreateProgress: the bar counts the steps done, weighted by what they take; the boot wait does not move it by
// the clock.
func TestCreateProgress(t *testing.T) {
	got := runCreateProgress(createPlan(stageBoot, true, false), 0, false, nil)
	checkBar(t, got)
	// the shares follow the stage weights: the boot is about half of a create without the bridge
	total := stagePrepare + stageBoot + stageTune + stageIme + stageFirstBoot + stageVulkanFix + stageShutdown
	for _, c := range []struct {
		step string
		want float64 // seconds done when it starts
	}{
		{"launch", stagePrepare},
		{"tune", stagePrepare + stageBoot},
		{"vulkan_fix", stagePrepare + stageBoot + stageTune + stageIme + stageFirstBoot},
		{"create_stop", total - stageShutdown},
	} {
		if d, want := doneAt(t, got, c.step), int(float64(progressTotal)*c.want/total); d != want {
			t.Errorf("%s starts at %d, want %d", c.step, d, want)
		}
	}
	// the boot wait events (Start counts them by the clock) leave the bar where the launch put it
	for _, e := range got {
		if e.Step == "boot" && e.Done != doneAt(t, got, "launch") {
			t.Errorf("the clock moved the bar: %+v", e)
		}
	}
	if d := doneAt(t, got, "setup"); d != doneAt(t, got, "create_stop") {
		t.Errorf("the end of the boot (%d) is not where the shutdown starts (%d)", d, doneAt(t, got, "create_stop"))
	}
}

// TestCreateProgressBridge: the bridge rounds of a new instance (two with a cold boot, then the check) are part of
// the bar; an unplanned round leaves it where it is; planned ones that do not happen are passed over.
func TestCreateProgressBridge(t *testing.T) {
	planned := runCreateProgress(createPlan(stageBoot, true, true), 2, true, nil)
	checkBar(t, planned)
	if d := doneAt(t, planned, "bridge"); d < 250 || d > 450 {
		t.Errorf("the bridge starts at %d", d)
	}
	// each cold boot moves the bar like the first boot
	var cold, rounds []int
	for _, e := range planned[slices.IndexFunc(planned, func(e Progress) bool { return e.Step == "bridge" }):] {
		switch e.Step {
		case "launch", "tune", "vulkan_fix":
			cold = append(cold, e.Done)
		case "bridge":
			rounds = append(rounds, e.Done)
		}
	}
	increasing := len(cold) == 6
	for k := 1; k < len(cold); k++ {
		increasing = increasing && cold[k] > cold[k-1]
	}
	if !increasing {
		t.Errorf("the cold boots of the bridge rounds: %v", cold)
	}
	// the check is a stage of its own, just before the shutdown
	if stop := doneAt(t, planned, "create_stop"); len(rounds) != 3 || rounds[2] >= stop || stop-rounds[2] > 10 {
		t.Errorf("bridge rounds start at %v, the shutdown at %d", rounds, doneAt(t, planned, "create_stop"))
	}
	checkBar(t, runCreateProgress(createPlan(stageBoot, true, false), 2, false, nil)) // two rounds, none planned
	checkBar(t, runCreateProgress(createPlan(stageBoot, true, true), 3, true, nil))   // one more than planned
	checkBar(t, runCreateProgress(createPlan(stageBoot, true, true), 1, true, nil))   // the first remount was done before
	skipped := runCreateProgress(createPlan(stageBoot, true, true), 0, false, nil)
	checkBar(t, skipped)
	if d := doneAt(t, skipped, "create_stop"); d < 900 {
		t.Errorf("the shutdown after bridge rounds that never came starts at %d", d)
	}
	// a resume of an instance that is initialized plans no first-boot setup
	if plan := createPlan(stageBoot, false, false); slices.ContainsFunc(plan, func(s createStage) bool { return s.step == "first_boot" }) {
		t.Errorf("plan %+v", plan)
	}
}

// TestCreatePlanOfSetup: the setup plans the boot of a new instance as its first boot, the boot of a resume as a
// later one, and the first-boot setup only while the instance is not initialized. A setup that fails leaves the bar
// where it got to for its shutdown.
func TestCreatePlanOfSetup(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	var h createHooks
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		for _, ev := range oneBoot(true) {
			opt.Progress(ev)
		}
		return &StartResult{HostIME: true}, nil
	}
	h.check = errs.New(errs.NotReady, "没做完：宿主输入法")
	h.install(t)
	// where the boot ends (the tuning starts), per mille
	tuneAt := func(req CreateRequest) int {
		var sent []Progress
		req.Progress = func(p Progress) { sent = append(sent, p) }
		if _, err := m.Create(ctx, req); errs.CodeOf(err) != errs.NotReady {
			opsSkipIfNotImplemented(t, err)
			t.Fatalf("create %+v: %v", req, err)
		}
		if stop := doneAt(t, sent, "create_stop"); stop != doneAt(t, sent, "setup") {
			t.Errorf("the shutdown of a failed setup moved the bar to %d", stop)
		}
		return doneAt(t, sent, "tune")
	}
	want := func(boot, firstBoot float64) int {
		total := stagePrepare + boot + stageTune + stageIme + firstBoot + stageVulkanFix + stageShutdown
		return int(float64(progressTotal) * (stagePrepare + boot) / total)
	}
	i := 6
	if d, w := tuneAt(CreateRequest{Index: &i, NoBridge: true}), want(stageBoot, stageFirstBoot); d != w {
		t.Errorf("create: the boot ends at %d, want %d", d, w)
	}
	if err := m.Root.EditInstanceMeta(6, func(meta *jsonord.Obj) error {
		meta.Set("initialized", jsonord.NewBool(true))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d, w := tuneAt(CreateRequest{Index: &i, Resume: true}), want(stageColdBoot, 0); d != w {
		t.Errorf("resume of an initialized instance: the boot ends at %d, want %d", d, w)
	}
	// a boot that fails in its wait (canceled from the manager: a normal shutdown)
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		opt.Progress(Progress{Step: "launch", Done: progressLaunch})
		return nil, errs.New(errs.LaunchFailed, "emulator.exe 退出码 1")
	}
	var sent []Progress
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true, Progress: func(p Progress) { sent = append(sent, p) }}); errs.CodeOf(err) != errs.LaunchFailed {
		t.Fatalf("resume with a failing boot: %v", err)
	}
	if stop, launch := doneAt(t, sent, "create_stop"), doneAt(t, sent, "launch"); stop != launch {
		t.Errorf("the shutdown after a boot that failed moved the bar from %d to %d", launch, stop)
	}
}

// TestCreateProgressBootLog: within the boot wait the emulator's log moves the bar, milestone by milestone, and
// only there.
func TestCreateProgressBootLog(t *testing.T) {
	log := filepath.Join(testutil.TempDir(t), "mas_6.log")
	write := func(s string) {
		if err := os.WriteFile(log, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("INFO | Android emulator version 37.2.10.0\n")
	var got []Progress
	p := &createProgress{i: 6, plan: createPlan(stageBoot, true, false), bootLog: func() string { return log },
		fn: func(e Progress) { got = append(got, e) }}
	p.stage("create", "开始")
	p.boot(Progress{Step: "launch", Done: progressLaunch})
	launched := got[len(got)-1].Done
	wait := func() int {
		p.boot(Progress{Step: "boot", Done: 150})
		return got[len(got)-1].Done
	}
	if d := wait(); d != launched {
		t.Fatalf("no milestone yet, the bar moved to %d", d)
	}
	write("Windows Hypervisor Platform accelerator is operational\nINFO | Advertising in: x\n")
	mid := wait()
	if mid <= launched {
		t.Fatalf("console up: %d", mid)
	}
	write("x is operational\nAdvertising in: x\nqemu-system-x86_64-headless.exe: info: qemu_win32_discard_memory: using DiscardVirtualMemory\n")
	guest := wait()
	if guest <= mid {
		t.Fatalf("guest kernel up: %d (console %d)", guest, mid)
	}
	write("x is operational\nAdvertising in: x\nqemu_win32_discard_memory\n" +
		"INFO | ColorBuffer 4 (1280x720 VK_FORMAT_R8G8B8A8_UNORM): driver prefers a dedicated allocation, using one\n" +
		"INFO | Boot completed in 19637 ms\n")
	late := wait()
	total := stagePrepare + stageBoot + stageTune + stageIme + stageFirstBoot + stageVulkanFix + stageShutdown
	booted := int(float64(progressTotal) * (stagePrepare + stageBoot) / total)
	if late <= guest || late >= booted {
		t.Fatalf("boot completed in the log: %d (guest %d, booted %d)", late, guest, booted)
	}
	// a log that cannot be read for a moment takes nothing back
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if d := wait(); d != late {
		t.Fatalf("unreadable log: %d, was %d", d, late)
	}
	write("x is operational\nAdvertising in: x\nINFO | Initialized handle: videmulator20060\nINFO | Boot completed in 19637 ms\n")
	// the log is only read in the boot wait: the tuning starts its own stage
	p.boot(Progress{Step: "tune", Done: progressTune})
	p.boot(Progress{Step: "boot", Done: 150})
	if d := got[len(got)-1].Done; d != booted {
		t.Fatalf("after the boot: %d, want %d", d, booted)
	}
	if bootLogShare(filepath.Join(testutil.TempDir(t), "missing.log")) != 0 || bootLogShare("") != 0 {
		t.Fatal("a missing log has milestones")
	}
}

// TestCreateProgressOnDisk: while the setup runs, status / list of another process (another Manager on the root)
// read its percentage; afterwards the file is gone and the field is 0.
func TestCreateProgressOnDisk(t *testing.T) {
	m, _ := opsTestManager(t)
	other := New(m.Root, nil)
	var h createHooks
	var during *InstanceInfo
	var sent []Progress
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		for _, ev := range oneBoot(true) {
			opt.Progress(ev)
		}
		during, _ = other.Info(ctx, i)
		return &StartResult{HostIME: true}, nil
	}
	h.install(t)
	i := 6
	info, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true,
		Progress: func(p Progress) { sent = append(sent, p) }})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	checkBar(t, sent)
	if during == nil || during.Status != StatusCreating || during.CreationProgress < 50 || during.CreationProgress > 95 {
		t.Fatalf("seen from another process during the setup: %+v", during)
	}
	if info.CreationProgress != 0 || fileOrDirExists(m.createProgressPath(6)) {
		t.Fatalf("after: %d, file left: %v", info.CreationProgress, fileOrDirExists(m.createProgressPath(6)))
	}
	// a failed setup removes it too
	h.check = errs.New(errs.NotReady, "没做完：宿主输入法")
	j := 7
	if _, err := m.Create(context.Background(), CreateRequest{Index: &j, NoBridge: true}); err == nil {
		t.Fatal("create did not fail")
	}
	if fileOrDirExists(m.createProgressPath(7)) {
		t.Fatal("progress file left after a failure")
	}
	if got, _ := m.Info(context.Background(), 7); got == nil || got.Status != StatusCreateFailed || got.CreationProgress != 0 {
		t.Fatalf("failed: %+v", got)
	}
}
