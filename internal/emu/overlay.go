package emu

// The boot screen (ModAVD/ModAVD#259): while a window-mode instance boots and its after-boot setup runs, a cover lies
// over the emulator's window from the moment the window shows until the instance reads online (bootready.go), and the
// emulator's windows take no mouse and keyboard input. Windows only; the picture is internal/bootscreen.
//
// Who starts it, who ends it. A start that works on a boot that has a window (it launched the qemu, took over a boot
// whose setup was left unfinished or whose guest rebooted by itself, or is about to do real setup work on a running
// one) starts a small process of this same executable: `<exe> __bootoverlay <root> <i> <start pid> <start created>
// <round>` (RunBootOverlay), detached like the boot watch, and makes sure the boot watch of that qemu runs (it is the
// fourth layer below). Everything the cover shows it reads from disk and from the guest the way status does (the setup
// record, the start lock, the guest's boot state), so it has no channel to the start, and a cover started by anyone
// looks the same. It ends itself, putting the windows back first, when
//   - the instance is online: Android has booted and the setup of THIS round is done (bootSetupOf with the round says
//     ready). The cap (the setup went on past bootSetupCap) turns the covers into a layer the mouse goes through at once,
//     puts the windows back, and leaves the picture up three more seconds with a line saying which step was not finished;
//   - the qemu it covers is gone, or never showed up in 30 s;
//   - no start holds the start lock any more for 1.5 s (the start was cancelled, failed or killed, or the manager that
//     ran it exited: the lock goes with the process);
//   - the process that started it is gone (waited for with a handle to that process, not looked at now and then);
//   - it is told to end (Ctrl+C, a close of its windows, the end of the session).
//
// The round. Every start attempt has a round token (a random string). It goes into the setup marker, into the setup
// record, into the overlay's arguments and into its mark, and it is written BEFORE the overlay is started. The overlay
// (and status, through the marker of the start that holds the lock) only believes a setup record of its own round: a
// "done" left by an earlier boot of the same qemu (a guest that restarted by itself) never releases a later round.
//
// What keeps input out. A cover is a pop-up over the picture area of the main window and over the side toolbar, so mouse
// input lands on the cover (internal/win cover_windows.go); it has no owner and follows the emulator window with
// SetWindowPos on itself, so a hung emulator window cannot hang it. The keyboard cannot be covered, so the emulator's
// windows are also disabled (EnableWindow FALSE) like under a modal dialog. That is state of the other process's
// windows, it has to be given back whatever happens to the cover, and EnableWindow sends a message to the window's own
// thread and waits, so it is only ever called on a worker (windowKeeper) with a deadline, never on the cover's thread and
// never in a lifecycle command.
//   - Record first: the windows are named in the overlay's mark (.mas_<i>.overlay: round, overlay process, qemu, and per
//     window the handle, the owning qemu and its class) before they are disabled; when the mark cannot be written nothing is
//     disabled (the cover still keeps the mouse out).
//   - Handles are checked before every touch, and again on the goroutine that makes the call, right before it (owning
//     process, class, qemu alive): a handle that is another window now is dropped, never enabled or disabled.
//   - Nothing is disabled unless everything the recovery needs is known: the overlay's own process and creation time,
//     the qemu's (process and creation time, published whole), that this round is tracked (its setup marker or record is
//     there; a round nobody tracks ends within 2 s having disabled nothing), that the boot watch of exactly this qemu is
//     running (waited for up to 3 s, then given up on for this run), and that the mark that is already there can be read
//     (what an earlier overlay left in it is carried into the new one, never overwritten). Otherwise the cover only keeps
//     the mouse out.
//   - A start that cannot publish its round starts no overlay. The cap does not depend on adb: counted from the overlay's own
//     start (DefaultBootTimeout + bootSetupCap) it releases whatever adb says, and once released nothing is disabled again.
//   - Marks are read, changed and deleted under a lock, and only by the one they belong to (the same round and overlay
//     process); an enable that fails on a window that still exists keeps its entry for the next try.
//
// Layers that give the windows back, and how long they take at most (EnableWindow answering; a window that does not
// answer is given up on after overlayEnableDeadline, 2 s, and tried again by the next layer):
//  1. the overlay, before it exits, whatever the reason (a deferred close, also on a panic, a signal, WM_CLOSE, the end of
//     the session): about 0.1 s after the instance is ready, at most 3 s;
//  2. the start that started it holds a handle to the overlay's process and recovers as soon as it ends: at once;
//  3. the overlay holds a handle to the start's process, and watches the start lock: a killed start ends the overlay at
//     once, a cancelled one within 1.5 s;
//  4. the boot watch of the instance (__bootwatch, which lives as long as the qemu; started together with the overlay)
//     looks at the mark every second: a mark whose overlay process is gone is recovered: at most about 1 s (+ 2 s if a
//     window is slow to answer) after both the overlay and the start were killed;
//  5. last, any modavd that looks at the instance (status / list / info, start, stop, window) recovers it, in the
//     background with a budget of overlayRecoverBudget (1 s): the command never waits for a window.
//
// This file is the part that does not depend on the platform: the mark, the recovery, the keeper of the windows, the
// guard, what to show and when to end. The windows themselves are in overlay_windows.go.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"modavd/internal/host"
	"modavd/internal/i18n"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// BootOverlayArg is the hidden subcommand both executables route to RunBootOverlay.
const BootOverlayArg = "__bootoverlay"

var (
	// overlayCapNote: how long the cover stays up with its note after the cap put the windows back.
	overlayCapNote = 3 * time.Second
	// overlayEnableDeadline: how long one EnableWindow call is waited for.
	overlayEnableDeadline = 2 * time.Second
	// overlayRecoverBudget: how long a lifecycle command waits for a recovery it started in the background.
	overlayRecoverBudget = time.Second
	// overlayMarkLockWait: how long taking the mark's lock is waited for.
	overlayMarkLockWait = 300 * time.Millisecond
	// overlayWatchRecoverEvery: how often the boot watch looks for a mark whose overlay is gone.
	overlayWatchRecoverEvery = time.Second
	// overlayConfirmWait: how long a start that is done waits for its overlay to have given the windows back.
	overlayConfirmWait = 2 * time.Second
)

const (
	// overlayStartLimit: the overlay ends when its qemu has not shown up this long after it started.
	overlayStartLimit = 30 * time.Second
	// overlayNoStartGrace: how long no start holding the start lock is put up with before the overlay ends.
	overlayNoStartGrace = 1500 * time.Millisecond
	// overlayQemuGrace: a process scan that misses the qemu for this long means it is gone.
	overlayQemuGrace = 2 * time.Second
	// overlayGuardRetry: how often the start's guard tries again to give back windows that did not answer.
	overlayGuardRetry = 500 * time.Millisecond
)

// overlayWindows is what the overlay needs of the emulator's windows; Windows has the real one (overlay_windows.go),
// the tests fake it.
type overlayWindows interface {
	// Windows are the main window and the side toolbar of emulator process pid.
	Windows(pid uint32) ([]uintptr, error)
	// IsToolbar: the window is the side toolbar (its cover is plain, the screen goes on the main window).
	IsToolbar(h uintptr) bool
	// SetEnabled enables or disables h and reports whether it is in that state afterwards. It may not return for a window
	// that does not answer: call it through setEnabledBounded.
	SetEnabled(h uintptr, on bool) bool
	Enabled(h uintptr) bool
	// Identity is the process that owns h and its class name; pid 0 when the window is gone. It never waits for the window.
	Identity(h uintptr) (pid uint32, class string)
}

// overlayOS is the platform's window operations; nil where there are none.
var overlayOS overlayWindows

// overlayWin is a window an overlay disabled, with who it was: a handle can be reused by another window.
type overlayWin struct {
	H           uint64 `json:"h"`
	QemuPID     uint32 `json:"qemu_pid"`
	QemuCreated uint64 `json:"qemu_created"`
	Class       string `json:"class"`
}

// overlayMark is the content of overlayMarkPath: the round it is of, the overlay's process (PID + creation time), the qemu
// it covers and the windows it disabled.
type overlayMark struct {
	Round          string       `json:"round"`
	OverlayPID     uint32       `json:"overlay_pid"`
	OverlayCreated uint64       `json:"overlay_created"`
	QemuPID        uint32       `json:"qemu_pid"`
	QemuCreated    uint64       `json:"qemu_created"`
	Windows        []overlayWin `json:"windows"`
}

func (k overlayMark) overlayAlive() bool {
	return k.OverlayPID != 0 && (host.Process{PID: k.OverlayPID, Created: k.OverlayCreated}).Alive()
}

// sameOwner: both marks are of the same round and the same overlay process.
func (k overlayMark) sameOwner(o overlayMark) bool {
	return k.Round == o.Round && k.OverlayPID == o.OverlayPID && k.OverlayCreated == o.OverlayCreated
}

// overlayMarkPath is <root>\avd\.mas_<i>.overlay.
func (m *Manager) overlayMarkPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".overlay")
}

func (m *Manager) readOverlayMark(i int) (overlayMark, bool) {
	var k overlayMark
	b, err := os.ReadFile(m.overlayMarkPath(i))
	if err != nil || json.Unmarshal(b, &k) != nil {
		return overlayMark{}, false
	}
	return k, k.OverlayPID != 0
}

// overlayMarkWrite stores the mark (a variable: the tests make it fail).
var overlayMarkWrite = func(m *Manager, i int, k overlayMark) error {
	b, err := json.Marshal(k)
	if err == nil {
		err = writeFileReplace(m.overlayMarkPath(i), b, strconv.Itoa(os.Getpid()))
	}
	return err
}

// writeOverlayMark stores the mark; the caller holds the mark's lock.
func (m *Manager) writeOverlayMark(i int, k overlayMark) error { return overlayMarkWrite(m, i, k) }

// removeOverlayMark deletes the mark. On Windows a file that somebody has open for reading (status looks at the mark
// without the lock) cannot be deleted at that moment, so it is tried again for up to 0.2 s; an error is what is left after
// that, and the caller keeps the work to do (the mark stays, the next try deletes it).
func (m *Manager) removeOverlayMark(i int) error {
	var err error
	for n := 0; n < 40; n++ {
		if err = os.Remove(m.overlayMarkPath(i)); err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.Log.Debugf("实例 %d 删不掉遮罩标记: %v", i, err)
	return err
}

// overlayMarkLock takes the lock every read-change-delete of the mark of instance i is done under, waiting up to
// overlayMarkLockWait; ok=false when it could not be had (somebody else is at it, or the lock file cannot be opened).
func (m *Manager) overlayMarkLock(i int) (l *host.FileLock, ok bool) {
	path := m.lockPath(i, "overlaymark")
	deadline := time.Now().Add(overlayMarkLockWait)
	for {
		l, free, err := host.TryLock(path)
		if err == nil && free {
			return l, true
		}
		if err != nil || !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- calls to the other process's windows ------------------------------------------------------------------------------

// overlayInflight holds the windows an EnableWindow call is running on right now: a window that does not answer has one
// call stuck in it, and the next calls on it are not stacked up behind it.
var overlayInflight sync.Map

// setEnabledChecked is ops.SetEnabled on window w on a goroutine of its own, waited for at most d. The window is
// checked (owning process, class, qemu alive) on that goroutine, right before the call: a handle that is another window
// now is skipped, never touched. ok: the window is in the state asked for; timedOut: the call did not return in time (or
// one is stuck in that window already), it may still take effect later, so the window has to be treated as in either
// state; skipped: it is not the window it was (or is gone), nothing was called.
func setEnabledChecked(ops overlayWindows, w overlayWin, on bool, d time.Duration) (ok, timedOut, skipped bool) {
	h := uintptr(w.H)
	if _, busy := overlayInflight.LoadOrStore(h, struct{}{}); busy {
		return false, true, false
	}
	type result struct{ ok, skipped bool }
	res := make(chan result, 1)
	go func() {
		defer overlayInflight.Delete(h)
		if w.stateOf(ops) != windowOurs {
			res <- result{skipped: true}
			return
		}
		res <- result{ok: ops.SetEnabled(h, on)}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case r := <-res:
		return r.ok, false, r.skipped
	case <-t.C:
		return false, true, false
	}
}

type windowState int

const (
	windowGone    windowState = iota // the window or its qemu is gone
	windowForeign                    // the handle is another window's now
	windowOurs
)

// stateOf is what w is now: the same window of the same qemu, another window with that handle, or nothing.
func (w overlayWin) stateOf(ops overlayWindows) windowState {
	if w.QemuPID != 0 && !(host.Process{PID: w.QemuPID, Created: w.QemuCreated}).Alive() {
		return windowGone
	}
	pid, class := ops.Identity(uintptr(w.H))
	switch {
	case pid == 0:
		return windowGone
	case pid != w.QemuPID || class != w.Class:
		return windowForeign
	}
	return windowOurs
}

// restoreWindows enables every window of ws that is still the window it was; windows that are gone or are other windows
// now are dropped. The calls run side by side and are waited for at most d each. left: the windows that still exist and
// could not be enabled (the call failed or did not answer), to be tried again.
func restoreWindows(ops overlayWindows, ws []overlayWin, d time.Duration) (left []overlayWin, restored int) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		w := w
		go func() {
			defer wg.Done()
			ok, _, skipped := setEnabledChecked(ops, w, true, d)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				restored++
			} else if !skipped && w.stateOf(ops) == windowOurs { // still there: the next try
				left = append(left, w)
			}
		}()
	}
	wg.Wait()
	return left, restored
}

// recoverOverlayMark gives back the windows of the mark of instance i when its overlay is gone (round "": of any round;
// else only that round's), under the mark's lock: what is enabled is dropped from the mark, and the mark is deleted when
// nothing is left. done=false: the lock could not be had or windows are left, so the mark stays for the next try.
func (m *Manager) recoverOverlayMark(i int, round string, ops overlayWindows) (restored int, done bool) {
	if _, ok := m.readOverlayMark(i); !ok {
		return 0, true
	}
	l, ok := m.overlayMarkLock(i)
	if !ok {
		return 0, false
	}
	defer l.Unlock()
	k, ok := m.readOverlayMark(i)
	if !ok || k.overlayAlive() || (round != "" && k.Round != round) {
		return 0, true // none, or its overlay lives (it gives back its own), or it is another round's
	}
	var left []overlayWin
	if ops != nil {
		left, restored = restoreWindows(ops, k.Windows, overlayEnableDeadline)
	}
	removeFailed := false
	switch {
	case len(left) == 0:
		removeFailed = m.removeOverlayMark(i) != nil
		recoverWarned.Delete(i)
	case len(left) != len(k.Windows):
		// something changed: the mark says what is left. When nothing did (the same windows still do not answer) it is not
		// written again, and not warned about again: the boot watch looks every second.
		k.Windows = left
		if err := m.writeOverlayMark(i, k); err != nil {
			m.Log.Debugf("实例 %d 遮罩标记改不了: %v", i, err)
		}
		fallthrough
	default:
		if prev, _ := recoverWarned.Load(i); prev != len(left) {
			recoverWarned.Store(i, len(left))
			m.Log.Warnf("实例 %d 有 %d 个模拟器窗口没应答，还没恢复成能操作，稍后再试", i, len(left))
		}
	}
	if restored > 0 {
		m.Log.Infof("实例 %d 的遮罩没了（进程 %d），已把模拟器的 %d 个窗口恢复成能操作", i, k.OverlayPID, restored)
	}
	return restored, len(left) == 0 && !removeFailed
}

// recoverWarned: per instance, how many windows the last warning of a recovery named (a warning is given again only when
// that changes).
var recoverWarned sync.Map

// RecoverOverlay gives the emulator windows of instance i back when an overlay disabled them and is gone (layers 4 and 5
// above). It never waits for a window: the work runs in the background and is waited for at most overlayRecoverBudget; what
// is not done by then is left (mark kept) for the boot watch and the next command. It reports whether it put windows back
// in time. Nothing happens while the overlay lives. Cheap when there is no mark.
func (m *Manager) RecoverOverlay(i int) bool {
	k, ok := m.readOverlayMark(i)
	if !ok || k.overlayAlive() {
		return false
	}
	res := make(chan int, 1)
	go func() {
		n, _ := m.recoverOverlayMark(i, "", overlayOS)
		res <- n
	}()
	t := time.NewTimer(overlayRecoverBudget)
	defer t.Stop()
	select {
	case n := <-res:
		return n > 0
	case <-t.C:
		m.Log.Debugf("实例 %d 恢复遮罩禁用的窗口超过 %v 还没做完，放到后台继续", i, overlayRecoverBudget)
		return false
	}
}

// ---- the keeper of the windows -----------------------------------------------------------------------------------------

// overlayGuardianWait: how long the keeper waits for the boot watch of the qemu to be running before it gives up on
// disabling windows in this run (the cover still keeps the mouse out).
var overlayGuardianWait = 3 * time.Second

// windowKeeper disables the emulator's windows while the cover is up and gives them back; the overlay's other threads
// only tell it what they want (Track, Decide, Release, Stop) and never wait for it. All calls into the other process's
// windows (EnableWindow) are made here, on a goroutine of their own with a deadline.
//
// It disables nothing unless it is sure of everything the recovery will need: who the overlay is (process and creation
// time), which qemu (process and creation time), that the boot watch of that qemu is running (the layer that gives the
// windows back when the overlay and its start are both gone), and that the mark can be written (and the mark that is
// there already can be read: what an earlier overlay could not give back is carried into this one's).
type windowKeeper struct {
	m        *Manager
	i        int
	ops      overlayWindows
	log      *logx.Logger
	round    string
	self     host.Process              // the overlay's own process (the mark's owner)
	guardian func(q host.Process) bool // the boot watch of qemu q is running; nil: not asked (tests)

	stopped atomic.Bool   // an end was asked for: nothing more is disabled
	wake    chan struct{} // capacity 1
	closed  chan struct{}
	stepMu  sync.Mutex // held while the worker does one step

	mu          sync.Mutex
	qemu        host.Process
	covered     []uintptr              // windows a cover is over now
	decided     bool                   // the round is tracked and the first look at the instance has been made: nothing is disabled before
	released    bool                   // given back (or to be): nothing is disabled any more
	held        map[uintptr]overlayWin // windows this keeper disabled (all of them are in the mark)
	carried     []overlayWin           // windows an earlier overlay left in the mark and did not get back: given back, never disabled
	markPending bool                   // the mark still has to be removed or rewritten
	warned      map[string]bool
	guardSeen   qemuID // the qemu ready() looked at last
	guardFor    qemuID // the qemu whose boot watch was found running (zero: none yet); another qemu waits afresh
	firstWant   time.Time
	gaveUp      bool
	lastLeft    string
}

// qemuID is a qemu's identity: the PID alone can be reused, with the creation time it cannot.
type qemuID struct {
	PID     uint32
	Created uint64
}

func newWindowKeeper(m *Manager, i int, ops overlayWindows, log *logx.Logger, round string, self host.Process,
	guardian func(q host.Process) bool) *windowKeeper {
	k := &windowKeeper{m: m, i: i, ops: ops, log: log, round: round, self: self, guardian: guardian, wake: make(chan struct{}, 1),
		closed: make(chan struct{}), held: map[uintptr]overlayWin{}, warned: map[string]bool{}}
	go k.loop()
	return k
}

func (k *windowKeeper) signal() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// warnOnce logs a warning once per key.
func (k *windowKeeper) warnOnce(key, format string, args ...any) {
	k.mu.Lock()
	seen := k.warned[key]
	k.warned[key] = true
	k.mu.Unlock()
	if !seen {
		k.log.Warnf(format, args...)
	}
}

// Track says which windows a cover is over now, of which qemu (one value: the identity is never half of one).
func (k *windowKeeper) Track(q host.Process, hs []uintptr) {
	k.mu.Lock()
	k.qemu, k.covered = q, slices.Clone(hs)
	k.mu.Unlock()
	k.signal()
}

// Decide says the round is tracked (its marker or record is there) and the instance has been looked at once: from now on
// windows are disabled.
func (k *windowKeeper) Decide() {
	k.mu.Lock()
	k.decided = true
	k.mu.Unlock()
	k.signal()
}

// Release says the windows are to be given back now (and not disabled again), in the background.
func (k *windowKeeper) Release() {
	k.mu.Lock()
	k.released = true
	k.mu.Unlock()
	k.signal()
}

// Stop says an end was asked for: nothing is disabled from now on, not even by a step that has begun.
func (k *windowKeeper) Stop() { k.stopped.Store(true) }

// mustNotDisable: an end was asked for, or the windows are given back.
func (k *windowKeeper) mustNotDisable() bool {
	if k.stopped.Load() {
		return true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.released
}

// Close is the end: stops disabling, waits for the step that is under way (at most d), gives everything back (at most d)
// and stops the worker. What did not answer stays in the mark for the layers that come after.
func (k *windowKeeper) Close(why string, d time.Duration) {
	k.stopped.Store(true)
	k.mu.Lock()
	k.released = true
	k.mu.Unlock()
	deadline := time.Now().Add(d)
	got := false
	for !got && time.Now().Before(deadline) {
		if got = k.stepMu.TryLock(); !got {
			time.Sleep(5 * time.Millisecond)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.restoreHeld(why)
	}()
	t := time.NewTimer(max(time.Until(deadline), 50*time.Millisecond))
	select {
	case <-done:
	case <-t.C:
		k.log.Warnf("实例 %d 遮罩收尾时有窗口没在 %v 内应答，留给恢复的后几层", k.i, d)
	}
	t.Stop()
	if got {
		k.stepMu.Unlock()
	}
	select {
	case <-k.closed:
	default:
		close(k.closed)
	}
}

func (k *windowKeeper) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-k.closed:
			return
		case <-k.wake:
		case <-t.C:
		}
		k.step()
	}
}

func (k *windowKeeper) heldList() []overlayWin {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]overlayWin, 0, len(k.held))
	for _, w := range k.held {
		out = append(out, w)
	}
	return out
}

// markList is everything the mark has to name: what is held and what was carried.
func (k *windowKeeper) markList() []overlayWin {
	out := k.heldList()
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, w := range k.carried {
		if !slices.ContainsFunc(out, func(o overlayWin) bool { return o.H == w.H }) {
			out = append(out, w)
		}
	}
	return out
}

func (k *windowKeeper) ownMark(cur overlayMark) bool {
	return cur.Round == k.round && cur.OverlayPID == k.self.PID && cur.OverlayCreated == k.self.Created
}

// readMarkStrict reads the mark on disk: found, absent, or unreadable (a file that is there and cannot be read or
// parsed is not "absent").
func (m *Manager) readMarkStrict(i int) (k overlayMark, found bool, err error) {
	b, err := os.ReadFile(m.overlayMarkPath(i))
	if errors.Is(err, fs.ErrNotExist) {
		return overlayMark{}, false, nil
	}
	if err != nil {
		return overlayMark{}, false, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return overlayMark{}, false, fmt.Errorf("遮罩标记读不懂: %w", err)
	}
	return k, true, nil
}

// writeMark stores the mark with windows ws (plus what was carried), under the lock. A mark of another overlay that is
// still there (an earlier one that could not give everything back) is read under the same lock and its windows are
// carried into this one: it is never overwritten with a list that has lost them. A mark that cannot be read is an error.
func (k *windowKeeper) writeMark(ws []overlayWin) error {
	l, ok := k.m.overlayMarkLock(k.i)
	if !ok {
		return errors.New("遮罩标记的锁拿不到")
	}
	defer l.Unlock()
	cur, found, err := k.m.readMarkStrict(k.i)
	if err != nil {
		return err
	}
	if found && !k.ownMark(cur) {
		k.mu.Lock()
		for _, w := range cur.Windows {
			if !slices.ContainsFunc(k.carried, func(o overlayWin) bool { return o.H == w.H }) {
				k.carried = append(k.carried, w)
			}
		}
		k.mu.Unlock()
	}
	list := slices.Clone(ws)
	k.mu.Lock()
	q := k.qemu
	for _, w := range k.carried {
		if !slices.ContainsFunc(list, func(o overlayWin) bool { return o.H == w.H }) {
			list = append(list, w)
		}
	}
	k.mu.Unlock()
	return k.m.writeOverlayMark(k.i, overlayMark{Round: k.round, OverlayPID: k.self.PID, OverlayCreated: k.self.Created,
		QemuPID: q.PID, QemuCreated: q.Created, Windows: list})
}

// finishMark makes the mark say what is held and carried: deleted when nothing is, rewritten otherwise; only a mark of this
// keeper.
func (k *windowKeeper) finishMark() {
	l, ok := k.m.overlayMarkLock(k.i)
	if !ok {
		k.mu.Lock()
		k.markPending = true
		k.mu.Unlock()
		return
	}
	defer l.Unlock()
	cur, exists := k.m.readOverlayMark(k.i)
	if !exists || !k.ownMark(cur) {
		return
	}
	left := k.markList()
	pending := false
	if len(left) == 0 {
		pending = k.m.removeOverlayMark(k.i) != nil
	} else if len(left) != len(cur.Windows) {
		cur.Windows = left
		if err := k.m.writeOverlayMark(k.i, cur); err != nil {
			k.log.Debugf("实例 %d 遮罩标记改不了: %v", k.i, err)
			pending = true
		}
	}
	k.mu.Lock()
	k.markPending = pending
	k.mu.Unlock()
}

// restoreHeld enables what is held and carried (the ones that answer), drops the rest the way restoreWindows does and
// fixes the mark.
func (k *windowKeeper) restoreHeld(why string) {
	ws := k.markList()
	left, n := restoreWindows(k.ops, ws, overlayEnableDeadline)
	k.mu.Lock()
	held := map[uintptr]overlayWin{}
	var carried []overlayWin
	for _, w := range left {
		if _, ok := k.held[uintptr(w.H)]; ok {
			held[uintptr(w.H)] = w
		} else {
			carried = append(carried, w)
		}
	}
	k.held, k.carried = held, carried
	sig := ""
	for _, w := range left {
		sig += fmt.Sprintf("%d,", w.H)
	}
	changed := sig != k.lastLeft
	k.lastLeft = sig
	k.mu.Unlock()
	k.finishMark()
	if n > 0 {
		k.log.Infof("实例 %d 开机遮罩把 %d 个模拟器窗口恢复成能操作（%s）", k.i, n, why)
	}
	if len(left) > 0 && changed {
		k.log.Warnf("实例 %d 开机遮罩有 %d 个模拟器窗口没应答，没能恢复成能操作（标记留着，稍后再试）", k.i, len(left))
	}
}

// restoreCarried gives back only what an earlier overlay left (not what this keeper holds).
func (k *windowKeeper) restoreCarried() {
	k.mu.Lock()
	ws := slices.Clone(k.carried)
	k.mu.Unlock()
	if len(ws) == 0 {
		return
	}
	left, n := restoreWindows(k.ops, ws, overlayEnableDeadline)
	k.mu.Lock()
	k.carried = left
	k.mu.Unlock()
	k.finishMark()
	if n > 0 {
		k.log.Infof("实例 %d 开机遮罩把上一个遮罩留下的 %d 个模拟器窗口恢复成能操作", k.i, n)
	}
}

// ready: the keeper is sure enough of everything to disable windows now; false: not yet, or never in this run (the cover
// goes on keeping the mouse out).
func (k *windowKeeper) ready(qemu host.Process) bool {
	if k.self.PID == 0 || k.self.Created == 0 {
		k.warnOnce("self", "实例 %d 读不到遮罩进程自己的创建时间，不禁用模拟器窗口（遮罩照样挡住鼠标，键盘没挡）", k.i)
		return false
	}
	if qemu.PID == 0 || qemu.Created == 0 {
		if qemu.PID != 0 { // found, but its start time cannot be read (PID 0: not found yet, nothing to say)
			k.warnOnce("qemu", "实例 %d 读不到模拟器进程的创建时间，不禁用模拟器窗口（遮罩照样挡住鼠标，键盘没挡）", k.i)
		}
		return false
	}
	id := qemuID{qemu.PID, qemu.Created}
	k.mu.Lock()
	if k.guardSeen != id { // another qemu (the instance was started again): its boot watch is waited for from now
		k.guardSeen, k.guardFor, k.firstWant, k.gaveUp = id, qemuID{}, time.Time{}, false
	}
	gaveUp, ok := k.gaveUp, k.guardFor == id
	first := k.firstWant
	if first.IsZero() {
		k.firstWant = time.Now()
		first = k.firstWant
	}
	k.mu.Unlock()
	if gaveUp {
		return false
	}
	if ok || k.guardian == nil {
		return true
	}
	if k.guardian(qemu) {
		k.mu.Lock()
		if k.guardSeen == id {
			k.guardFor = id
		}
		k.mu.Unlock()
		return true
	}
	if time.Since(first) > overlayGuardianWait {
		k.mu.Lock()
		k.gaveUp = true
		k.mu.Unlock()
		k.warnOnce(fmt.Sprintf("guardian-%d-%d", qemu.PID, qemu.Created), "实例 %d 的开机守护 %v 内没就位，不禁用模拟器窗口（遮罩照样挡住鼠标，键盘没挡）", k.i, overlayGuardianWait)
	}
	return false
}

// step is one turn of the worker.
func (k *windowKeeper) step() {
	k.stepMu.Lock()
	defer k.stepMu.Unlock()
	k.mu.Lock()
	released, decided, qemu, pending := k.released, k.decided, k.qemu, k.markPending
	covered := slices.Clone(k.covered)
	nCarried := len(k.carried)
	k.mu.Unlock()
	if released {
		if len(k.heldList()) > 0 || nCarried > 0 {
			k.restoreHeld("release")
		} else if pending {
			k.finishMark()
		}
		return
	}
	if k.stopped.Load() || !decided || !k.ready(qemu) {
		return
	}
	// what an earlier overlay could not give back is given back first
	if nCarried > 0 {
		k.restoreCarried()
	}
	// a window Qt enabled again is disabled again; one that is not the window it was is dropped
	dropped := false
	for _, w := range k.heldList() {
		if k.mustNotDisable() {
			return
		}
		if w.stateOf(k.ops) == windowOurs && !k.ops.Enabled(uintptr(w.H)) {
			continue
		}
		_, _, skipped := setEnabledChecked(k.ops, w, false, overlayEnableDeadline)
		if skipped {
			k.mu.Lock()
			delete(k.held, uintptr(w.H))
			k.mu.Unlock()
			dropped = true
		}
	}
	// windows that take input and have a cover now: named in the mark first, then disabled
	var fresh []overlayWin
	k.mu.Lock()
	for _, h := range covered {
		if _, ok := k.held[h]; !ok {
			fresh = append(fresh, overlayWin{H: uint64(h)})
		}
	}
	k.mu.Unlock()
	fresh = slices.DeleteFunc(fresh, func(w overlayWin) bool {
		pid, class := k.ops.Identity(uintptr(w.H))
		if pid != qemu.PID || class == "" || !k.ops.Enabled(uintptr(w.H)) { // not the qemu's, or disabled before us: left as it is
			return true
		}
		return false
	})
	for n := range fresh {
		_, class := k.ops.Identity(uintptr(fresh[n].H))
		fresh[n].QemuPID, fresh[n].QemuCreated, fresh[n].Class = qemu.PID, qemu.Created, class
	}
	if len(fresh) == 0 {
		if dropped {
			k.finishMark()
		}
		return
	}
	if k.mustNotDisable() {
		return
	}
	all := append(k.heldList(), fresh...)
	if err := k.writeMark(all); err != nil {
		k.warnOnce("mark", "实例 %d 记不下遮罩禁用的窗口（%v）：这次不禁用模拟器窗口（遮罩照样挡住鼠标，键盘没挡）", k.i, err)
		return
	}
	k.mu.Lock()
	for _, w := range fresh {
		k.held[uintptr(w.H)] = w
	}
	k.mu.Unlock()
	k.restoreCarried() // the mark just carried what an earlier overlay could not give back: it is given back now
	failed := false
	for _, w := range fresh {
		if k.mustNotDisable() {
			return // they are in the mark and held: Close enables them (a window that was never disabled stays as it is)
		}
		ok, timedOut, skipped := setEnabledChecked(k.ops, w, false, overlayEnableDeadline)
		switch {
		case timedOut:
			k.log.Warnf("实例 %d 的模拟器窗口 %#x 没在 %v 内应答（EnableWindow），按已禁用对待，会照常恢复", k.i, w.H, overlayEnableDeadline)
		case skipped || !ok:
			k.mu.Lock()
			delete(k.held, uintptr(w.H))
			k.mu.Unlock()
			failed = true
		}
	}
	if failed {
		k.finishMark()
	}
}

// ---- starting it ---------------------------------------------------------------------------------------------------------

// newRound is a fresh round token (see the top of this file).
func newRound() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// spawnBootOverlay starts the overlay process of instance i for the start of round `round` running in this process (a
// variable for the tests): `<exe> __bootoverlay <root> <i> <start pid> <start created> <round>`, detached. Only on Windows.
var spawnBootOverlay = func(m *Manager, i int, round string) (host.Process, error) {
	exe, err := os.Executable()
	if err != nil {
		return host.Process{}, err
	}
	me, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok {
		return host.Process{}, errors.New("读不出本进程的创建时间")
	}
	logPath := filepath.Join(m.Root.LogsDir(), overlayLogName)
	rotateLog(logPath, bootWatchLogMax)
	return host.StartOrphan(exe, []string{BootOverlayArg, m.Root.Dir, strconv.Itoa(i), strconv.FormatUint(uint64(me.PID), 10),
		strconv.FormatUint(me.Created, 10), round}, host.StartOptions{OutputPath: logPath})
}

const overlayLogName = "modavd-bootoverlay.log"

// overlayHandle is the start's side of its overlay: the guard that gives the windows back as soon as the overlay's process
// is gone, and the check, when the start is done, that the overlay has given them back. Bound to the start's round.
type overlayHandle struct {
	m     *Manager
	i     int
	round string
	proc  host.Process
	stop  chan struct{}
}

// startBootOverlay starts the overlay for the windowed boot this start (round) is running and the guard that goes with
// it (layer 2). nil when there is none to start (not Windows, or it could not be started: a warning, the boot goes on
// without the cover). Not for a headless instance: the caller checks that.
func (m *Manager) startBootOverlay(i int, round string) *overlayHandle {
	if overlayOS == nil {
		return nil
	}
	p, err := spawnBootOverlay(m, i, round)
	if err != nil {
		m.Log.Warnf("实例 %d 开机画面没起来（开机照常进行，只是没有遮罩）: %v", i, err)
		return nil
	}
	h := &overlayHandle{m: m, i: i, round: round, proc: p, stop: make(chan struct{})}
	exited := make(chan struct{})
	go func() {
		p.Wait(6 * time.Hour) // a handle to the overlay's process: returns the moment it ends, however it ends
		close(exited)
	}()
	go func() {
		select {
		case <-h.stop:
			return
		case <-exited:
		}
		// gone (a normal end finds no mark of this round: nothing to do); windows that did not answer are tried again
		for {
			if _, done := m.recoverOverlayMark(i, round, overlayOS); done {
				return
			}
			select {
			case <-h.stop:
				return
			case <-time.After(overlayGuardRetry):
			}
		}
	}()
	return h
}

// confirm is for a start that is done: waits (at most d) until the overlay has given the windows back (its mark of this
// round is gone) or has ended. An overlay that is still holding them after that is ended and the windows are given back
// here. Never for a start that failed (the overlay itself gives back when the start lock is released).
func (h *overlayHandle) confirm(d time.Duration) {
	if h == nil {
		return
	}
	deadline := time.Now().Add(d)
	for {
		if !h.proc.Alive() {
			h.m.recoverOverlayMark(h.i, h.round, overlayOS)
			return
		}
		if k, ok := h.m.readOverlayMark(h.i); !ok || k.Round != h.round {
			return // given back (or never taken)
		}
		if !time.Now().Before(deadline) {
			h.m.Log.Warnf("实例 %d 的开机遮罩在 %v 内没放开模拟器窗口，结束它并恢复窗口", h.i, d)
			h.proc.Kill()
			h.proc.Wait(time.Second)
			h.m.recoverOverlayMark(h.i, h.round, overlayOS)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// release ends the guard (the start is over; the overlay ends itself).
func (h *overlayHandle) release() {
	if h != nil {
		select {
		case <-h.stop:
		default:
			close(h.stop)
		}
	}
}

// ---- what to show and when to end -----------------------------------------------------------------------------------

// overlayIn is what one look of the overlay finds out.
type overlayIn struct {
	Now        time.Time
	QemuFound  bool
	Tracked    bool // this round's setup marker or record is there: some start is doing this round's setup
	Answered   bool // adb answered the getprop
	Booted     bool
	StartHeld  bool
	WatchAlive bool
	Setup      bootSetupState // only meaningful once Booted
}

// overlayOut is what the overlay does about it.
type overlayOut struct {
	Exit    bool
	Reason  string  // why it exits: ready, cap, qemu-gone, qemu-never, start-gone, watch-gone
	Release bool    // the emulator windows are given back now (the instance is ready, or the cap passed)
	Note    string  // the step that was not finished, while the cover stays up with its note
	Percent float64 // the bar, 0 to 1; negative: no real percentage yet (the bar is indeterminate)
	Stage   string  // launch, android, or the setup step
	Decided bool    // the poller has looked at the instance at least once (nothing is disabled before)
}

// overlayMachine decides, look after look. Not safe for concurrent use.
type overlayMachine struct {
	started      time.Time
	qemuSeen     bool
	qemuMissing  time.Time // since when the qemu has not been found (zero: it was)
	noStart      time.Time // since when no start holds the lock (zero: one does)
	releasedAt   time.Time // when the cap put the windows back
	releasedStep string
	trackedSeen  bool   // the round was seen tracked once
	lastStage    string // the stage of the last look (what the hard cap names)
}

// overlayTrackGrace: a round that is not seen tracked (no marker, no record of it) within this long is not one anybody is
// setting up: the overlay ends without having disabled anything.
const overlayTrackGrace = 2 * time.Second

// overlayHardCap is the cap that does not depend on adb or on the setup record: counted from the overlay's start, the
// longest a start waits for Android (DefaultBootTimeout) plus the setup's own cap. The record's deadline (bootSetupOf)
// still decides earlier when the setup is under way. A variable for tests.
var overlayHardCap = func() time.Duration { return DefaultBootTimeout + bootSetupCap }

func newOverlayMachine(now time.Time) *overlayMachine { return &overlayMachine{started: now} }

// Next is the decision for the look in.
func (om *overlayMachine) Next(in overlayIn) overlayOut {
	out := overlayOut{Stage: "launch", Decided: true}
	switch {
	case !in.WatchAlive:
		return overlayOut{Exit: true, Reason: "watch-gone"}
	case in.QemuFound:
		om.qemuSeen, om.qemuMissing = true, time.Time{}
	case om.qemuSeen:
		if om.qemuMissing.IsZero() {
			om.qemuMissing = in.Now
		}
		if in.Now.Sub(om.qemuMissing) >= overlayQemuGrace {
			return overlayOut{Exit: true, Reason: "qemu-gone"}
		}
	case in.Now.Sub(om.started) >= overlayStartLimit:
		return overlayOut{Exit: true, Reason: "qemu-never"}
	}
	if in.StartHeld {
		om.noStart = time.Time{}
	} else {
		if om.noStart.IsZero() {
			om.noStart = in.Now
		}
		if in.Now.Sub(om.noStart) >= overlayNoStartGrace {
			return overlayOut{Exit: true, Reason: "start-gone"}
		}
	}
	// Nothing is decided (nothing disabled) before this round is seen tracked: a marker or record of the round says some
	// start is setting it up. Reading none is not "done", and not "tracked" either.
	if in.Tracked {
		om.trackedSeen = true
	}
	if !om.trackedSeen {
		if in.Now.Sub(om.started) >= overlayTrackGrace {
			return overlayOut{Exit: true, Reason: "untracked"}
		}
		return overlayOut{Stage: "launch", Percent: -1}
	}
	if !om.releasedAt.IsZero() { // the cap passed: the note stays up for a while, then the cover goes
		out.Release, out.Note, out.Percent, out.Stage = true, om.releasedStep, 1, "sdcard"
		if in.Now.Sub(om.releasedAt) >= overlayCapNote {
			out.Exit, out.Reason = true, "cap"
		}
		return out
	}
	switch {
	case in.Booted && in.Setup.Ready && in.Setup.Unfinished != "":
		om.releasedAt, om.releasedStep = in.Now, in.Setup.Unfinished
		out.Release, out.Note, out.Percent, out.Stage = true, in.Setup.Unfinished, 1, in.Setup.Unfinished
	case !(in.Booted && in.Setup.Ready) && in.Now.Sub(om.started) >= overlayHardCap():
		// the cap from the overlay's own start, whatever adb says or does not: the windows come back
		step := orText(om.lastStage, readyStepBoot)
		om.releasedAt, om.releasedStep = in.Now, step
		out.Release, out.Note, out.Percent, out.Stage = true, step, 1, step
	case in.Booted && in.Setup.Ready:
		return overlayOut{Exit: true, Release: true, Reason: "ready", Percent: 1}
	case in.Booted && in.Setup.Step != "" && in.Setup.Step != readyStepBoot:
		// the setup says how far it is: the bar is that percentage
		out.Percent, out.Stage = float64(min(max(in.Setup.Progress, 0), 100))/100, in.Setup.Step
	case in.Booted:
		out.Percent, out.Stage = -1, readyStepBoot // no record yet: nothing reliable to show
	case in.Answered:
		out.Percent, out.Stage = -1, "android"
	default:
		out.Percent, out.Stage = -1, "launch"
	}
	om.lastStage = out.Stage
	return out
}

// overlayTitle is the main line of the cover for a stage, in the manager's language.
func overlayTitle(stage string) string {
	var s string
	switch stage {
	case "launch":
		s = "正在启动模拟器…"
	case "android":
		s = "正在等安卓开机…"
	case "vulkan_fix":
		s = "正在挂上客体库补丁…"
	case "ime":
		s = "正在设置输入法…"
	case "first_boot":
		s = "正在做首次初始化…"
	case readyStepSDCard:
		s = "正在等存储就绪…"
	case readyStepBoot:
		s = "正在收尾…"
	default:
		s = "正在做开机后调优…"
	}
	return i18n.T(s)
}

// overlayNote is the line for a setup that did not finish within the cap, naming its step.
func overlayNote(step string) string {
	return i18n.T(fmt.Sprintf("「%s」还没做完，先放你进去，它会接着做", readyStepText(step)))
}
