//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/win"
	"modavd/internal/win/wintest"
)

// fakeOverlayWindows is overlayWindows on made-up handles: who owns each, whether each takes input.
type fakeOverlayWindows struct {
	mu      sync.Mutex
	owner   map[uintptr]uint32
	enabled map[uintptr]bool
	toolbar map[uintptr]bool
	calls   int
}

func newFakeOverlayWindows(owner uint32, handles ...uintptr) *fakeOverlayWindows {
	f := &fakeOverlayWindows{owner: map[uintptr]uint32{}, enabled: map[uintptr]bool{}, toolbar: map[uintptr]bool{}}
	for _, h := range handles {
		f.owner[h], f.enabled[h] = owner, false // as an overlay left them
	}
	return f
}

func (f *fakeOverlayWindows) Windows(pid uint32) ([]uintptr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uintptr
	for h, p := range f.owner {
		if p == pid {
			out = append(out, h)
		}
	}
	return out, nil
}
func (f *fakeOverlayWindows) IsToolbar(h uintptr) bool { return f.toolbar[h] }
func (f *fakeOverlayWindows) SetEnabled(h uintptr, on bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if _, ok := f.owner[h]; !ok {
		return false
	}
	f.enabled[h] = on
	return true
}
func (f *fakeOverlayWindows) Enabled(h uintptr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[h]
}
func (f *fakeOverlayWindows) ProcessOf(h uintptr) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner[h]
}

func withOverlayOS(t *testing.T, ops overlayWindows) {
	t.Helper()
	saved := overlayOS
	overlayOS = ops
	t.Cleanup(func() { overlayOS = saved })
}

// deadProcess is a process that has exited; sleeper one that runs until the test ends.
func deadProcess(t *testing.T) win.Process {
	t.Helper()
	p, kill := startSleeper(t)
	kill()
	return p
}

// ---- the decisions ------------------------------------------------------------------------------------------------

func TestOverlayMachineLifecycle(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	om := newOverlayMachine(t0)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	in := func(s float64) overlayIn {
		return overlayIn{Now: at(s), QemuFound: true, StartHeld: true, WatchAlive: true}
	}
	step := func(i overlayIn) overlayOut { return om.Next(i) }

	// no qemu yet, then the qemu, adb answering, Android booted with the setup running, ready
	o := step(overlayIn{Now: at(0.2), StartHeld: true, WatchAlive: true})
	if o.Exit || o.Release || o.Stage != "launch" || o.Percent <= 0 {
		t.Fatalf("waiting for the qemu: %+v", o)
	}
	o = step(in(1))
	if o.Exit || o.Stage != "launch" || o.Percent < 0.05 {
		t.Fatalf("qemu up: %+v", o)
	}
	var last = o.Percent
	check := func(what string, o overlayOut) {
		t.Helper()
		if o.Exit || o.Release {
			t.Fatalf("%s: %+v", what, o)
		}
		if o.Percent < last {
			t.Fatalf("%s: the bar went back %.3f -> %.3f", what, last, o.Percent)
		}
		last = o.Percent
	}
	a := in(8)
	a.Answered = true
	check("adb answers", step(a))
	a = in(14)
	a.Answered = true
	o = step(a)
	check("Android booting", o)
	if o.Stage != "android" || o.Percent < overlayAndroidFrom || o.Percent > overlayAndroidTo {
		t.Fatalf("android stage: %+v", o)
	}
	for k, s := range []bootSetupState{{Step: readyStepBoot}, {Step: "vulkan_fix", Progress: 5}, {Step: "tune", Progress: 40},
		{Step: "ime", Progress: 70}, {Step: readyStepSDCard, Progress: 95}} {
		b := in(26 + float64(k))
		b.Answered, b.Booted, b.Setup = true, true, s
		o = step(b)
		check("setup "+s.Step, o)
		if o.Stage != s.Step || o.Percent < overlaySetupFrom || o.Percent >= 1 {
			t.Fatalf("setup stage %s: %+v", s.Step, o)
		}
	}
	b := in(32)
	b.Answered, b.Booted, b.Setup = true, true, bootSetupState{Ready: true}
	if o = step(b); !o.Exit || o.Reason != "ready" || !o.Release {
		t.Fatalf("ready: %+v (the windows come back at once, no wait)", o)
	}
}

func TestOverlayMachineEndsWhenThereIsNothingToCover(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	base := func(s float64) overlayIn {
		return overlayIn{Now: at(s), QemuFound: true, StartHeld: true, WatchAlive: true}
	}
	for _, c := range []struct {
		name   string
		run    func(om *overlayMachine) (overlayOut, overlayOut) // the look before the end, and the one that ends it
		reason string
	}{
		{"the starting process is gone: at once", func(om *overlayMachine) (overlayOut, overlayOut) {
			x := base(5)
			x.WatchAlive = false
			return om.Next(base(4)), om.Next(x)
		}, "watch-gone"},
		{"no start holds the lock for 1.5 s", func(om *overlayMachine) (overlayOut, overlayOut) {
			x, y := base(5), base(6.4)
			x.StartHeld, y.StartHeld = false, false
			om.Next(base(4))
			om.Next(x)
			// the lock comes back for a moment: the count starts over
			back := base(5.5)
			back.StartHeld = true
			om.Next(back)
			z := base(5.9)
			z.StartHeld = false
			om.Next(z)
			return om.Next(y), om.Next(func() overlayIn { w := base(7.5); w.StartHeld = false; return w }())
		}, "start-gone"},
		{"the qemu is gone for 2 s", func(om *overlayMachine) (overlayOut, overlayOut) {
			om.Next(base(4))
			x, y := base(5), base(6.5)
			x.QemuFound, y.QemuFound = false, false
			om.Next(x)
			return om.Next(y), om.Next(func() overlayIn { w := base(7.1); w.QemuFound = false; return w }())
		}, "qemu-gone"},
		{"the qemu never comes", func(om *overlayMachine) (overlayOut, overlayOut) {
			x, y := base(29), base(30.5)
			x.QemuFound, y.QemuFound = false, false
			return om.Next(x), om.Next(y)
		}, "qemu-never"},
	} {
		om := newOverlayMachine(t0)
		before, end := c.run(om)
		if before.Exit {
			t.Errorf("%s: ended too early: %+v", c.name, before)
		}
		if !end.Exit || end.Reason != c.reason {
			t.Errorf("%s: %+v, want exit %s", c.name, end, c.reason)
		}
	}
	// a qemu that was missed once is not the end
	om := newOverlayMachine(t0)
	om.Next(base(1))
	miss := base(2)
	miss.QemuFound = false
	om.Next(miss)
	if o := om.Next(base(2.5)); o.Exit {
		t.Errorf("one missed scan ended the overlay: %+v", o)
	}
	if o := om.Next(func() overlayIn { w := base(5); w.QemuFound = false; return w }()); o.Exit {
		t.Errorf("the miss count did not start over: %+v", o)
	}
}

// The cap: the windows come back at once, the cover stays with a line naming the step, and goes after overlayCapNote.
func TestOverlayMachineCap(t *testing.T) {
	saved := overlayCapNote
	overlayCapNote = 3 * time.Second
	t.Cleanup(func() { overlayCapNote = saved })
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	om := newOverlayMachine(t0)
	in := func(s float64) overlayIn {
		return overlayIn{Now: t0.Add(time.Duration(s * float64(time.Second))), QemuFound: true, StartHeld: true, WatchAlive: true,
			Answered: true, Booted: true, Setup: bootSetupState{Ready: true, Unfinished: "ime"}}
	}
	o := om.Next(in(70))
	if o.Exit || !o.Release || o.Note != "ime" || o.Percent != 1 {
		t.Fatalf("at the cap: %+v", o)
	}
	later := in(71)
	later.Setup = bootSetupState{Ready: true} // the setup finished meanwhile: the note stays its time all the same
	if o = om.Next(later); o.Exit || !o.Release || o.Note != "ime" {
		t.Fatalf("during the note: %+v", o)
	}
	if o = om.Next(in(73.1)); !o.Exit || o.Reason != "cap" || !o.Release {
		t.Fatalf("after the note: %+v", o)
	}
}

func TestOverlayTextsAreTranslatedAndNameTheStep(t *testing.T) {
	seen := map[string]bool{}
	for _, stage := range []string{"launch", "android", "vulkan_fix", "tune", "ime", "first_boot", readyStepSDCard, readyStepBoot} {
		s := overlayTitle(stage)
		if s == "" || seen[s] {
			t.Errorf("stage %s: title %q (empty or the same as another)", stage, s)
		}
		seen[s] = true
	}
	if n := overlayNote("ime"); !strings.Contains(n, "宿主输入法") {
		t.Errorf("note %q does not name the step", n)
	}
}

// ---- giving the windows back ---------------------------------------------------------------------------------------

// A mark of an overlay that is gone gives the windows back and is deleted; a handle that another process's window has
// now is left alone; nothing happens while the overlay lives, without a mark, or when the qemu is gone too.
func TestRecoverOverlay(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	other, _ := startSleeper(t)
	dead := deadProcess(t)
	live, _ := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 101, 102)
	ops.owner[103] = other.PID // a handle that was reused by somebody else's window
	ops.enabled[103] = false
	withOverlayOS(t, ops)

	mark := func(overlay, q win.Process, hs ...uint64) overlayMark {
		return overlayMark{OverlayPID: overlay.PID, OverlayCreated: overlay.Created, QemuPID: q.PID, QemuCreated: q.Created, Windows: hs}
	}
	if m.RecoverOverlay(i) {
		t.Error("recovered with no mark")
	}
	// the overlay lives: left alone
	if err := m.writeOverlayMark(i, mark(live, qemu, 101, 102)); err != nil {
		t.Fatal(err)
	}
	if m.RecoverOverlay(i) || ops.Enabled(101) {
		t.Error("windows were enabled under a live overlay")
	}
	if _, ok := m.readOverlayMark(i); !ok {
		t.Error("the mark of a live overlay was deleted")
	}
	// the overlay is gone
	if err := m.writeOverlayMark(i, mark(dead, qemu, 101, 102, 103)); err != nil {
		t.Fatal(err)
	}
	if !m.RecoverOverlay(i) {
		t.Fatal("nothing recovered")
	}
	if !ops.Enabled(101) || !ops.Enabled(102) {
		t.Error("the windows of the qemu were not enabled")
	}
	if ops.Enabled(103) {
		t.Error("a window of another process was enabled")
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
	// the qemu is gone too: the mark goes, nothing is touched
	calls := ops.calls
	if err := m.writeOverlayMark(i, mark(dead, deadProcess(t), 101)); err != nil {
		t.Fatal(err)
	}
	if m.RecoverOverlay(i) || ops.calls != calls {
		t.Error("windows of a qemu that is gone were touched")
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark of a gone qemu stays")
	}
	// a torn mark is none
	if err := os.WriteFile(m.overlayMarkPath(i), []byte(`{"overlay_pid":12`), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.RecoverOverlay(i) {
		t.Error("recovered from a torn mark")
	}
}

// Layer 2: the start's guard sees the overlay's process go, however it went, and gives the windows back within about
// a second. It does nothing while the process lives, and ends with the start.
func TestGuardGivesTheWindowsBackWhenTheOverlayIsKilled(t *testing.T) {
	m, i := opsTestInstance(t)
	qemu, _ := startSleeper(t)
	overlayProc, kill := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 201, 202)
	withOverlayOS(t, ops)
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int) (host.Process, error) { return overlayProc, nil }
	if err := m.writeOverlayMark(i, overlayMark{OverlayPID: overlayProc.PID, OverlayCreated: overlayProc.Created, QemuPID: qemu.PID,
		QemuCreated: qemu.Created, Windows: []uint64{201, 202}}); err != nil {
		t.Fatal(err)
	}

	h := m.startBootOverlay(i)
	if h == nil {
		t.Fatal("no handle")
	}
	time.Sleep(3 * overlayGuardEvery)
	if ops.Enabled(201) {
		t.Fatal("the guard enabled windows under a live overlay")
	}
	killed := time.Now()
	kill()
	deadline := time.Now().Add(5 * time.Second)
	for !(ops.Enabled(201) && ops.Enabled(202)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !ops.Enabled(201) || !ops.Enabled(202) {
		t.Fatal("the windows were not given back")
	}
	if d := time.Since(killed); d > 2*time.Second {
		t.Errorf("the windows came back %v after the overlay was killed", d)
	} else {
		t.Logf("windows back %v after the overlay process ended", d.Round(10*time.Millisecond))
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
	h.release()
	h.release()

	// a start that ends before its overlay does leaves the overlay alone
	live, _ := startSleeper(t)
	spawnBootOverlay = func(*Manager, int) (host.Process, error) { return live, nil }
	ops.enabled[201] = false
	if err := m.writeOverlayMark(i, overlayMark{OverlayPID: live.PID, OverlayCreated: live.Created, QemuPID: qemu.PID,
		QemuCreated: qemu.Created, Windows: []uint64{201}}); err != nil {
		t.Fatal(err)
	}
	m.startBootOverlay(i).release()
	time.Sleep(2 * overlayGuardEvery)
	if ops.Enabled(201) {
		t.Error("a released guard touched the windows of a live overlay")
	}
}

// An overlay that cannot start is a warning and a boot without the cover.
func TestStartBootOverlayFailureIsOnlyAWarning(t *testing.T) {
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	withOverlayOS(t, newFakeOverlayWindows(1))
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int) (host.Process, error) { return host.Process{}, errors.New("boom") }
	if h := m.startBootOverlay(3); h != nil {
		t.Error("a handle for an overlay that did not start")
	}
	var warned bool
	for _, msg := range logs.Messages() {
		warned = warned || strings.Contains(msg, "开机画面没起来")
	}
	if !warned {
		t.Errorf("no warning in %q", logs.Messages())
	}
	withOverlayOS(t, nil)
	if h := m.startBootOverlay(3); h != nil {
		t.Error("an overlay without window operations")
	}
}

// ---- Start starts it ----------------------------------------------------------------------------------------------

// Start starts the boot screen for a boot that has a window and is set up here (launched, or owed), once; never for a
// headless one, and never for a take-over that owes nothing.
func TestStartStartsTheOverlayOnlyForAWindowedBoot(t *testing.T) {
	setCap(t, time.Hour)
	withOverlayOS(t, newFakeOverlayWindows(1))
	for _, c := range []struct {
		name     string
		windowed bool
		owed     bool
		want     int32
	}{
		{"owed, windowed", true, true, 1},
		{"owed, headless", false, true, 0},
		{"plain take-over, windowed", true, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			sleeper, _ := startSleeper(t) // before the fake adb is in the environment: it would run as that
			s := newStartRun(t)
			var spawned atomic.Int32
			saved := spawnBootOverlay
			t.Cleanup(func() { spawnBootOverlay = saved })
			spawnBootOverlay = func(*Manager, int) (host.Process, error) { spawned.Add(1); return sleeper, nil }
			if c.windowed {
				q := s.q
				q.Name = "qemu-system-x86_64.exe" // the windowed build
				startFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
			}
			if c.owed {
				s.owedMarker()
			} else {
				s.m.writeBootRecord(s.i, bootRecord{QemuPID: s.q.PID, QemuCreated: s.q.Created, BootID: testBootA})
			}
			if _, err := s.start(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if n := spawned.Load(); n != c.want {
				t.Errorf("overlay started %d times, want %d", n, c.want)
			}
		})
	}
}

// ---- the overlay process itself, on windows of a stand-in emulator ------------------------------------------------

type overlayFixture struct {
	t     *testing.T
	m     *Manager
	i     int
	emu   *wintest.Emulator
	q     host.Process
	run   *overlayRun
	boot  atomic.Pointer[[2]bool] // booted, answered
	code  chan int
	ended atomic.Bool
}

func newOverlayFixture(t *testing.T) *overlayFixture {
	t.Helper()
	setCap(t, time.Minute)
	m, i := opsTestInstance(t)
	e := wintest.Start(t)
	q, ok := host.FindProcess(e.PID)
	if !ok {
		t.Fatal("stand-in emulator not found")
	}
	f := &overlayFixture{t: t, m: m, i: i, emu: e, q: q, code: make(chan int, 1)}
	f.boot.Store(&[2]bool{false, false})
	me, _ := host.FindProcess(uint32(os.Getpid()))
	f.run = newOverlayRun(m, i, logx.Nop(), me)
	f.run.find = func() (host.Process, bool, error) { return q, true, nil }
	f.run.probe = func(context.Context) (bool, bool) { b := f.boot.Load(); return b[0], b[1] }
	return f
}

func (f *overlayFixture) start() {
	go func() { f.code <- f.run.run(); f.ended.Store(true) }()
}

func (f *overlayFixture) waitFor(what string, d time.Duration, ok func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *overlayFixture) windowsEnabled() (bool, bool) {
	return win.IsWindowEnabled(f.emu.Main), win.IsWindowEnabled(f.emu.Tool)
}

func (f *overlayFixture) covered() bool {
	g := win.GeometryOf(f.emu.Main)
	x, y := (g.Client.Left+g.Client.Right)/2, (g.Client.Top+g.Client.Bottom)/2
	top := win.WindowFromPoint(x, y)
	return top != 0 && top != f.emu.Main
}

func (f *overlayFixture) wait(what string, d time.Duration) int {
	f.t.Helper()
	select {
	case c := <-f.code:
		return c
	case <-time.After(d):
		f.t.Fatalf("the overlay did not end: %s", what)
		return -1
	}
}

// The whole run: the cover goes up over the picture area and the toolbar and the windows take no input (mark on disk);
// it stays while Android boots and the setup runs; the moment the setup is done the windows are back, the mark is gone
// and the overlay ends. All read with queries.
func TestOverlayRunCoversUntilTheInstanceIsOnline(t *testing.T) {
	f := newOverlayFixture(t)
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	if !f.covered() {
		t.Error("the middle of the picture area is not covered")
	}
	// the cover is over the picture area (the client area), not the title bar around it
	g := win.GeometryOf(f.emu.Main)
	cx, cy := (g.Client.Left+g.Client.Right)/2, (g.Client.Top+g.Client.Bottom)/2
	if cover := win.WindowFromPoint(cx, cy); win.GeometryOf(cover).Frame != g.Client {
		t.Errorf("cover at %+v, want the client area %+v", win.GeometryOf(cover).Frame, g.Client)
	}
	if top := win.WindowFromPoint(cx, g.Frame.Top+4); top != f.emu.Main {
		t.Errorf("the title bar is covered (top window %#x)", top)
	}
	k, ok := f.m.readOverlayMark(f.i)
	if !ok || len(k.Windows) != 2 || k.QemuPID != f.q.PID || k.OverlayPID != uint32(os.Getpid()) {
		t.Errorf("mark %+v %v", k, ok)
	}
	tg := win.GeometryOf(f.emu.Tool)
	if top := win.WindowFromPoint((tg.Frame.Left+tg.Frame.Right)/2, (tg.Frame.Top+tg.Frame.Bottom)/2); top == f.emu.Tool || top == 0 {
		t.Error("the toolbar is not covered")
	}

	// Android booted, the setup is running: still covered
	f.boot.Store(&[2]bool{true, true})
	putRecord(t, f.m, f.i, f.q, nil)
	time.Sleep(900 * time.Millisecond)
	if a, b := f.windowsEnabled(); a || b || !f.covered() || f.ended.Load() {
		t.Fatalf("while the setup runs: enabled %v %v covered %v ended %v", a, b, f.covered(), f.ended.Load())
	}

	// done: the windows are back at once and the overlay ends
	done := time.Now()
	putRecord(t, f.m, f.i, f.q, func(r *readyRecord) { r.Done = true })
	if c := f.wait("setup done", 5*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("the windows were not given back: %v %v", a, b)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
	if f.covered() {
		t.Error("still covered")
	}
	t.Logf("windows back and cover gone %v after the setup was recorded as done", time.Since(done).Round(10*time.Millisecond))
}

// The start is cancelled or killed (nobody holds the start lock any more): the windows are back within 1.5 s plus a
// look, whatever the instance is doing.
func TestOverlayRunEndsWhenTheStartIsGone(t *testing.T) {
	f := newOverlayFixture(t)
	unlock := lockStart(t, f.m, f.i)
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	gone := time.Now()
	unlock()
	if c := f.wait("start gone", 8*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows not back: %v %v", a, b)
	}
	if d := time.Since(gone); d > 3*time.Second {
		t.Errorf("back after %v", d)
	} else {
		t.Logf("windows back %v after the start lock was released", d.Round(10*time.Millisecond))
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
}

// The process that started it is gone.
func TestOverlayRunEndsWhenItsStarterIsGone(t *testing.T) {
	f := newOverlayFixture(t)
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	starter, kill := startSleeper(t)
	f.run.watch = starter
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	gone := time.Now()
	kill()
	if c := f.wait("starter gone", 8*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows not back: %v %v", a, b)
	}
	t.Logf("windows back %v after the starting process was killed", time.Since(gone).Round(10*time.Millisecond))
}

// The cap: the windows come back as soon as the setup is past its deadline, the cover goes on for the note, then ends.
func TestOverlayRunCapPutsTheWindowsBackAndKeepsANote(t *testing.T) {
	f := newOverlayFixture(t)
	saved := overlayCapNote
	overlayCapNote = 800 * time.Millisecond
	t.Cleanup(func() { overlayCapNote = saved })
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	f.boot.Store(&[2]bool{true, true})
	putRecord(t, f.m, f.i, f.q, nil)
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	at := time.Now()
	putRecord(t, f.m, f.i, f.q, func(r *readyRecord) { r.DeadlineMS = readyClockMS() - 1; r.Step = "ime" })
	f.waitFor("the windows back at the cap", 5*time.Second, func() bool { a, b := f.windowsEnabled(); return a && b })
	if f.ended.Load() {
		t.Error("the cover went with the windows: no time to read the note")
	}
	if !f.covered() {
		t.Error("the note is not on the cover")
	}
	t.Logf("windows back %v after the cap", time.Since(at).Round(10*time.Millisecond))
	if c := f.wait("cap note", 5*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if f.covered() {
		t.Error("still covered after the note")
	}
}

// Layer 4 on real windows: windows an overlay disabled and left (it and its starter are gone) are enabled by the next
// look at the instance.
func TestRecoverOverlayOnRealWindows(t *testing.T) {
	f := newOverlayFixture(t)
	dead := deadProcess(t)
	for _, h := range []uintptr{f.emu.Main, f.emu.Tool} {
		if !win.SetWindowEnabled(h, false) {
			t.Fatal("cannot disable")
		}
	}
	if err := f.m.writeOverlayMark(f.i, overlayMark{OverlayPID: dead.PID, OverlayCreated: dead.Created, QemuPID: f.q.PID,
		QemuCreated: f.q.Created, Windows: []uint64{uint64(f.emu.Main), uint64(f.emu.Tool)}}); err != nil {
		t.Fatal(err)
	}
	// what status, list and info do for a running instance
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{f.i: f.q} }
	statusBooted = func(*Manager, context.Context, int) (bool, bool) { return true, true }
	start := time.Now()
	if _, err := f.m.Info(context.Background(), f.i); err != nil {
		t.Fatal(err)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows not recovered by a status look: %v %v", a, b)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
	t.Logf("recovered by one Info call in %v", time.Since(start).Round(time.Millisecond))
}

func TestBootOverlayArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"x"}, {"root", "99", "1", "2"}, {"root", "3", "0", "2"}, {"root", "x", "1", "2"}} {
		if code := RunBootOverlay(args, discard{}); code != 2 {
			t.Errorf("args %q: exit code %d, want 2", slices.Clone(args), code)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
