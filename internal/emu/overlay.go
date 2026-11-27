package emu

// The boot screen (ModAVD/ModAVD#259): while a window-mode instance boots and its after-boot setup runs, a cover lies
// over the emulator's window from the moment the window shows until the instance reads online (bootready.go), and the
// emulator's windows take no mouse and keyboard input. Windows only; the picture is internal/bootscreen.
//
// Who starts it, who ends it. The start that launches a windowed qemu (Start, from the command line or the manager)
// starts a small process of this same executable: `<exe> __bootoverlay <root> <i> <start pid> <start created>`
// (RunBootOverlay), detached like the boot watch. Everything the cover shows it reads from disk and from the guest
// the way status does (the setup record, the start lock, one getprop), so it has no channel to the start, and a cover
// started by anyone looks the same. It ends itself, restoring the windows first, when
//   - the instance is online: Android has booted and the setup is done (bootSetupOf says ready). The cap (the setup
//     went on past bootSetupCap) puts the windows back at once and leaves the cover up three more seconds with a line
//     saying which step was not finished;
//   - the qemu it covers is gone, or never showed up in 30 s;
//   - no start holds the start lock any more for 1.5 s (the start was cancelled, failed or killed, or the manager that
//     ran it exited: the lock goes with the process);
//   - the process that started it is gone (the same, one more way to see it);
//   - it is told to end (Ctrl+C, a close, the session ends).
//
// What keeps input out. A cover is an owned pop-up over the picture area of the main window and over the side
// toolbar, so mouse input lands on the cover (internal/win cover_windows.go); the keyboard cannot be covered, so the
// emulator's windows are also disabled (EnableWindow FALSE) like under a modal dialog, which is state of the other
// process's windows and has to be given back whatever happens to the cover. Four layers see to that:
//  1. the overlay restores them before it exits, whatever the reason (a deferred restore, also on a panic, a signal,
//     the end of the session);
//  2. the start that started it watches the overlay's process every 500 ms and restores them when it is gone however it
//     went (guardOverlay): at most about a second;
//  3. the overlay watches the start (above): a killed or cancelled start ends the overlay and the windows come back
//     within 1.5 s;
//  4. when both are gone at once the overlay's mark on disk (.mas_<i>.overlay: its process, the qemu, the windows it
//     disabled) is found by the next modavd that looks at the instance (status / list / info, start, stop, window:
//     RecoverOverlay), which restores them and deletes the mark. The manager polls status every few seconds when it is
//     open; with nothing open it is the next command that does it.
// A qemu that is gone takes its windows with it, so nothing is left to restore then.
//
// This file is the part that does not depend on the platform: the mark, the recovery, the guard, what to show and when
// to end. The windows are in overlay_windows.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"modavd/internal/host"
	"modavd/internal/i18n"
	"modavd/internal/root"
)

// BootOverlayArg is the hidden subcommand both executables route to RunBootOverlay.
const BootOverlayArg = "__bootoverlay"

var (
	// overlayCapNote: how long the cover stays up with its note after the cap put the windows back.
	overlayCapNote = 3 * time.Second
)

const (
	// overlayStartLimit: the overlay ends when its qemu has not shown up this long after it started.
	overlayStartLimit = 30 * time.Second
	// overlayNoStartGrace: how long no start holding the start lock is put up with before the overlay ends.
	overlayNoStartGrace = 1500 * time.Millisecond
	// overlayQemuGrace: a process scan that misses the qemu for this long means it is gone.
	overlayQemuGrace = 2 * time.Second
	// overlayGuardEvery: how often the start looks whether its overlay process is still there.
	overlayGuardEvery = 500 * time.Millisecond
)

// overlayWindows is what the overlay needs of the emulator's windows; Windows has the real one (overlay_windows.go),
// the tests fake it.
type overlayWindows interface {
	// Windows are the main window and the side toolbar of emulator process pid.
	Windows(pid uint32) ([]uintptr, error)
	// IsToolbar: the window is the side toolbar (its cover is plain, the screen goes on the main window).
	IsToolbar(h uintptr) bool
	// SetEnabled enables or disables h and reports whether it is in that state afterwards.
	SetEnabled(h uintptr, on bool) bool
	Enabled(h uintptr) bool
	// ProcessOf is the process that owns h, 0 when the window is gone.
	ProcessOf(h uintptr) uint32
}

// overlayOS is the platform's window operations; nil where there are none.
var overlayOS overlayWindows

// overlayMark is the content of overlayMarkPath: the overlay's process (PID + creation time), the qemu it covers and
// the windows it disabled.
type overlayMark struct {
	OverlayPID     uint32   `json:"overlay_pid"`
	OverlayCreated uint64   `json:"overlay_created"`
	QemuPID        uint32   `json:"qemu_pid"`
	QemuCreated    uint64   `json:"qemu_created"`
	Windows        []uint64 `json:"windows"`
}

func (k overlayMark) overlayAlive() bool {
	return k.OverlayPID != 0 && (host.Process{PID: k.OverlayPID, Created: k.OverlayCreated}).Alive()
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

func (m *Manager) writeOverlayMark(i int, k overlayMark) error {
	b, err := json.Marshal(k)
	if err == nil {
		err = writeFileReplace(m.overlayMarkPath(i), b, strconv.Itoa(os.Getpid()))
	}
	return err
}

func (m *Manager) removeOverlayMark(i int) {
	if err := os.Remove(m.overlayMarkPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.Log.Debugf("实例 %d 删不掉遮罩标记: %v", i, err)
	}
}

// RecoverOverlay gives the emulator windows of instance i back when an overlay disabled them and is gone (layers 2 and
// 4 above): it enables the windows the mark names, as long as they still belong to that qemu, and deletes the mark.
// Nothing happens while the overlay lives. It reports whether it put windows back. Cheap when there is no mark.
func (m *Manager) RecoverOverlay(i int) bool {
	k, ok := m.readOverlayMark(i)
	if !ok || k.overlayAlive() {
		return false
	}
	return m.restoreOverlayMark(i, k, overlayOS)
}

// restoreOverlayMark enables the windows of mark k and deletes it.
func (m *Manager) restoreOverlayMark(i int, k overlayMark, ops overlayWindows) bool {
	restored := 0
	qemuUp := k.QemuPID != 0 && (host.Process{PID: k.QemuPID, Created: k.QemuCreated}).Alive()
	if ops != nil && qemuUp {
		for _, h := range k.Windows {
			// a handle that was reused by another process's window is not ours to enable
			if ops.ProcessOf(uintptr(h)) == k.QemuPID && ops.SetEnabled(uintptr(h), true) {
				restored++
			}
		}
	}
	m.removeOverlayMark(i)
	if restored > 0 {
		m.Log.Infof("实例 %d 的遮罩没了（进程 %d），已把模拟器的 %d 个窗口恢复成能操作", i, k.OverlayPID, restored)
	}
	return restored > 0
}

// spawnBootOverlay starts the overlay process of instance i for the start running in this process (a variable for the
// tests): `<exe> __bootoverlay <root> <i> <start pid> <start created>`, detached. Only on Windows.
var spawnBootOverlay = func(m *Manager, i int) (host.Process, error) {
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
		strconv.FormatUint(me.Created, 10)}, host.StartOptions{OutputPath: logPath})
}

const overlayLogName = "modavd-bootoverlay.log"

// overlayHandle is the start's side of its overlay: the guard that gives the windows back when the overlay's process is
// gone.
type overlayHandle struct {
	stop chan struct{}
}

// startBootOverlay starts the overlay for the windowed boot this start is running and the guard that goes with it
// (layer 2). nil when there is none to start (not Windows, or it could not be started: a warning, the boot goes on
// without the cover). Not for a headless instance: the caller checks that.
func (m *Manager) startBootOverlay(i int) *overlayHandle {
	if overlayOS == nil {
		return nil
	}
	p, err := spawnBootOverlay(m, i)
	if err != nil {
		m.Log.Warnf("实例 %d 开机画面没起来（开机照常进行，只是没有遮罩）: %v", i, err)
		return nil
	}
	h := &overlayHandle{stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(overlayGuardEvery)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				if !p.Alive() {
					m.RecoverOverlay(i)
					return
				}
			}
		}
	}()
	return h
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
	Answered   bool // adb answered the getprop
	Booted     bool
	StartHeld  bool
	WatchAlive bool
	Setup      bootSetupState // only meaningful once Booted
}

// overlayOut is what the overlay does about it.
type overlayOut struct {
	Exit    bool
	Reason  string // why it exits: ready, cap, qemu-gone, qemu-never, start-gone, watch-gone
	Release bool   // the emulator windows are given back now (the instance is ready, or the cap passed)
	Note    string // the step that was not finished, while the cover stays up with its note
	Percent float64
	Stage   string // launch, android, or the setup step
}

// overlayMachine decides, look after look. Not safe for concurrent use.
type overlayMachine struct {
	started      time.Time
	qemuSeen     bool
	qemuMissing  time.Time // since when the qemu has not been found (zero: it was)
	noStart      time.Time // since when no start holds the lock (zero: one does)
	answeredAt   time.Time // when adb first answered
	releasedAt   time.Time // when the cap put the windows back
	releasedStep string
}

func newOverlayMachine(now time.Time) *overlayMachine { return &overlayMachine{started: now} }

// Next is the decision for the look in.
func (om *overlayMachine) Next(in overlayIn) overlayOut {
	out := overlayOut{Stage: "launch"}
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
	if !om.releasedAt.IsZero() { // the cap passed: the note stays up for a while, then the cover goes
		out.Release, out.Note, out.Percent, out.Stage = true, om.releasedStep, 1, "sdcard"
		if in.Now.Sub(om.releasedAt) >= overlayCapNote {
			out.Exit, out.Reason = true, "cap"
		}
		return out
	}
	if in.Answered && om.answeredAt.IsZero() {
		om.answeredAt = in.Now
	}
	switch {
	case in.Booted && in.Setup.Ready && in.Setup.Unfinished != "":
		om.releasedAt, om.releasedStep = in.Now, in.Setup.Unfinished
		out.Release, out.Note, out.Percent, out.Stage = true, in.Setup.Unfinished, 1, in.Setup.Unfinished
	case in.Booted && in.Setup.Ready:
		return overlayOut{Exit: true, Release: true, Reason: "ready", Percent: 1}
	case in.Booted:
		step := orText(in.Setup.Step, readyStepBoot)
		out.Percent, out.Stage = overlaySetupFraction(in.Setup.Progress), step
	case in.Answered:
		out.Percent, out.Stage = overlayAndroidFraction(in.Now.Sub(om.answeredAt)), "android"
	case in.QemuFound:
		out.Percent, out.Stage = 0.08, "launch"
	default:
		out.Percent = 0.03
	}
	return out
}

// The bar's scale: the launch, Android's boot (from adb answering to sys.boot_completed), then the setup's own
// percentage. Real signals, not time, except inside the Android stage where nothing but time is known between the two
// observations: it creeps from 0.30 toward 0.68.
const (
	overlayAndroidFrom = 0.30
	overlayAndroidTo   = 0.68
	overlaySetupFrom   = 0.70
)

func overlayAndroidFraction(sinceAnswered time.Duration) float64 {
	k := 1 - 1/(1+sinceAnswered.Seconds()/6)
	return overlayAndroidFrom + (overlayAndroidTo-overlayAndroidFrom)*k
}

func overlaySetupFraction(percent int) float64 {
	return overlaySetupFrom + (1-overlaySetupFrom)*float64(min(max(percent, 0), 100))/100
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
