//go:build windows

package emu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/win"
	"modavd/internal/win/wintest"
)

const fakeClass = "Qt653QWindowIcon"

// fakeOverlayWindows is overlayWindows on made-up handles: who owns each, its class, whether each takes input; and
// the failures a real window can have: a call that does not return (hang), one that does not take effect (fail).
type fakeOverlayWindows struct {
	mu       sync.Mutex
	owner    map[uintptr]uint32
	class    map[uintptr]string
	enabled  map[uintptr]bool
	toolbar  map[uintptr]bool
	hang     map[uintptr]chan struct{} // SetEnabled on it waits until the channel is closed
	fail     map[uintptr]bool          // SetEnabled on it leaves it as it is and says no
	setCalls map[uintptr]int
	calls    int
	onSet    func(h uintptr, on bool) // called before the state changes (not under the lock)
}

func newFakeOverlayWindows(owner uint32, handles ...uintptr) *fakeOverlayWindows {
	f := &fakeOverlayWindows{owner: map[uintptr]uint32{}, class: map[uintptr]string{}, enabled: map[uintptr]bool{},
		toolbar: map[uintptr]bool{}, hang: map[uintptr]chan struct{}{}, fail: map[uintptr]bool{}, setCalls: map[uintptr]int{}}
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
	f.calls++
	f.setCalls[h]++
	_, exists := f.owner[h]
	ch, hang, fail, onSet := f.hang[h], f.hang[h] != nil, f.fail[h], f.onSet
	f.mu.Unlock()
	if !exists {
		return false
	}
	if hang {
		<-ch
	}
	if onSet != nil {
		onSet(h, on)
	}
	if fail {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled[h] = on
	return true
}
func (f *fakeOverlayWindows) Enabled(h uintptr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[h]
}
func (f *fakeOverlayWindows) Identity(h uintptr) (uint32, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.owner[h]
	if !ok {
		return 0, ""
	}
	if c := f.class[h]; c != "" {
		return p, c
	}
	return p, fakeClass
}
func (f *fakeOverlayWindows) setEnabled(h uintptr, on bool) {
	f.mu.Lock()
	f.enabled[h] = on
	f.mu.Unlock()
}
func (f *fakeOverlayWindows) setCallsOf(h uintptr) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCalls[h]
}
func (f *fakeOverlayWindows) reown(h uintptr, pid uint32) {
	f.mu.Lock()
	f.owner[h] = pid
	f.mu.Unlock()
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

func withVar[T any](t *testing.T, v *T, val T) {
	t.Helper()
	saved := *v
	*v = val
	t.Cleanup(func() { *v = saved })
}

// winsOf names windows as an overlay's mark does: of qemu q, with the class the fake gives.
func winsOf(q win.Process, hs ...uint64) []overlayWin {
	var out []overlayWin
	for _, h := range hs {
		out = append(out, overlayWin{H: h, QemuPID: q.PID, QemuCreated: q.Created, Class: fakeClass})
	}
	return out
}

func markOf(round string, overlay, q win.Process, ws []overlayWin) overlayMark {
	return overlayMark{Round: round, OverlayPID: overlay.PID, OverlayCreated: overlay.Created, QemuPID: q.PID, QemuCreated: q.Created, Windows: ws}
}

func putMark(t *testing.T, m *Manager, i int, k overlayMark) {
	t.Helper()
	if err := m.writeOverlayMark(i, k); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func markHas(m *Manager, i int, h uintptr) bool {
	k, ok := m.readOverlayMark(i)
	if !ok {
		return false
	}
	for _, w := range k.Windows {
		if w.H == uint64(h) {
			return true
		}
	}
	return false
}

// ---- giving the windows back ---------------------------------------------------------------------------------------

// A mark of an overlay that is gone gives the windows back and is deleted; a handle that another process's window or a
// window of another class has now is left alone; nothing happens while the overlay lives, without a mark, or when the
// qemu is gone too.
func TestRecoverOverlay(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	other, _ := startSleeper(t)
	dead := deadProcess(t)
	live, _ := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 101, 102, 104)
	ops.owner[103] = other.PID        // a handle that was reused by somebody else's window
	ops.class[104] = "SomeOtherClass" // a handle that is another kind of window of the same process now
	withOverlayOS(t, ops)

	if m.RecoverOverlay(i) {
		t.Error("recovered with no mark")
	}
	// the overlay lives: left alone
	putMark(t, m, i, markOf("r", live, qemu, winsOf(qemu, 101, 102)))
	if m.RecoverOverlay(i) || ops.Enabled(101) {
		t.Error("windows were enabled under a live overlay")
	}
	if _, ok := m.readOverlayMark(i); !ok {
		t.Error("the mark of a live overlay was deleted")
	}
	// the overlay is gone
	putMark(t, m, i, markOf("r", dead, qemu, winsOf(qemu, 101, 102, 103, 104)))
	if !m.RecoverOverlay(i) {
		t.Fatal("nothing recovered")
	}
	if !ops.Enabled(101) || !ops.Enabled(102) {
		t.Error("the windows of the qemu were not enabled")
	}
	if ops.Enabled(103) || ops.setCallsOf(103) != 0 {
		t.Error("a window of another process was touched")
	}
	if ops.Enabled(104) || ops.setCallsOf(104) != 0 {
		t.Error("a window of another class was touched")
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
	// the qemu is gone too: the mark goes, nothing is touched
	calls := ops.calls
	putMark(t, m, i, markOf("r", dead, deadProcess(t), winsOf(deadProcess(t), 101)))
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

// A window that still exists and does not take the enable stays in the mark for the next try; a window that is gone
// counts as done.
func TestRecoverOverlayKeepsWhatCouldNotBeEnabled(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(qemu.PID, 501, 502)
	ops.fail[501] = true
	withOverlayOS(t, ops)
	putMark(t, m, i, markOf("r", dead, qemu, winsOf(qemu, 501, 502, 503))) // 503 does not exist: gone
	if n, done := m.recoverOverlayMark(i, "", ops); n != 1 || done {
		t.Fatalf("recovered %d, done %v: want 1 and not done", n, done)
	}
	k, ok := m.readOverlayMark(i)
	if !ok || len(k.Windows) != 1 || k.Windows[0].H != 501 {
		t.Fatalf("the mark after the first try: %+v %v", k, ok)
	}
	ops.mu.Lock()
	ops.fail[501] = false
	ops.mu.Unlock()
	if n, done := m.recoverOverlayMark(i, "", ops); n != 1 || !done || !ops.Enabled(501) {
		t.Fatalf("second try: %d %v enabled %v", n, done, ops.Enabled(501))
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays after everything was given back")
	}
}

// A recovery only touches the mark of the round it is for, and a mark is changed under its lock: a recovery that cannot
// have the lock leaves everything as it is.
func TestRecoverOverlayIsPerRoundAndUnderTheLock(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(qemu.PID, 601)
	putMark(t, m, i, markOf("round-A", dead, qemu, winsOf(qemu, 601)))
	if n, done := m.recoverOverlayMark(i, "round-B", ops); n != 0 || !done || ops.Enabled(601) {
		t.Fatalf("a recovery for another round touched the mark: %d %v enabled %v", n, done, ops.Enabled(601))
	}
	if _, ok := m.readOverlayMark(i); !ok {
		t.Fatal("the mark of round A was deleted by a recovery of round B")
	}
	withVar(t, &overlayMarkLockWait, 100*time.Millisecond)
	l, ok := m.overlayMarkLock(i)
	if !ok {
		t.Fatal("cannot take the mark lock")
	}
	begin := time.Now()
	if n, done := m.recoverOverlayMark(i, "", ops); n != 0 || done || ops.Enabled(601) {
		t.Fatalf("recovered without the lock: %d %v", n, done)
	}
	if d := time.Since(begin); d < 90*time.Millisecond || d > time.Second {
		t.Errorf("waited %v for the lock", d)
	}
	l.Unlock()
	if n, done := m.recoverOverlayMark(i, "round-A", ops); n != 1 || !done || !ops.Enabled(601) {
		t.Fatalf("recovery of round A: %d %v", n, done)
	}
}

// A lifecycle command never waits for a window that does not answer: RecoverOverlay gives up after its budget, the
// mark stays, and the work goes on in the background and finishes when the window answers.
func TestRecoverOverlayDoesNotWaitForAHungWindow(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(qemu.PID, 701, 702)
	hang := make(chan struct{})
	ops.hang[701] = hang
	withOverlayOS(t, ops)
	withVar(t, &overlayRecoverBudget, 200*time.Millisecond)
	putMark(t, m, i, markOf("r", dead, qemu, winsOf(qemu, 701, 702)))
	begin := time.Now()
	if m.RecoverOverlay(i) {
		t.Error("reported a recovery that was not done in time")
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("RecoverOverlay waited %v for a window that does not answer", d)
	} else {
		t.Logf("RecoverOverlay returned after %v with a window that does not answer", d.Round(10*time.Millisecond))
	}
	eventually(t, "the window that answers is enabled", 2*time.Second, func() bool { return ops.Enabled(702) })
	close(hang) // the window answers at last
	eventually(t, "the mark is gone after the window answered", 5*time.Second, func() bool {
		_, ok := m.readOverlayMark(i)
		return !ok
	})
	if !ops.Enabled(701) {
		t.Error("the window that answered late was not enabled")
	}
}

// ---- the keeper of the windows ---------------------------------------------------------------------------------------

type keeperFixture struct {
	t    *testing.T
	m    *Manager
	i    int
	q    win.Process
	ops  *fakeOverlayWindows
	k    *windowKeeper
	logs *logx.Collector
}

func newKeeperFixture(t *testing.T, self *win.Process, handles ...uintptr) *keeperFixture {
	t.Helper()
	m, i := opsTestInstance(t)
	q, _ := startSleeper(t)
	logs := &logx.Collector{}
	ops := newFakeOverlayWindows(q.PID, handles...)
	for _, h := range handles {
		ops.enabled[h] = true
	}
	me, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("cannot read self")
	}
	if self != nil {
		me = *self
	}
	k := newWindowKeeper(m, i, ops, logx.New(logs.Sink()), "round-1", me, nil)
	t.Cleanup(func() { k.Close("test end", time.Second) })
	return &keeperFixture{t: t, m: m, i: i, q: q, ops: ops, k: k, logs: logs}
}

func handlesOf(hs ...uint64) []uintptr {
	var out []uintptr
	for _, h := range hs {
		out = append(out, uintptr(h))
	}
	return out
}

// The windows are named in the mark before they are disabled, with who they are; they are given back and the mark is
// deleted at the end.
func TestKeeperNamesTheWindowsBeforeDisablingThem(t *testing.T) {
	f := newKeeperFixture(t, nil, 301, 302)
	var mu sync.Mutex
	named := map[uintptr]bool{}
	f.ops.onSet = func(h uintptr, on bool) {
		if !on {
			mu.Lock()
			named[h] = markHas(f.m, f.i, h)
			mu.Unlock()
		}
	}
	f.k.Track(f.q, handlesOf(301, 302))
	f.k.Decide()
	eventually(t, "both windows disabled", 3*time.Second, func() bool { return !f.ops.Enabled(301) && !f.ops.Enabled(302) })
	mu.Lock()
	defer mu.Unlock()
	if !named[301] || !named[302] {
		t.Errorf("a window was disabled before the mark named it: %v", named)
	}
	k, ok := f.m.readOverlayMark(f.i)
	if !ok || k.Round != "round-1" || k.QemuPID != f.q.PID || len(k.Windows) != 2 {
		t.Fatalf("mark %+v %v", k, ok)
	}
	for _, w := range k.Windows {
		if w.QemuPID != f.q.PID || w.QemuCreated != f.q.Created || w.Class != fakeClass {
			t.Errorf("window entry %+v does not say who it is", w)
		}
	}
	f.k.Close("test", 2*time.Second)
	if !f.ops.Enabled(301) || !f.ops.Enabled(302) {
		t.Error("the windows were not given back")
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
}

// When the mark cannot be written nothing is disabled (the cover still keeps the mouse out): one warning, no calls to
// the windows; when it can be written again the windows are disabled.
func TestKeeperDisablesNothingWhenTheMarkCannotBeWritten(t *testing.T) {
	f := newKeeperFixture(t, nil, 311, 312)
	var fail atomic.Bool
	fail.Store(true)
	saved := overlayMarkWrite
	t.Cleanup(func() { overlayMarkWrite = saved })
	overlayMarkWrite = func(m *Manager, i int, k overlayMark) error {
		if fail.Load() {
			return errors.New("disk full")
		}
		return saved(m, i, k)
	}
	f.k.Track(f.q, handlesOf(311, 312))
	f.k.Decide()
	time.Sleep(1300 * time.Millisecond) // a few turns of the worker
	if f.ops.calls != 0 || !f.ops.Enabled(311) || !f.ops.Enabled(312) {
		t.Fatalf("windows were disabled without a mark: calls %d", f.ops.calls)
	}
	warned := 0
	for _, msg := range f.logs.Messages() {
		if strings.Contains(msg, "记不下遮罩禁用的窗口") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d warnings, want 1: %q", warned, f.logs.Messages())
	}
	fail.Store(false)
	eventually(t, "the windows disabled once the mark can be written", 3*time.Second, func() bool {
		return !f.ops.Enabled(311) && !f.ops.Enabled(312)
	})
}

// Nothing is disabled before the first look at the instance (the instance may be ready already), and nothing after an
// end was asked for, not even by a step that is under way.
func TestKeeperWaitsForTheFirstLookAndHonoursStop(t *testing.T) {
	f := newKeeperFixture(t, nil, 321, 322)
	f.k.Track(f.q, handlesOf(321, 322))
	time.Sleep(700 * time.Millisecond)
	if f.ops.calls != 0 {
		t.Fatal("a window was disabled before the first look")
	}
	// the end is asked for while the first disable does not answer: the second window is never disabled
	hang := make(chan struct{})
	f.ops.hang[321] = hang
	f.k.Decide()
	eventually(t, "the call into the first window", 3*time.Second, func() bool { return f.ops.setCallsOf(321) == 1 })
	f.k.Stop()
	close(hang)
	time.Sleep(700 * time.Millisecond)
	if f.ops.setCallsOf(322) != 0 || !f.ops.Enabled(322) {
		t.Error("a window was disabled after the end was asked for")
	}
	f.k.Close("test", 2*time.Second)
	if !f.ops.Enabled(321) || !f.ops.Enabled(322) {
		t.Error("the windows are not back")
	}
}

// A handle that is another process's window now (the window was destroyed and the number reused) is dropped: not
// disabled again, not enabled at the end.
func TestKeeperDropsAHandleThatIsAnotherWindowNow(t *testing.T) {
	f := newKeeperFixture(t, nil, 331, 332)
	other, _ := startSleeper(t)
	f.k.Track(f.q, handlesOf(331, 332))
	f.k.Decide()
	eventually(t, "both disabled", 3*time.Second, func() bool { return !f.ops.Enabled(331) && !f.ops.Enabled(332) })
	f.ops.reown(331, other.PID) // somebody else's window has that handle now, and it takes input
	f.ops.setEnabled(331, true)
	f.ops.setEnabled(332, true) // Qt enabled ours again: disabled again at once
	eventually(t, "the window that is ours is disabled again", 3*time.Second, func() bool { return !f.ops.Enabled(332) })
	calls := f.ops.setCallsOf(331)
	time.Sleep(600 * time.Millisecond)
	if f.ops.setCallsOf(331) != calls || !f.ops.Enabled(331) {
		t.Error("the other process's window was touched again")
	}
	f.k.Close("test", 2*time.Second)
	if f.ops.setCallsOf(331) != calls {
		t.Error("the other process's window was enabled at the end")
	}
	if !f.ops.Enabled(332) {
		t.Error("our window is not back")
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
}

// The calls into windows that do not answer are the keeper's, not the caller's: Release returns at once, Close is back
// within its limit, the mark keeps the window that did not answer, and the boot watch's recovery gives it back once it
// answers.
func TestKeeperDoesNotWaitForAWindowThatDoesNotAnswer(t *testing.T) {
	withVar(t, &overlayEnableDeadline, 300*time.Millisecond)
	dead := deadProcess(t) // as the overlay that owns the mark once it is gone
	f := newKeeperFixture(t, &dead, 341, 342)
	f.k.Track(f.q, handlesOf(341, 342))
	f.k.Decide()
	eventually(t, "both disabled", 3*time.Second, func() bool { return !f.ops.Enabled(341) && !f.ops.Enabled(342) })
	hang := make(chan struct{})
	f.ops.hang[342] = hang
	begin := time.Now()
	f.k.Release()
	if d := time.Since(begin); d > 50*time.Millisecond {
		t.Errorf("Release took %v", d)
	}
	eventually(t, "the window that answers is enabled", 3*time.Second, func() bool { return f.ops.Enabled(341) })
	begin = time.Now()
	f.k.Close("test", 900*time.Millisecond)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("Close took %v with a window that does not answer", d)
	}
	if !markHas(f.m, f.i, 342) || markHas(f.m, f.i, 341) {
		k, ok := f.m.readOverlayMark(f.i)
		t.Fatalf("the mark after the close: %+v %v (want only window 342)", k, ok)
	}
	close(hang)
	eventually(t, "the later layer gives it back", 3*time.Second, func() bool {
		f.m.recoverOverlayMark(f.i, "", f.ops)
		return f.ops.Enabled(342)
	})
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
}

// A keeper only changes or deletes a mark that is its own: the mark of another round (or overlay) is left.
func TestKeeperOnlyDeletesItsOwnMark(t *testing.T) {
	f := newKeeperFixture(t, nil, 351)
	other, _ := startSleeper(t)
	putMark(t, f.m, f.i, markOf("round-2", other, f.q, winsOf(f.q, 351)))
	f.k.Close("test", time.Second)
	if k, ok := f.m.readOverlayMark(f.i); !ok || k.Round != "round-2" {
		t.Errorf("the mark of another round after Close: %+v %v", k, ok)
	}
}

// The boot watch looks for orphan marks and recovers them within about a second, with nobody asking for the status.
func TestBootWatchRecoversAnOrphanMark(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(qemu.PID, 801, 802)
	withOverlayOS(t, ops)
	w := &bootWatch{m: m, i: i, q: qemu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.watchOverlayMarks(ctx, 100*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(250 * time.Millisecond)
	killed := time.Now()
	putMark(t, m, i, markOf("r", dead, qemu, winsOf(qemu, 801, 802)))
	eventually(t, "the watch gives the windows back", 3*time.Second, func() bool { return ops.Enabled(801) && ops.Enabled(802) })
	t.Logf("the watch gave the windows back %v after the mark was left (look every 100 ms in this test, every %v in the product)",
		time.Since(killed).Round(10*time.Millisecond), overlayWatchRecoverEvery)
	if overlayWatchRecoverEvery > time.Second {
		t.Errorf("the product looks every %v", overlayWatchRecoverEvery)
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
}

// ---- the start's side: the guard and the check at the end ----------------------------------------------------------------

// Layer 2: the start's guard waits on a handle to the overlay's process and gives the windows back the moment it ends,
// however it ended; only its own round's mark. It does nothing while the process lives, and ends with the start.
func TestGuardGivesTheWindowsBackWhenTheOverlayIsKilled(t *testing.T) {
	m, i := opsTestInstance(t)
	qemu, _ := startSleeper(t)
	overlayProc, kill := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 201, 202)
	withOverlayOS(t, ops)
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { return overlayProc, nil }
	putMark(t, m, i, markOf("R1", overlayProc, qemu, winsOf(qemu, 201, 202)))

	h := m.startBootOverlay(i, "R1")
	if h == nil {
		t.Fatal("no handle")
	}
	time.Sleep(700 * time.Millisecond)
	if ops.Enabled(201) {
		t.Fatal("the guard enabled windows under a live overlay")
	}
	killed := time.Now()
	kill()
	eventually(t, "the windows given back", 5*time.Second, func() bool { return ops.Enabled(201) && ops.Enabled(202) })
	if d := time.Since(killed); d > time.Second {
		t.Errorf("the windows came back %v after the overlay was killed", d)
	} else {
		t.Logf("windows back %v after the overlay process ended", d.Round(10*time.Millisecond))
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
	h.release()
	h.release()

	// another round's mark is not this start's to recover
	other, killOther := startSleeper(t)
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { return other, nil }
	ops.setEnabled(201, false)
	putMark(t, m, i, markOf("R2", other, qemu, winsOf(qemu, 201)))
	h2 := m.startBootOverlay(i, "R1")
	killOther()
	time.Sleep(700 * time.Millisecond)
	if ops.Enabled(201) {
		t.Error("a guard of one round gave back the windows of another round")
	}
	h2.release()

	// a start that ends before its overlay does leaves the overlay alone
	live, _ := startSleeper(t)
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { return live, nil }
	putMark(t, m, i, markOf("R1", live, qemu, winsOf(qemu, 201)))
	m.startBootOverlay(i, "R1").release()
	time.Sleep(700 * time.Millisecond)
	if ops.Enabled(201) {
		t.Error("a released guard touched the windows of a live overlay")
	}
}

// A start that is done waits (briefly) until its overlay has given the windows back, and ends an overlay that holds
// them for too long.
func TestOverlayHandleConfirm(t *testing.T) {
	m, i := opsTestInstance(t)
	qemu, _ := startSleeper(t)
	live, _ := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 211)
	withOverlayOS(t, ops)
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { return live, nil }

	// the overlay gives them back after a moment (its mark goes): confirm returns then
	putMark(t, m, i, markOf("R1", live, qemu, winsOf(qemu, 211)))
	h := m.startBootOverlay(i, "R1")
	defer h.release()
	time.AfterFunc(150*time.Millisecond, func() { m.removeOverlayMark(i) })
	begin := time.Now()
	h.confirm(3 * time.Second)
	if d := time.Since(begin); d < 100*time.Millisecond || d > time.Second {
		t.Errorf("confirm waited %v", d)
	}
	if h.proc.Alive() == false {
		t.Error("an overlay that gave the windows back was ended")
	}

	// it holds them: ended after the limit, windows given back by the start
	putMark(t, m, i, markOf("R1", live, qemu, winsOf(qemu, 211)))
	ops.setEnabled(211, false)
	begin = time.Now()
	h.confirm(300 * time.Millisecond)
	if d := time.Since(begin); d > 3*time.Second {
		t.Errorf("confirm waited %v", d)
	}
	if h.proc.Alive() {
		t.Error("the overlay that did not give the windows back is still running")
	}
	if !ops.Enabled(211) {
		t.Error("the windows were not given back")
	}
	if _, ok := m.readOverlayMark(i); ok {
		t.Error("the mark stays")
	}
	// nil handle: nothing
	(*overlayHandle)(nil).confirm(time.Second)
}

// An overlay that cannot start is a warning and a boot without the cover.
func TestStartBootOverlayFailureIsOnlyAWarning(t *testing.T) {
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	withOverlayOS(t, newFakeOverlayWindows(1))
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { return host.Process{}, errors.New("boom") }
	if h := m.startBootOverlay(3, "r"); h != nil {
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
	if h := m.startBootOverlay(3, "r"); h != nil {
		t.Error("an overlay without window operations")
	}
}

// ---- Start starts it ----------------------------------------------------------------------------------------------

// Start starts the boot screen for a boot that has a window and is set up here (launched, owed, or a take-over that is
// about to do real work), once; never for a headless one, and never for a take-over that only checks. The round is
// published (in the setup marker, or the setup record) before the overlay starts.
func TestStartStartsTheOverlayOnlyForAWindowedBoot(t *testing.T) {
	setCap(t, time.Hour)
	withOverlayOS(t, newFakeOverlayWindows(1))
	for _, c := range []struct {
		name          string
		windowed      bool
		owed          bool
		uninitialized bool
		want          int32
	}{
		{"owed, windowed", true, true, false, 1},
		{"owed, headless", false, true, false, 0},
		{"plain take-over, windowed", true, false, false, 0},
		{"take-over with first-boot setup to do, windowed", true, false, true, 1},
		{"take-over with first-boot setup to do, headless", false, false, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			sleeper, _ := startSleeper(t) // before the fake adb is in the environment: it would run as that
			s := newStartRun(t)
			var spawned atomic.Int32
			var published atomic.Bool
			saved := spawnBootOverlay
			t.Cleanup(func() { spawnBootOverlay = saved })
			spawnBootOverlay = func(m *Manager, i int, round string) (host.Process, error) {
				spawned.Add(1)
				mk, ok := m.readSetupMarker(i)
				rec, rok := m.readReady(i)
				published.Store(round != "" && (ok && mk.Round == round || rok && rec.Round == round))
				return sleeper, nil
			}
			if c.windowed {
				q := s.q
				q.Name = "qemu-system-x86_64.exe" // the windowed build
				startFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
				waitBootFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
			}
			switch {
			case c.owed:
				s.owedMarker()
			case c.uninitialized:
				if err := s.m.Root.EditInstanceMeta(s.i, func(meta *jsonord.Obj) error {
					meta.Set("initialized", jsonord.NewBool(false))
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			default:
				s.m.writeBootRecord(s.i, bootRecord{QemuPID: s.q.PID, QemuCreated: s.q.Created, BootID: testBootA})
			}
			if _, err := s.start(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if n := spawned.Load(); n != c.want {
				t.Errorf("overlay started %d times, want %d", n, c.want)
			}
			if c.want > 0 && !published.Load() {
				t.Error("the round was not in the marker or the record when the overlay was started")
			}
		})
	}
}

// A start that sets up a boot again after a guest reboot starts its overlay with a round of its own: the "done" record of
// the boot before (same qemu) is gone before the overlay starts, and a setup record of another round is no record for it.
func TestOldDoneRecordNeverReleasesALaterRound(t *testing.T) {
	sleeper, _ := startSleeper(t) // before the fake adb is in the environment: it would run as that
	s := newStartRun(t)
	setCap(t, time.Hour)
	withOverlayOS(t, newFakeOverlayWindows(1))
	q := s.q
	q.Name = "qemu-system-x86_64.exe"
	startFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	waitBootFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	s.m.writeBootRecord(s.i, bootRecord{QemuPID: s.q.PID, QemuCreated: s.q.Created, BootID: testBootA})
	s.guestBoot(testBootB) // the guest restarted by itself
	putRecord(t, s.m, s.i, s.q, func(r *readyRecord) { r.Done, r.Percent, r.Step, r.Round = true, 100, "sdcard", "old-round" })
	var recordAtSpawn atomic.Bool
	var roundSeen atomic.Value
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(m *Manager, i int, round string) (host.Process, error) {
		_, ok := m.readReady(i)
		recordAtSpawn.Store(ok)
		roundSeen.Store(round)
		return sleeper, nil
	}
	if _, err := s.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if recordAtSpawn.Load() {
		t.Error("the done record of the boot before was still there when the overlay started")
	}
	if r, _ := roundSeen.Load().(string); r == "" || r == "old-round" {
		t.Errorf("round %q", r)
	}

	// the readers of a given round: the old record does not count, whatever it says
	m := s.m
	putRecord(t, m, s.i, s.q, func(r *readyRecord) { r.Done, r.Round = true, "old-round" })
	unlock := lockStart(t, m, s.i)
	defer unlock()
	// the start of the new round has published its marker (the round, and a fresh heartbeat) and has no record yet
	m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix(),
		AliveMS: readyClockMS(), Round: "new-round"})
	if st := m.bootSetupOf(s.i, s.q, true, time.Now(), nil, "new-round"); st.Ready || st.Step != readyStepBoot {
		t.Errorf("a done record of another round released the round: %+v", st)
	}
	putRecord(t, m, s.i, s.q, func(r *readyRecord) { r.Done, r.Round = true, "new-round" })
	if st := m.bootSetupOf(s.i, s.q, true, time.Now(), nil, "new-round"); !st.Ready {
		t.Error("the round's own done record did not release it")
	}
	// status: the round of the start that holds the lock is the one its marker names
	m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix(),
		AliveMS: readyClockMS(), Round: "newer-round"})
	if st := m.bootSetupOf(s.i, s.q, true, time.Now(), nil, ""); st.Ready || st.Step != readyStepBoot {
		t.Errorf("status read an old round's done record while a newer start holds the lock: %+v", st)
	}
}

// ---- the record that cannot be written ---------------------------------------------------------------------------------

// "Done" cannot be written (only that write fails): the readers do not go on reading the step before it; the instance
// reads online at once and the start returns.
func TestStartDoneRecordWriteFailureIsStillOnline(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	withVar(t, &readyWrite, func(path string, data []byte, tag string) error {
		if bytes.Contains(data, []byte(`"done":true`)) {
			return errors.New("disk full")
		}
		return writeFileReplace(path, data, tag)
	})
	seen := map[string]InstanceInfo{}
	res, err := s.start(context.Background(), func(step string, in InstanceInfo) { seen[step] = in })
	if err != nil {
		t.Fatal(err)
	}
	if in := seen["tune"]; in.Status != StatusStarting {
		t.Errorf("at tune: %s", in.Status)
	}
	if in := seen["done"]; in.Status != StatusOnline {
		t.Errorf("at done, the done record could not be written: %s step %q", in.Status, in.SetupStep)
	}
	if res.Instance.Status != StatusOnline {
		t.Errorf("result %s", res.Instance.Status)
	}
	if in := infoOf(t, s.m, s.i, &s.q); in.Status != StatusOnline || in.Busy != "" {
		t.Errorf("after: %s busy %q", in.Status, in.Busy)
	}
}

// Neither the record nor the marker can be written or removed at the end: the start returning (its lock released) is
// the release; it is never "starting" with nobody working on it.
func TestStartRecordAndMarkerBothFailingStillEndsOnline(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	var obstacle sync.Once
	withVar(t, &readyWrite, func(path string, data []byte, tag string) error {
		if bytes.Contains(data, []byte(`"done":true`)) {
			// the marker's place becomes a folder with a file in it: it can be neither rewritten nor removed
			obstacle.Do(func() {
				mp := s.m.setupMarkerPath(s.i)
				os.Remove(mp)
				os.MkdirAll(mp, 0o755)
				os.WriteFile(filepath.Join(mp, "x"), []byte("x"), 0o644)
			})
			return errors.New("disk full")
		}
		return writeFileReplace(path, data, tag)
	})
	if _, err := s.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if in := infoOf(t, s.m, s.i, &s.q); in.Status != StatusOnline || in.Busy != "" || in.SetupStep != "" {
		t.Errorf("after the start: %s busy %q step %q", in.Status, in.Busy, in.SetupStep)
	}
}

// ---- the decisions ------------------------------------------------------------------------------------------------

func TestOverlayMachineLifecycle(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	om := newOverlayMachine(t0)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	in := func(s float64) overlayIn {
		return overlayIn{Now: at(s), QemuFound: true, StartHeld: true, WatchAlive: true, Tracked: true}
	}
	step := func(i overlayIn) overlayOut { return om.Next(i) }

	// Before the setup says how far it is there is no real percentage: the bar is indeterminate. No qemu yet, the qemu,
	// adb answering, Android booted but no record of the setup yet.
	o := step(overlayIn{Now: at(0.2), StartHeld: true, WatchAlive: true, Tracked: true})
	if o.Exit || o.Release || o.Stage != "launch" || o.Percent >= 0 {
		t.Fatalf("waiting for the qemu: %+v", o)
	}
	if o = step(in(1)); o.Exit || o.Stage != "launch" || o.Percent >= 0 {
		t.Fatalf("qemu up: %+v", o)
	}
	a := in(14)
	a.Answered = true
	if o = step(a); o.Exit || o.Stage != "android" || o.Percent >= 0 {
		t.Fatalf("Android booting: %+v", o)
	}
	b := in(26)
	b.Answered, b.Booted, b.Setup = true, true, bootSetupState{Step: readyStepBoot}
	if o = step(b); o.Exit || o.Release || o.Stage != readyStepBoot || o.Percent >= 0 {
		t.Fatalf("booted, no setup record yet: %+v", o)
	}
	// the setup's own steps: the bar is its percentage, the stage its step
	last := -1.0
	for k, s := range []bootSetupState{{Step: "vulkan_fix", Progress: 5}, {Step: "tune", Progress: 40}, {Step: "ime", Progress: 70},
		{Step: readyStepSDCard, Progress: 95}} {
		b := in(27 + float64(k))
		b.Answered, b.Booted, b.Setup = true, true, s
		o = step(b)
		if o.Exit || o.Release || o.Stage != s.Step || o.Percent != float64(s.Progress)/100 || o.Percent < last {
			t.Fatalf("setup step %s: %+v (last %.2f)", s.Step, o, last)
		}
		last = o.Percent
	}
	b = in(32)
	b.Answered, b.Booted, b.Setup = true, true, bootSetupState{Ready: true}
	if o = step(b); !o.Exit || o.Reason != "ready" || !o.Release || o.Percent != 1 {
		t.Fatalf("ready: %+v (the windows come back at once, no wait)", o)
	}
}
func TestOverlayMachineEndsWhenThereIsNothingToCover(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	base := func(s float64) overlayIn {
		return overlayIn{Now: at(s), QemuFound: true, StartHeld: true, WatchAlive: true, Tracked: true}
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
		return overlayIn{Now: t0.Add(time.Duration(s * float64(time.Second))), QemuFound: true, StartHeld: true, WatchAlive: true, Tracked: true,
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

// ---- the overlay process itself, on windows of a stand-in emulator ------------------------------------------------

const testRound = "round-test"

// record writes the setup record of the round the overlay is of, naming the stand-in qemu.
func (f *overlayFixture) record(mut func(*readyRecord)) readyRecord {
	f.t.Helper()
	return putRecord(f.t, f.m, f.i, f.q, func(r *readyRecord) {
		r.Round = testRound
		if mut != nil {
			mut(r)
		}
	})
}

// guardian makes the boot watch of the stand-in qemu present: its info names the qemu and a live process, and the instance's
// watch lock is held.
func (f *overlayFixture) guardian() {
	f.t.Helper()
	me, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok {
		f.t.Fatal("cannot read self")
	}
	f.m.writeWatchInfo(f.i, watchInfo{QemuPID: f.q.PID, QemuCreated: f.q.Created, WatchPID: me.PID, WatchCreated: me.Created})
	l, free, err := host.TryLock(f.m.lockPath(f.i, "watch"))
	if err != nil || !free {
		f.t.Fatalf("watch lock: %v %v", free, err)
	}
	f.t.Cleanup(l.Unlock)
}

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
	// the start of this round has published its marker, and the boot watch of the qemu is there (what the overlay looks for
	// before it disables anything)
	m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: time.Now().Add(time.Hour).Unix(),
		AliveMS: readyClockMS(), Round: testRound})
	f.guardian()
	f.run = newOverlayRun(m, i, logx.Nop(), me, testRound)
	f.run.bootID = func(context.Context) string { return "" }
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
	if !ok || len(k.Windows) != 2 || k.QemuPID != f.q.PID || k.OverlayPID != uint32(os.Getpid()) || k.Round != testRound {
		t.Errorf("mark %+v %v", k, ok)
	}
	for _, w := range k.Windows {
		if pid, class := win.WindowIdentity(uintptr(w.H)); pid != f.q.PID || class != w.Class || w.QemuPID != f.q.PID || w.QemuCreated != f.q.Created {
			t.Errorf("mark entry %+v does not say who the window is (%d %q)", w, pid, class)
		}
	}
	tg := win.GeometryOf(f.emu.Tool)
	if top := win.WindowFromPoint((tg.Frame.Left+tg.Frame.Right)/2, (tg.Frame.Top+tg.Frame.Bottom)/2); top == f.emu.Tool || top == 0 {
		t.Error("the toolbar is not covered")
	}

	// Android booted, the setup is running: still covered
	f.boot.Store(&[2]bool{true, true})
	f.record(nil)
	time.Sleep(900 * time.Millisecond)
	if a, b := f.windowsEnabled(); a || b || !f.covered() || f.ended.Load() {
		t.Fatalf("while the setup runs: enabled %v %v covered %v ended %v", a, b, f.covered(), f.ended.Load())
	}

	// a window that enables itself again (Qt does, on a restore) is disabled again while the cover is up
	win.SetWindowEnabled(f.emu.Main, true)
	f.waitFor("the window disabled again", 2*time.Second, func() bool { return !win.IsWindowEnabled(f.emu.Main) })

	// done: the windows are back at once and the overlay ends
	done := time.Now()
	f.record(func(r *readyRecord) { r.Done = true })
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

// What the overlay does not own it does not touch: Qt's hidden helper window stays hidden, enabled and where it was, and a
// window that was disabled before the overlay came is left disabled when the windows are given back (only what the
// overlay disabled is given back, to the state it had).
func TestOverlayRunTouchesOnlyWhatItDisabled(t *testing.T) {
	f := newOverlayFixture(t)
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	helperBefore := win.GeometryOf(f.emu.Helper)
	if helperBefore.Visible || !win.IsWindowEnabled(f.emu.Helper) {
		t.Fatalf("the stand-in helper window: %+v", helperBefore)
	}
	win.SetWindowEnabled(f.emu.Tool, false) // disabled before the overlay comes
	f.start()
	f.waitFor("the main window disabled", 10*time.Second, func() bool { return !win.IsWindowEnabled(f.emu.Main) })
	k, ok := f.m.readOverlayMark(f.i)
	if !ok || len(k.Windows) != 1 || k.Windows[0].H != uint64(f.emu.Main) {
		t.Errorf("mark %+v: want the main window only", k)
	}
	f.boot.Store(&[2]bool{true, true})
	f.record(func(r *readyRecord) { r.Done = true })
	if c := f.wait("done", 5*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if !win.IsWindowEnabled(f.emu.Main) {
		t.Error("the main window was not given back")
	}
	if win.IsWindowEnabled(f.emu.Tool) {
		t.Error("a window that was disabled before was enabled")
	}
	if g := win.GeometryOf(f.emu.Helper); g != helperBefore || !win.IsWindowEnabled(f.emu.Helper) {
		t.Errorf("the helper window changed: %+v", g)
	}
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
	f.record(nil)
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	gm := win.GeometryOf(f.emu.Main)
	cover := win.WindowFromPoint((gm.Client.Left+gm.Client.Right)/2, (gm.Client.Top+gm.Client.Bottom)/2)
	coverVisible := func() bool { return cover != 0 && cover != f.emu.Main && win.GeometryOf(cover).Visible }
	at := time.Now()
	f.record(func(r *readyRecord) { r.DeadlineMS = readyClockMS() - 1; r.Step = "ime" })
	f.waitFor("the windows back at the cap", 5*time.Second, func() bool { a, b := f.windowsEnabled(); return a && b })
	if f.ended.Load() {
		t.Error("the cover went with the windows: no time to read the note")
	}
	// the picture stays up for the note, but it no longer takes the mouse: what is at the middle of the picture area is the
	// emulator window again (the cover is a layer that only shows)
	g := win.GeometryOf(f.emu.Main)
	cx, cy := (g.Client.Left+g.Client.Right)/2, (g.Client.Top+g.Client.Bottom)/2
	if top := win.WindowFromPoint(cx, cy); top != f.emu.Main {
		t.Errorf("after the cap the mouse still lands on %#x, not on the emulator window %#x", top, f.emu.Main)
	}
	if !coverVisible() {
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
	var ws []overlayWin
	for _, h := range []uintptr{f.emu.Main, f.emu.Tool} {
		_, class := win.WindowIdentity(h)
		ws = append(ws, overlayWin{H: uint64(h), QemuPID: f.q.PID, QemuCreated: f.q.Created, Class: class})
	}
	putMark(t, f.m, f.i, markOf("r", dead, f.q, ws))
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

// An end asked for before the loop has begun is not lost: nothing is disabled, no mark is written, the overlay ends.
func TestOverlayRunEndAskedForBeforeTheLoopBegins(t *testing.T) {
	f := newOverlayFixture(t)
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	f.run.quitReq.Store(true)
	f.start()
	if c := f.wait("end", 5*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows disabled by an overlay that was told to end first: %v %v", a, b)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("a mark was written")
	}
}

// What the poller decides never waits for adb: a getprop that does not return does not hold up the end of the overlay
// when the start is gone (the windows are back within the start-lock grace plus a look).
func TestOverlayRunDoesNotWaitForADBToEnd(t *testing.T) {
	f := newOverlayFixture(t)
	f.run.probe = func(context.Context) (bool, bool) { time.Sleep(8 * time.Second); return false, false }
	unlock := lockStart(t, f.m, f.i)
	f.start()
	f.waitFor("the windows disabled", 10*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	gone := time.Now()
	unlock()
	if c := f.wait("start gone with a probe that hangs", 6*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows not back: %v %v", a, b)
	}
	t.Logf("windows back %v after the start lock was released, with a getprop that never returns", time.Since(gone).Round(10*time.Millisecond))
}

// Without the boot watch of the qemu the cover goes up and keeps the mouse out, but the windows are not disabled; they are
// once the watch is there.
func TestOverlayRunDisablesNothingBeforeTheBootWatchIsThere(t *testing.T) {
	f := newOverlayFixture(t)
	os.Remove(f.m.watchInfoPath(f.i))
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	f.start()
	f.waitFor("covered", 10*time.Second, func() bool { return f.covered() })
	time.Sleep(1300 * time.Millisecond)
	if a, b := f.windowsEnabled(); !a || !b {
		t.Fatalf("windows disabled with no boot watch: %v %v", a, b)
	}
	me, _ := host.FindProcess(uint32(os.Getpid()))
	f.m.writeWatchInfo(f.i, watchInfo{QemuPID: f.q.PID, QemuCreated: f.q.Created, WatchPID: me.PID, WatchCreated: me.Created})
	f.waitFor("disabled once the watch is there", 5*time.Second, func() bool { a, b := f.windowsEnabled(); return !a && !b })
	unlock()
	f.wait("end", 8*time.Second) // one overlay per process: the next test starts its own
}

// A round nobody tracks (no marker, no record of it) ends within the grace without having disabled anything.
func TestOverlayRunUntrackedRoundEndsWithoutDisabling(t *testing.T) {
	f := newOverlayFixture(t)
	f.m.removeSetupMarker(f.i)
	unlock := lockStart(t, f.m, f.i)
	defer unlock()
	f.start()
	if c := f.wait("untracked", 8*time.Second); c != 0 {
		t.Errorf("exit code %d", c)
	}
	if a, b := f.windowsEnabled(); !a || !b {
		t.Errorf("windows disabled by an overlay of a round nobody tracks: %v %v", a, b)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("a mark was written")
	}
}

func TestBootOverlayArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"x"}, {"root", "3", "1", "2"}, {"root", "99", "1", "2", "r"}, {"root", "3", "0", "2", "r"},
		{"root", "x", "1", "2", "r"}, {"root", "3", "1", "2", ""}} {
		if code := RunBootOverlay(args, discard{}); code != 2 {
			t.Errorf("args %q: exit code %d, want 2", slices.Clone(args), code)
		}
	}
}

// ---- the second review: what must not be disabled, and what must be known first --------------------------------------------

// An overlay that cannot read its own process identity (or a qemu whose identity is not whole) disables nothing: the
// recovery could not tell its mark from another's.
func TestKeeperNeedsWholeIdentities(t *testing.T) {
	zero := win.Process{}
	f := newKeeperFixture(t, &zero, 361, 362)
	f.k.Track(f.q, handlesOf(361, 362))
	f.k.Decide()
	time.Sleep(900 * time.Millisecond)
	if f.ops.calls != 0 || !f.ops.Enabled(361) {
		t.Fatalf("windows were disabled by an overlay that does not know who it is: %d calls", f.ops.calls)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("a mark was written")
	}
	warned := 0
	for _, msg := range f.logs.Messages() {
		if strings.Contains(msg, "读不到遮罩进程自己的创建时间") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d warnings, want 1", warned)
	}
	// a qemu with no creation time
	g := newKeeperFixture(t, nil, 363)
	g.k.Track(win.Process{PID: g.q.PID}, handlesOf(363))
	g.k.Decide()
	time.Sleep(700 * time.Millisecond)
	if g.ops.calls != 0 {
		t.Error("a window was disabled for a qemu whose identity is not whole")
	}
}

// A mark that is there and cannot be read is not "no mark": nothing is disabled, and the file is left as it was.
func TestKeeperDoesNotDisableOverAnUnreadableMark(t *testing.T) {
	f := newKeeperFixture(t, nil, 371)
	if err := os.WriteFile(f.m.overlayMarkPath(f.i), []byte(`{"overlay_pid":12`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.k.Track(f.q, handlesOf(371))
	f.k.Decide()
	time.Sleep(900 * time.Millisecond)
	if f.ops.calls != 0 || !f.ops.Enabled(371) {
		t.Fatal("a window was disabled although the mark that is there could not be read")
	}
	if b, _ := os.ReadFile(f.m.overlayMarkPath(f.i)); string(b) != `{"overlay_pid":12` {
		t.Errorf("the unreadable mark was overwritten: %q", b)
	}
}

// What an earlier overlay could not give back is not lost when a new overlay writes its own mark: the old entries are
// carried into it, given back at once, and kept in the mark until they answer.
func TestKeeperCarriesWhatTheOverlayBeforeLeft(t *testing.T) {
	dead := deadProcess(t)
	f := newKeeperFixture(t, &dead, 381, 382) // 381 is the new overlay's, 382 an old one's that did not answer
	f.ops.setEnabled(382, false)              // left disabled by the old overlay
	f.ops.fail[382] = true
	old, _ := startSleeper(t)
	putMark(t, f.m, f.i, markOf("round-old", old, f.q, winsOf(f.q, 382)))
	f.k.Track(f.q, handlesOf(381))
	f.k.Decide()
	eventually(t, "the new window disabled", 3*time.Second, func() bool { return !f.ops.Enabled(381) })
	k, ok := f.m.readOverlayMark(f.i)
	if !ok || k.Round != "round-1" || !markHas(f.m, f.i, 381) || !markHas(f.m, f.i, 382) {
		t.Fatalf("the mark after the new overlay wrote it: %+v %v (want both windows, the old one carried)", k, ok)
	}
	f.ops.mu.Lock()
	f.ops.fail[382] = false
	f.ops.mu.Unlock()
	eventually(t, "the carried window given back", 3*time.Second, func() bool { return f.ops.Enabled(382) })
	eventually(t, "the carried window leaves the mark", 3*time.Second, func() bool { return !markHas(f.m, f.i, 382) })
	if f.ops.Enabled(381) {
		t.Error("the new overlay's own window was given back with the old one's")
	}
	f.k.Close("test", 2*time.Second)
	if !f.ops.Enabled(381) {
		t.Error("the window is not back at the end")
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("the mark stays")
	}
}

// The window is checked on the goroutine that makes the call, right before it: a handle that became another window after
// the earlier looks is never touched.
func TestSetEnabledCheckedLooksRightBeforeTheCall(t *testing.T) {
	qemu, _ := startSleeper(t)
	other, _ := startSleeper(t)
	ops := newFakeOverlayWindows(qemu.PID, 391)
	ops.setEnabled(391, true)
	w := winsOf(qemu, 391)[0]
	ops.reown(391, other.PID)
	if ok, timedOut, skipped := setEnabledChecked(ops, w, false, time.Second); ok || timedOut || !skipped || ops.setCallsOf(391) != 0 {
		t.Fatalf("a window that is another's now: ok %v timedOut %v skipped %v calls %d", ok, timedOut, skipped, ops.setCallsOf(391))
	}
	ops.reown(391, qemu.PID)
	if ok, _, skipped := setEnabledChecked(ops, w, false, time.Second); !ok || skipped || ops.Enabled(391) {
		t.Fatalf("its own window: ok %v skipped %v enabled %v", ok, skipped, ops.Enabled(391))
	}
}

// Nothing is disabled before the boot watch of this qemu is running; after overlayGuardianWait without it the keeper gives
// up for this run.
func TestKeeperWaitsForTheBootWatch(t *testing.T) {
	withVar(t, &overlayGuardianWait, 700*time.Millisecond)
	f := newKeeperFixture(t, nil, 401)
	var up atomic.Bool
	f.k.guardian = func(host.Process) bool { return up.Load() }
	f.k.Track(f.q, handlesOf(401))
	f.k.Decide()
	time.Sleep(450 * time.Millisecond)
	if f.ops.calls != 0 {
		t.Fatal("a window was disabled before the boot watch was there")
	}
	up.Store(true)
	eventually(t, "disabled once the watch is there", 3*time.Second, func() bool { return !f.ops.Enabled(401) })

	// never there: given up, and not disabled later either
	g := newKeeperFixture(t, nil, 402)
	var up2 atomic.Bool
	g.k.guardian = func(host.Process) bool { return up2.Load() }
	g.k.Track(g.q, handlesOf(402))
	g.k.Decide()
	time.Sleep(1700 * time.Millisecond)
	up2.Store(true)
	time.Sleep(900 * time.Millisecond)
	if g.ops.calls != 0 {
		t.Error("a window was disabled after the keeper gave up waiting for the boot watch")
	}
	warned := 0
	for _, msg := range g.logs.Messages() {
		if strings.Contains(msg, "开机守护") && strings.Contains(msg, "没就位") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d warnings, want 1: %q", warned, g.logs.Messages())
	}
}

// The boot watch of exactly this qemu: its info names the qemu, its process is alive and the watch lock is held.
func TestWatchRunningFor(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	watchProc, killWatch := startSleeper(t)
	other := win.Process{PID: qemu.PID, Created: qemu.Created + 1}
	if m.watchRunningFor(i, qemu) {
		t.Fatal("running with no info")
	}
	m.writeWatchInfo(i, watchInfo{QemuPID: qemu.PID, QemuCreated: qemu.Created, WatchPID: watchProc.PID, WatchCreated: watchProc.Created})
	if m.watchRunningFor(i, qemu) {
		t.Error("running while the watch lock is not held")
	}
	l, free, err := host.TryLock(m.lockPath(i, "watch"))
	if err != nil || !free {
		t.Fatal(err)
	}
	defer l.Unlock()
	if !m.watchRunningFor(i, qemu) {
		t.Error("not running with info, a live process and the lock")
	}
	if m.watchRunningFor(i, other) {
		t.Error("the watch of another qemu (same pid, another creation time) counted")
	}
	killWatch()
	if m.watchRunningFor(i, qemu) {
		t.Error("a watch process that is gone counted")
	}
}

// The boot watch only looks at marks of its own qemu (process and creation time).
func TestBootWatchIgnoresTheMarkOfAnotherQemu(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	qemu, _ := startSleeper(t)
	other, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(other.PID, 811)
	withOverlayOS(t, ops)
	putMark(t, m, i, markOf("r", dead, other, winsOf(other, 811)))
	w := &bootWatch{m: m, i: i, q: qemu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.watchOverlayMarks(ctx, 50*time.Millisecond); close(done) }()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done
	if ops.Enabled(811) {
		t.Error("the watch of one qemu gave back the windows of another's mark")
	}
}

// The same windows not answering is not rewritten into the mark and not warned about again at every look.
func TestRecoverOverlayDoesNotRewriteOrRepeatWhenNothingChanged(t *testing.T) {
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	const i = 3
	qemu, _ := startSleeper(t)
	dead := deadProcess(t)
	ops := newFakeOverlayWindows(qemu.PID, 821)
	ops.fail[821] = true
	putMark(t, m, i, markOf("r", dead, qemu, winsOf(qemu, 821)))
	m.recoverOverlayMark(i, "", ops)
	st1, err := os.Stat(m.overlayMarkPath(i))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	for n := 0; n < 3; n++ {
		m.recoverOverlayMark(i, "", ops)
	}
	st2, _ := os.Stat(m.overlayMarkPath(i))
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("the mark was written again with nothing changed")
	}
	warned := 0
	for _, msg := range logs.Messages() {
		if strings.Contains(msg, "没应答") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d warnings for four looks at the same state, want 1", warned)
	}
}

// Machine: a round that is not seen tracked is not decided about (nothing disabled) and ends; one that is tracked goes on.
// And the cap from the overlay's own start does not wait for adb.
func TestOverlayMachineTrackingAndHardCap(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	om := newOverlayMachine(t0)
	in := overlayIn{Now: at(0.5), QemuFound: true, StartHeld: true, WatchAlive: true}
	if o := om.Next(in); o.Exit || o.Decided {
		t.Fatalf("not tracked yet: %+v (nothing is decided before the round is seen)", o)
	}
	in.Now = at(2.1)
	if o := om.Next(in); !o.Exit || o.Reason != "untracked" || o.Decided || o.Release {
		t.Fatalf("never tracked: %+v", o)
	}
	om = newOverlayMachine(t0)
	in.Now, in.Tracked = at(0.5), true
	if o := om.Next(in); o.Exit || !o.Decided {
		t.Fatalf("tracked: %+v", o)
	}
	in.Now, in.Tracked = at(5), false // the marker goes (the setup is over): it was seen, that is enough
	if o := om.Next(in); o.Exit || !o.Decided {
		t.Fatalf("tracked once: %+v", o)
	}
	// adb never says Android booted: the cap from the start of the overlay puts the windows back, naming the stage
	withVar(t, &overlayHardCap, func() time.Duration { return 90 * time.Second })
	in.Now = at(89)
	if o := om.Next(in); o.Release {
		t.Fatalf("before the hard cap: %+v", o)
	}
	in.Now = at(91)
	o := om.Next(in)
	if !o.Release || o.Exit || o.Note == "" || o.Percent != 1 {
		t.Fatalf("at the hard cap with adb silent: %+v", o)
	}
	in.Now = at(91 + overlayCapNote.Seconds() + 0.1)
	if o = om.Next(in); !o.Exit || o.Reason != "cap" || !o.Release {
		t.Fatalf("after the note: %+v", o)
	}
}

// A start that cannot publish its round (the marker cannot be written) starts no boot screen.
func TestStartWithoutAPublishedRoundHasNoOverlay(t *testing.T) {
	setCap(t, time.Hour)
	withOverlayOS(t, newFakeOverlayWindows(1))
	sleeper, _ := startSleeper(t)
	s := newStartRun(t)
	q := s.q
	q.Name = "qemu-system-x86_64.exe"
	startFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	waitBootFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	s.owedMarker()
	var spawned atomic.Int32
	saved := spawnBootOverlay
	t.Cleanup(func() { spawnBootOverlay = saved })
	spawnBootOverlay = func(*Manager, int, string) (host.Process, error) { spawned.Add(1); return sleeper, nil }
	withVar(t, &setupMarkerWrite, func(string, []byte, string) error { return errors.New("disk full") })
	if _, err := s.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if spawned.Load() != 0 {
		t.Error("a boot screen was started for a round that could not be published")
	}
	warned := false
	for _, msg := range s.logs.Messages() {
		warned = warned || strings.Contains(msg, "这次不起开机遮罩")
	}
	if !warned {
		t.Errorf("no warning in %q", s.logs.Messages())
	}
}

// After the windows are given back (Release) nothing is disabled again, whatever is still told about covers.
func TestKeeperNeverDisablesAfterRelease(t *testing.T) {
	f := newKeeperFixture(t, nil, 411, 412)
	f.k.Release()
	f.k.Track(f.q, handlesOf(411, 412))
	f.k.Decide()
	time.Sleep(900 * time.Millisecond)
	if f.ops.calls != 0 || !f.ops.Enabled(411) || !f.ops.Enabled(412) {
		t.Fatalf("windows were disabled after Release: %d calls", f.ops.calls)
	}
	if _, ok := f.m.readOverlayMark(f.i); ok {
		t.Error("a mark was written after Release")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestOverlayPreview writes pictures of the cover (the window read back with PrintWindow, text and all) when
// MODAVD_OVERLAY_PREVIEW names a folder: the states the screen has, at the shapes a window has. For looking at; the
// stand-in windows are far off the screen.
func TestOverlayPreview(t *testing.T) {
	dir := os.Getenv("MODAVD_OVERLAY_PREVIEW")
	if dir == "" {
		t.Skip("set MODAVD_OVERLAY_PREVIEW to a folder")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	type shot struct {
		name string
		out  overlayOut
	}
	shots := []shot{
		{"launch-indeterminate", overlayOut{Stage: "launch", Percent: -1}},
		{"tune-40", overlayOut{Stage: "tune", Percent: 0.40}},
		{"ime-80", overlayOut{Stage: "ime", Percent: 0.80}},
		{"cap-note", overlayOut{Stage: "ime", Percent: 1, Note: "ime"}},
	}
	for _, size := range [][2]int{{1280, 720}, {960, 720}, {720, 1280}} {
		for _, sh := range shots {
			f := newOverlayFixture(t)
			unlock := lockStart(t, f.m, f.i)
			f.emu.Move(wintest.X, wintest.Y, size[0]+16, size[1]+39)
			out := sh.out
			f.run.fixed.Store(&out)
			f.start()
			f.waitFor("covered", 10*time.Second, func() bool { return f.covered() })
			time.Sleep(700 * time.Millisecond)
			cover := win.WindowFromPoint(int32(wintest.X+size[0]/2), int32(wintest.Y+size[1]/2+30))
			img, ok := win.CapturePixels(cover, false)
			if !ok {
				t.Fatalf("cannot read the cover back")
			}
			name := filepath.Join(dir, fmt.Sprintf("%dx%d-%s.png", img.Bounds().Dx(), img.Bounds().Dy(), sh.name))
			var b bytes.Buffer
			if err := png.Encode(&b, img); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, b.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			f.run.fixed.Store(&overlayOut{Exit: true, Reason: "test", Release: true})
			unlock()
			if c := f.wait("end", 8*time.Second); c != 0 {
				t.Errorf("exit code %d", c)
			}
		}
	}
}
