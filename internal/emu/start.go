package emu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// Progress values: one scale for the whole call so they only ever grow (MCP requires strictly increasing progress).
// The boot wait spreads over bootProgressFrom..bootProgressTo by elapsed time.
const (
	progressTotal      = 1000
	progressLaunch     = 50
	bootProgressFrom   = 100
	bootProgressTo     = 600
	progressBooted     = 610
	progressVulkanFix  = 615
	progressTune       = 620
	progressIme        = 700
	progressFirstBoot  = 800
	bootReportInterval = 5 * time.Second
	// how long a start waits for a boot lock nobody should hold (the start lock is ours)
	bootLockWait = 10 * time.Second
)

// progressor sends Progress to the caller with Done strictly increasing.
type progressor struct {
	fn   func(Progress)
	last int
	// ready, once the setup of the boot has begun (Start), is told which step of it is running (readyTracker);
	// lazyReady starts it for a take-over at the first real work (needReady)
	ready     *readyTracker
	lazyReady func() *readyTracker
	lastStep  string // the last of those steps that was emitted
}

// readyPercent tells the setup's record that done of total after-boot steps are done.
func (p *progressor) readyPercent(done, total int) {
	if p != nil {
		p.ready.percent(done, total)
	}
}

func (p *progressor) emit(step, msg string, done int) {
	if p == nil {
		return
	}
	switch step {
	case "vulkan_fix", "tune", "ime", "first_boot":
		p.lastStep = step
		p.ready.step(step)
	}
	if p.fn == nil {
		return
	}
	if done <= p.last {
		done = p.last + 1
	}
	if step != "done" && done >= progressTotal {
		done = progressTotal - 1
	}
	if done > progressTotal {
		done = progressTotal
	}
	p.last = done
	p.fn(Progress{Step: step, Message: msg, Done: done, Total: progressTotal})
}

// startFindQemu is how Start looks for the instance's qemu (m.findQemu, always the real scan); a test replaces it to
// have Start find one running.
var startFindQemu = (*Manager).findQemu

// Start boots instance i (or adopts it when already running), runs the per-boot setup and, once, the first-boot
// setup, and returns when the instance is online. See StartOptions / StartResult.
//
// Port of AUTO-MAS AvdManager.open (without launching an app): one start per instance at a time, and a start waits
// for a stop of the same instance to finish first (otherwise it would take an instance that is shutting down for a
// running one). A guest that is powering off without a stop of ours (the window's ×, AUTO-MAS's own close) is waited
// for the same way: its qemu has to exit first, then the instance boots again (see guestShuttingDown). Already
// running: no launch; the boot wait and the per-boot steps run again, the guest library fixes are only checked
// (mounting them restarts zygote and would end the apps running on it). On cancel the emulator is left running: ending qemu without
// a guest shutdown is a power cut.
//
// Unlike AUTO-MAS (one long-running process), a start here can end before its setup is done: an MCP call cancelled,
// a boot slower than the timeout. A start that launches qemu therefore leaves the setup marker (setupMarkerPath) on
// disk until the whole after-boot setup has run; a later start that adopts that same qemu process before the marker's
// deadline (setupMarkerWindow after qemu started: the launching start's own boot timeout counts) does the setup as on
// a fresh boot, and the error of the interrupted start names that deadline. The guest may be in use by then all the
// same (AUTO-MAS adopted it and launched a game, an MCP client went on with shell / install_apk, a CLI user pressed
// Ctrl+C on a slow start --window and started playing), so that start mounts the guest library fixes (a zygote
// restart, ending every app) only while no app is in the foreground and no install holds the guest lock; otherwise it leaves the
// marker, warns and reports SetupPending.
//
// A start that finishes records the guest boot it set up and leaves a boot watch on its qemu (bootwatch.go): when
// the guest reboots on its own (qemu stays), the watch runs this start again, and so does any start that finds the
// recorded boot_id changed. The per-boot steps then run again and the guest library fixes are owed as above.
//
// With opt.Launch, the app is opened once all of that is done (launchapp.go). The name is checked first, so a bad one
// boots nothing; an app that cannot be opened leaves the instance up and is reported in StartResult.App.
func (m *Manager) Start(ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
	if opt.Launch != nil {
		if err := opt.Launch.Validate(); err != nil {
			return nil, err
		}
	}
	res, err := startBootedStep(m, ctx, i, opt)
	if err != nil || opt.Launch == nil || res == nil {
		return res, err
	}
	res.App = m.openApp(ctx, i, *opt.Launch)
	return res, nil
}

// startBootedStep is the boot part of Start; a test replaces it.
var startBootedStep = (*Manager).startBooted

// startBooted is Start without opt.Launch.
func (m *Manager) startBooted(ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
	if err := requireLifecycle(); err != nil {
		return nil, err
	}
	t0 := time.Now()
	inst, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	m.RecoverOverlay(i)
	if opt.creating && m.createCanceled(i) {
		return nil, errs.New(errs.Canceled, "新建初始化已被关机打断")
	}
	// A new instance boots only for create's own setup until that is done (createinit.go); the boot watch of that
	// boot's qemu may adopt it again.
	if !opt.creating && opt.watchQemu == nil {
		if err := m.notCreated(i, "不能开机"); err != nil {
			return nil, err
		}
	}
	col := &logx.Collector{}
	mm := m.withLog(col.Sink())
	// The execution bridge (bridge.go): an instance this start launches gets its boot marker first (bridgeMarker).
	bridge := opt.watchQemu == nil && !opt.NoBridge && m.bridgeWanted(i, inst.Config)
	opt.bridgeMarker = bridge
	// A launch that is known to be followed by the bridge's install and cold boots only prepares the instance: it is
	// headless, and the window comes with the last boot (afterStartBridge).
	opt.launchHeadless = bridgePrepLaunch(opt, bridge, regularFile(bridgeMarker(m.Root, i)))
	res, err := mm.start(ctx, inst, opt, t0)
	if err != nil {
		return nil, err
	}
	res.Warnings = col.Messages()
	return m.afterStartBridge(ctx, i, opt, bridge, res)
}

// bridgePrepLaunch: a launch of this start (opt; bridge: Start wants the execution bridge; marker: its boot marker is
// there already) is headless although the caller asked for a window, because the bridge is surely installed after it,
// with a cold boot or two: the marker is not there yet (the instance never had the bridge: this launch places it), or
// the caller knows (bridgePrep). With the marker there, whether the guest has the bridge is only known once it runs,
// so that launch has the window as asked.
func bridgePrepLaunch(opt StartOptions, bridge, marker bool) bool {
	return bridge && opt.Window && (opt.bridgePrep || !marker)
}

// afterStartBridge is the end of Start (res: its boot, set up): the execution bridge's install or update, and the
// last boot with the window asked for when the instance runs headless after them.
func (m *Manager) afterStartBridge(ctx context.Context, i int, opt StartOptions, bridge bool,
	res *StartResult) (*StartResult, error) {
	var err error
	switch {
	// The execution bridge (bridge.go): its marker is there and the guest still runs the stock translator. After the
	// whole setup, because installing takes a cold boot or two; and only on a boot this start launched, never on a
	// running instance it adopted (someone may be playing on it: the install restarts adbd and boots it cold).
	case bridge && res.Launched && m.bridgePending(ctx, i):
		if opt.bridgeLocked {
			res, err = m.installBridge(ctx, i, opt, res)
		} else {
			res, err = m.finishBridge(ctx, i, opt, res)
		}
	// The bridge is installed: its files are updated when they are not this package's payload (the package was updated
	// since), also only on a boot this start launched. `modavd bridge <i> install` (bridgeLocked) updates by itself.
	case bridge && res.Launched && !opt.bridgeLocked:
		res, err = m.updateBridge(ctx, i, opt, res)
	}
	if err != nil || res == nil {
		return res, err
	}
	// Asked for the window, and the instance runs headless after the prep boots (or after a prep launch with nothing
	// to install after all): one more cold boot, after a normal shutdown, gives it the window. A running instance
	// does not switch (StartOptions.Window), so this is never an adopt.
	if opt.Window && res.Launched && !res.launchWindow {
		m.Log.Infof("ModAVD 实例 %d 准备阶段无头开机已做完，关机后按要求带窗口开机", i)
		r, err := m.bridgeRestart(ctx, i, StartOptions{Window: true, Timeout: opt.Timeout, Progress: opt.Progress,
			creating: opt.creating})
		if err != nil {
			return nil, errs.Wrap(errs.As(err).Code, err, fmt.Sprintf("实例 %d 准备完以后带窗口开机失败: %v", i, err))
		}
		r.Warnings = append(append([]string{}, res.Warnings...), r.Warnings...)
		res = r
	}
	return res, nil
}

func (m *Manager) start(ctx context.Context, inst *avd.Instance, opt StartOptions, t0 time.Time) (*StartResult, error) {
	i := inst.Index
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultBootTimeout
	}
	// how long after qemu started a marker this start writes stays good (a long timeout_s gives a later deadline)
	window := setupMarkerWindow(timeout)
	prog := &progressor{fn: opt.Progress}

	if opt.watchQemu != nil {
		startLock, err := m.watchStartLock(i)
		if err != nil {
			return nil, err
		}
		defer startLock.Unlock()
	} else {
		startLock, err := m.lock(ctx, i, "start", startLockWait, "正由另一次启动开机或做开机后设置")
		if err != nil {
			return nil, err
		}
		defer startLock.Unlock()
		stopLock, err := m.lock(ctx, i, "stop", stopLockWait, "正在关机")
		if err != nil {
			return nil, err
		}
		// Stop may have canceled this creation after Start's entry check. Check
		// under its lock; the cancellation watcher need not have run yet.
		if opt.creating && m.createCanceled(i) {
			stopLock.Unlock()
			return nil, errs.New(errs.Canceled, "新建初始化已被关机打断")
		}
		stopLock.Unlock()
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	var res *StartResult
	var launchedAt time.Time
	// marker: this boot's after-boot setup is owed (this call launched qemu, or adopts the qemu of a start that ended
	// before finishing it); its one-time parts then run as on a fresh boot. nil = an ordinary adopt.
	var marker *setupMarker
	var mode bootSetup
	var qemu host.Process
	earlyRestarted := false // earlyGuestFixes restarted zygote during the boot
	// The boot screen over the emulator's window (overlay.go): started once, by the first point at which this start knows
	// it works on a boot that has a window; the guard that goes with it ends with the start.
	// The round of this attempt is in the setup marker or the setup record before the overlay is started (overlay.go).
	roundID := newRound()
	var overlay *overlayHandle
	defer func() { overlay.release() }()
	watching := false
	// The boot watch of the qemu is the one process that lives as long as it, so the layer that gives the windows back
	// when the overlay and this start are both gone: it is started with the overlay (and again, harmlessly, at the end).
	watchQemu := func(q host.Process) {
		if overlay != nil && !watching && q.PID != 0 && q.Created != 0 {
			watching = true
			m.startBootWatch(i, q)
		}
	}
	// roundPublished: the round is in the setup marker or the record. Without it (the write failed) this round has no boot
	// screen: an overlay could not tell its round from none, and a cover that nothing tracks must not take the keyboard.
	roundPublished := true
	coverWindow := func(windowed bool, q host.Process) {
		if windowed && overlay == nil {
			if !roundPublished {
				m.Log.Warnf("实例 %d 这一轮的标记写不进去，这次不起开机遮罩（开机照常进行）", i)
				return
			}
			overlay = m.startBootOverlay(i, roundID)
		}
		watchQemu(q)
	}
	// owed: errors from here on leave a boot whose setup the next start finishes
	owed := func(err error) error {
		if marker == nil {
			return err
		}
		return withSetupAdvice(err, setupAdvice(marker.until(), time.Now()))
	}
	onQemu := func(q host.Process) {
		watchQemu(q)
		if marker == nil {
			return
		}
		if marker.QemuPID == 0 && q.Created != 0 {
			marker.QemuPID, marker.QemuCreated = q.PID, q.Created
			// only a launched boot gets here (an owed marker has its qemu recorded already): count from qemu itself
			if started := q.StartedAt(); mode == setupLaunched && !started.IsZero() {
				marker.ValidUntil = started.Add(window).Unix()
			}
			marker.AliveMS = readyClockMS()
			m.writeSetupMarker(i, *marker)
		} else if now := readyClockMS(); heartbeatDue(marker.AliveMS, now) {
			// the boot wait says it is alive: what status waits for before the setup's own record exists (bootready.go)
			marker.AliveMS = now
			m.writeSetupMarker(i, *marker)
		}
	}
	// A second round only when the qemu the first one adopted exited before Android was up (errAdoptedGone).
	for round := 1; ; round++ {
		res, launchedAt, marker, mode = &StartResult{}, time.Time{}, nil, setupAdopted
		var emulator, adopted *host.Process
		var bootLock *host.FileLock
		var adoptedAt time.Time
		stopSeen := false // a stop of this instance held its lock while this round waited for an adopted qemu
		p, running, err := startFindQemu(m, i)
		if err != nil {
			return nil, err
		}
		if w := opt.watchQemu; w != nil && (!running || p.PID != w.PID || p.Created != w.Created) {
			// the watched qemu is gone (or replaced): a boot watch never launches an emulator
			return nil, errs.Newf(errs.LaunchFailed, "实例 %d 的模拟器（pid %d）已不在运行", i, w.PID)
		}
		if running && opt.watchQemu == nil && m.guestShuttingDown(ctx, i) {
			var err error
			if p, running, err = m.waitShutdown(ctx, i, p, prog); err != nil {
				return nil, err
			}
		}
		// no switch between window and headless while it runs (StartOptions.Window); when that cannot be told, it is
		// adopted as it runs
		if running && opt.watchQemu == nil {
			if windowed, known := qemuWindowed(p); known {
				if err := windowConflict(i, opt, windowed); err != nil {
					return nil, err
				}
			}
		}
		if running {
			m.Log.Infof("ModAVD 实例 %d 已在运行（pid %d）", i, p.PID)
			prog.emit("launch", fmt.Sprintf("实例 %d 已在运行（pid %d），不重新开机", i, p.PID), progressLaunch)
			adopted, adoptedAt = &p, time.Now()
			mk, ok := m.readSetupMarker(i)
			now := time.Now()
			switch use, stale := setupMarkerUse(mk, ok, p, now); {
			case use:
				m.Log.Infof("实例 %d 这次开机后的设置还没做完（拉起它的那次启动被取消或超时了），这次接着做完", i)
				if mk.QemuPID == 0 {
					// matched by its parent emulator.exe: from now on by qemu itself
					mk.QemuPID, mk.QemuCreated = p.PID, p.Created
				}
				if mk.ValidUntil == 0 {
					// written by an older modavd: keep the deadline it was just judged by, so the messages can name it
					mk.ValidUntil = mk.deadline(p).Unix()
				}
				// this start works on the boot now: what status waits for until the setup's record comes (bootready.go)
				mk.AliveMS = readyClockMS()
				mk.Round = roundID
				roundPublished = m.writeSetupMarker(i, mk) == nil
				marker = &mk
				mode = setupOwed
				windowed, known := qemuWindowed(p)
				coverWindow(known && windowed, p)
				if regularFile(mk.LogFile) {
					res.LogFile = mk.LogFile // exactly this boot's emulator log (the IME channel is read from it)
				}
			case stale:
				// this boot has run long enough to be in use; the interrupted start is long past
				why := "读不出这次开机的时间"
				if d := mk.deadline(p); !d.IsZero() {
					why = "接着做设置的期限 " + clockText(d, now) + " 已过"
				}
				m.Log.Infof("实例 %d 的开机设置标记已过期（%s），按已在运行的实例处理，客体库补丁等下次开机再挂", i, why)
				m.removeSetupMarker(i)
			default:
				// absent, unreadable or naming another process (that boot is over): an ordinary adopt
				m.removeSetupMarker(i)
			}
			if res.LogFile == "" {
				// the output of the emulator that runs it, when it was started into <root>\logs (by modavd or AUTO-MAS)
				res.LogFile = openLogFile(m.Root.LogsDir(), inst.Name+"-", ".log")
			}
		} else {
			// no qemu: a marker left here names a boot that is over
			m.removeSetupMarker(i)
			m.removeReady(i)
			if opt.bridgeMarker {
				// the execution bridge (bridge.go): an instance about to be launched boots writable already, so the
				// install after the setup needs one cold boot fewer
				if err := writeBridgeMarker(m.Root, i); err != nil {
					m.Log.Warnf("实例 %d 没放上执行桥的开机标记: %v", i, err)
				}
			}
			launchWindow := opt.Window && !opt.launchHeadless
			if opt.launchHeadless {
				m.Log.Infof("ModAVD 实例 %d 开机后要装 arm64 执行桥（要冷启动一到两次）：准备阶段无头开机，装好后再带窗口开机", i)
			}
			m.warnStaleScreenshotShm(i)
			plan, err := m.prepareLaunch(ctx, inst, launchWindow)
			if err != nil {
				return nil, err
			}
			if err := ctxErr(ctx); err != nil {
				return nil, err
			}
			// From here until Android has booted no modavd process re-registers it with the private server: the
			// emulator registers itself at the end of its boot (status polls would otherwise wait 5 s and warn).
			if bootLock, err = m.lock(ctx, i, "boot", bootLockWait, "正在开机"); err != nil {
				return nil, err
			}
			m.startDiagnosticArchive(i)
			p, err := m.startEmulator(i, plan)
			if err != nil {
				bootLock.Unlock()
				return nil, err
			}
			launchedAt = time.Now()
			emulator = &p
			res.Launched = true
			res.launchWindow = launchWindow
			mode = setupLaunched
			res.LogFile = plan.logPath
			// written before qemu shows up (a cancel may come first): until then it names qemu by its parent
			// emulator.exe, and the deadline counts from the launch (qemu starts right after it; onQemu moves it to
			// qemu's own start). Before the overlay: the round is published first.
			marker = &setupMarker{EmulatorPID: p.PID, EmulatorCreated: p.Created, LogFile: plan.logPath,
				ValidUntil: launchedAt.Add(window).Unix(), AliveMS: readyClockMS(), Round: roundID}
			roundPublished = m.writeSetupMarker(i, *marker) == nil
			coverWindow(launchWindow, host.Process{})
			prog.emit("launch", fmt.Sprintf("实例 %d 的模拟器已拉起（pid %d），等安卓开机", i, p.PID), progressLaunch)
		}

		// A launched boot of an instance set up before mounts the guest library fixes under the boot animation; the
		// first boot of a new one leaves them to afterBoot (system_server's very first start is not cut short).
		var early func() bool
		if mode == setupLaunched && m.initialized(i) {
			early = func() bool {
				done, restarted := m.earlyGuestFixes(ctx, inst, prog)
				earlyRestarted = earlyRestarted || restarted
				return done
			}
		}
		qemu, err = m.waitBoot(ctx, i, timeout, emulator, adopted, res.LogFile, prog, onQemu, &stopSeen, early)
		bootLock.Unlock()
		if err == nil {
			break
		}
		if relaunchAfterAdoptedGone(round, opt.watchQemu != nil, err, time.Since(adoptedAt), stopSeen) {
			// Most likely a guest that was powering off and no longer answered adb, so guestShuttingDown could not
			// tell: boot the instance as asked. Its setup marker, if any, names a boot that is over now.
			m.Log.Infof("ModAVD 实例 %d 接管的模拟器（pid %d）没等安卓开机完成就退出了（多半是正在关机），重新开机",
				i, adopted.PID)
			continue
		}
		return nil, m.startFailure(ctx, i, owed(err), string(StatusStarting))
	}
	if res.Launched {
		res.BootSeconds = time.Since(launchedAt).Seconds()
		prog.emit("boot", fmt.Sprintf("实例 %d 安卓已开机（%.1f 秒）", i, res.BootSeconds), progressBooted)
	} else {
		prog.emit("boot", fmt.Sprintf("实例 %d 安卓已开机", i), progressBooted)
	}

	// The guest boot this start sets up (bootRecord, written once the setup is done). An adopted qemu whose record
	// names another boot_id has had its guest reboot on its own since a start (or the boot watch) set it up: that
	// boot is owed its setup like one whose start was interrupted, with a marker of its own.
	_, bootID, _ := m.guestBoot(ctx, i, true)
	if mode == setupAdopted {
		if rec, ok := m.readBootRecord(i); ok && rec.guestRebooted(qemu, bootID) {
			m.Log.Infof("实例 %d 的客体在模拟器没退出时自己重启过（boot_id %s → %s），这次开机后的设置按没做处理", i,
				shortBootID(rec.BootID), shortBootID(bootID))
			mode = setupOwed
			windowed, known := qemuWindowed(qemu)
			marker = &setupMarker{QemuPID: qemu.PID, QemuCreated: qemu.Created, LogFile: res.LogFile,
				ValidUntil: time.Now().Add(SetupMarkerGrace).Unix(), AliveMS: readyClockMS(), Round: roundID}
			roundPublished = m.writeSetupMarker(i, *marker) == nil
			// the record of the boot before names this same qemu and is done: it is of another round, and is taken away
			// before the overlay is started anyway
			m.removeReady(i)
			coverWindow(known && windowed, qemu)
		}
	}

	// A boot this start sets up is "starting" for status until that is done (bootready.go); an adopt that owes
	// nothing only checks, and the instance stays online.
	// A take-over that finds it has real work to do (the first-boot setup, installing the host input method) starts
	// the record at that point (progressor.needReady).
	begin := func() *readyTracker { return m.beginReady(i, qemu, bootID, roundID) }
	if mode != setupAdopted || !m.initialized(i) {
		prog.ready = begin()
		if prog.ready != nil && mode == setupAdopted {
			// a take-over that has the first-boot setup to do: the boot screen, after the record (a launched or owed boot has it
			// since before)
			windowed, known := qemuWindowed(qemu)
			coverWindow(known && windowed, qemu)
		}
	} else {
		// A take-over that finds real work to do (first-boot setup, installing the input method) on a boot with a window
		// gets the boot screen too, started after its record (a mere check shows nothing).
		prog.lazyReady = func() *readyTracker {
			t := begin()
			if t != nil {
				windowed, known := qemuWindowed(qemu)
				coverWindow(known && windowed, qemu)
			}
			return t
		}
	}
	defer func() { prog.ready.close() }()

	var owedUntil time.Time // the deadline the deferred-mount warning names
	if marker != nil {
		owedUntil = marker.until()
	}
	ab, err := m.afterBoot(ctx, inst, mode, owedUntil, res.LogFile, prog)
	if err != nil {
		return nil, m.startFailure(ctx, i, owed(err), unknownState)
	}
	res.FirstBoot = ab.FirstBoot
	res.ZygoteRestarted = ab.ZygoteRestarted || earlyRestarted
	res.LogcatFile = ab.LogcatFile
	res.HostIME = ab.HostIME

	// The setup ran on adb alone, whose failures are only warnings: qemu may have gone meanwhile (a stop, which does
	// not wait for a start; a host GPU driver reset). Reporting online with the old PID would send the caller on to a
	// device that is not there; modavd has no watchdog that would correct it later (AUTO-MAS has).
	now, found, err := m.findQemu(i)
	if err != nil {
		return nil, owed(err)
	}
	qemu, gone := qemuAfterSetup(qemu, now, found)
	// The setup is over (done, or its boot is gone), unless a guest library fix was left out because an app was in the
	// foreground or an install was running, or a language request still needs confirmation: the marker stays so
	// a later start of this boot can finish the work.
	if gone || !ab.SetupPending && !ab.LanguagePending {
		m.removeSetupMarker(i)
	}
	if gone {
		return nil, errs.Newf(errs.LaunchFailed, "ModAVD 实例 %d 在开机后设置期间退出了（模拟器 pid %d）：%s", i,
			qemu.PID, orText(logTail(res.LogFile, 8), "日志见 "+m.Root.LogsDir()))
	}
	// Callers write to /sdcard right after start (10-31 measurements): it is not there yet at sys.boot_completed,
	// takes another 1–3 s to accept a new file after it is mounted, and the zygote restart of the guest library fixes
	// takes it
	// away again for about 7 s. Usually ready long before the setup is done, so this costs nothing then.
	prog.ready.sdcard()
	if !m.waitSDCard(ctx, i) {
		if err := ctxErr(ctx); err != nil {
			// cancelled in the middle of it: the setup is not done (no record of that, no boot record) and nobody does it now
			return nil, m.startFailure(ctx, i, owed(err), unknownState)
		}
		m.Log.Warnf("实例 %d 开机后 /sdcard 在 %.0f 秒内一直写不进文件；先返回，稍后再写", i, sdcardMountWait.Seconds())
	}
	// The setup is done: status reports online from here, and the boot record below lets the boot watch register the
	// instance with the default adb server.
	prog.ready.finish()
	res.SetupPending = ab.SetupPending
	res.LanguagePending = ab.LanguagePending
	// The boot read before the setup, so a guest that rebooted during it is set up again by the watch. Unknown (adb
	// failed): the old record stays, which at worst makes the watch set this boot up once more.
	if bootID != "" {
		m.writeBootRecord(i, bootRecord{QemuPID: qemu.PID, QemuCreated: qemu.Created, BootID: bootID})
	}
	if opt.watchQemu == nil {
		m.startBootWatch(i, qemu)
	}
	// the setup is recorded as done: the boot screen gives the windows back within a look; not left to chance when this
	// start is about to return
	overlay.confirm(overlayConfirmWait)

	// the first-boot setup changed mas-avd.json (initialized, launcher, renderer): build the result from disk again
	if fresh, err := m.instance(i); err == nil {
		inst = fresh
	}
	booted := true
	res.Instance = m.instanceInfo(ctx, inst, &qemu, &booted)
	if res.Instance.Busy == "starting" {
		// that start lock is ours, and this start is done
		res.Instance.Busy = ""
	}
	res.TotalSeconds = time.Since(t0).Seconds()
	prog.emit("done", fmt.Sprintf("实例 %d 已就绪（共 %.1f 秒）", i, res.TotalSeconds), progressTotal)
	return res, nil
}

// waitSDCard waits up to sdcardMountWait until a new file can be created in /sdcard (and removed again). Being
// mounted, or test -w, is not enough: right after the mount both say yes while creating a file still fails with
// EPERM. A timeout is only a warning: the emulator is usable, the caller's write fails visibly if it is still too early.
func (m *Manager) waitSDCard(ctx context.Context, i int) bool {
	return pollUntil(ctx, sdcardMountWait, 500*time.Millisecond, func() bool {
		return m.shell(ctx, i, sdcardProbe, statusAdbTimeout).Code == 0
	})
}

// sdcardProbe creates and removes a file in /sdcard as the shell user (whom apps and AUTO-MAS's adb push write as).
const sdcardProbe = "f=/sdcard/.modavd-sdcard-check; touch $f && rm -f $f"

// pollUntil calls ok (at least once) every interval until it returns true (true), limit has passed or ctx ends
// (false).
func pollUntil(ctx context.Context, limit, every time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(limit)
	for {
		if ctx.Err() != nil {
			return false
		}
		if ok() {
			return true
		}
		if !time.Now().Before(deadline) || !sleepOrDone(ctx, every) {
			return false
		}
	}
}

// waitBoot polls sys.boot_completed every second until it is 1 (AUTO-MAS _wait_boot). An instance this call launched
// is not re-registered meanwhile (it has not registered with the private server yet, which is normal); an adopted
// one may have missed a server restart and is. Fails when the deadline passes, when emulator.exe has exited while
// no qemu is there, or when the adopted qemu is gone. onQemu, when set, is told the qemu process each time the scan
// finds it (the setup marker records it the first time). stopSeen, when set and a qemu was adopted, is set to true
// once a stop of this instance is seen holding its lock (relaunchAfterAdoptedGone). early, when set, runs each time
// adb answers before the boot has completed, until it returns true (earlyGuestFixes).
func (m *Manager) waitBoot(ctx context.Context, i int, timeout time.Duration, emulator, adopted *host.Process,
	logPath string, prog *progressor, onQemu func(host.Process), stopSeen *bool, early func() bool) (host.Process, error) {
	launched := emulator != nil
	started := time.Now()
	deadline := started.Add(timeout)
	nextReport := started.Add(bootReportInterval)
	// booted: when the emulator was first seen to report Android booted; lastRegister: the last RegisterBooted try
	var booted, lastRegister time.Time
	for {
		if err := ctxErr(ctx); err != nil {
			return host.Process{}, err
		}
		p, found, err := waitBootFindQemu(m, i)
		if err != nil {
			return host.Process{}, err
		}
		if found && onQemu != nil {
			// before the getprop, which may take seconds: a cancel then must find qemu already recorded
			onQemu(p)
		}
		if found {
			done, answered := waitBootProbe(m, ctx, i, statusAdbTimeout, !launched)
			if done {
				return p, nil
			}
			if answered && early != nil && early() {
				early = nil
				continue // ask getprop again right away
			}
		}
		if err := ctxErr(ctx); err != nil {
			return host.Process{}, err
		}
		// Android is up but adb does not answer: register the instance with a private server that came up (or was
		// restarted) after the emulator's own registration, and stop waiting once that has not helped for a while.
		if found {
			if booted.IsZero() && emulatorReportedBoot(m.Root, i, p) {
				booted = time.Now()
				m.Log.Infof("实例 %d 的模拟器已报告安卓开机完成，等私有 adb 连上", i)
			}
			if !booted.IsZero() {
				if time.Since(lastRegister) >= bootRegisterEvery {
					lastRegister = time.Now()
					if registerBooted(ctx, m, i) {
						continue // ask getprop again right away
					}
				}
				if time.Since(booted) >= adbAfterBootWait {
					return host.Process{}, m.adbUnusableAfterBoot(i, booted)
				}
			}
		}
		if !time.Now().Before(deadline) {
			if !booted.IsZero() {
				return host.Process{}, m.adbUnusableAfterBoot(i, booted)
			}
			return host.Process{}, errs.Newf(errs.Timeout, "ModAVD 实例 %d 启动超时（%.0f 秒内安卓没有启动完成），日志见 %s",
				i, timeout.Seconds(), m.Root.LogsDir())
		}
		if !found && emulator != nil && !emulator.Alive() {
			code := "未知"
			if c, ok := emulator.ExitCode(); ok {
				code = strconv.FormatUint(uint64(c), 10)
			}
			return host.Process{}, emulatorExitedError(i, code, logPath)
		}
		if adopted != nil && stopSeen != nil && !*stopSeen && m.lockHeld(i, "stop") {
			*stopSeen = true
		}
		// both the process snapshot and the PID + creation time check must agree, so a failed snapshot is no exit
		if !found && adopted != nil && !adopted.Alive() {
			return host.Process{}, errs.Wrap(errs.LaunchFailed, errAdoptedGone,
				fmt.Sprintf("ModAVD 实例 %d 的模拟器（pid %d）在等安卓开机时退出了", i, adopted.PID))
		}
		if now := time.Now(); !now.Before(nextReport) {
			elapsed := now.Sub(started)
			done := bootProgressFrom + int(float64(bootProgressTo-bootProgressFrom)*elapsed.Seconds()/timeout.Seconds())
			if done >= bootProgressTo {
				done = bootProgressTo - 1
			}
			prog.emit("boot", fmt.Sprintf("实例 %d 等安卓开机，已 %.0f 秒", i, elapsed.Seconds()), done)
			nextReport = now.Add(bootReportInterval)
		}
		if !sleepOrDone(ctx, bootPollInterval) {
			return host.Process{}, ctxErr(ctx)
		}
	}
}

var (
	// adbAfterBootWait: how long the boot wait goes on once the emulator has reported Android booted while adb still
	// does not answer. adb normally answers before the boot completes; the rest of the timeout would only be waited
	// out (a user's create waited 300 s for an Android that had booted after 72 s).
	adbAfterBootWait = 60 * time.Second
	// bootRegisterEvery: how often the boot wait tries RegisterBooted meanwhile.
	bootRegisterEvery = 10 * time.Second
)

// emulatorReportedBoot: the emulator of qemu has reported Android booted. It deletes avd\mas_<i>.avd\bootcompleted.ini
// when it launches and writes it when the guest reports boot completion, so a file no older than qemu is this boot's.
var emulatorReportedBoot = func(r *root.Root, i int, qemu host.Process) bool {
	started := qemu.StartedAt()
	fi, err := os.Stat(filepath.Join(avd.Dir(r, i), "bootcompleted.ini"))
	return err == nil && !started.IsZero() && !fi.ModTime().Before(started)
}

// waitBootFindQemu is the boot wait's process scan (a variable for the tests).
var waitBootFindQemu = func(m *Manager, i int) (host.Process, bool, error) { return m.findQemu(i) }

// waitBootProbe is the boot wait's getprop (a variable for the tests).
var waitBootProbe = (*Manager).bootProbe

// registerBooted is adb's RegisterBooted for instance i (a variable for the tests).
var registerBooted = func(ctx context.Context, m *Manager, i int) bool {
	return m.ADB.RegisterBooted(ctx, m.Root.Ports().Serial(i))
}

// adbUnusableAfterBoot is the boot wait's error when Android has booted but adb never answered: says so, with the
// last line of the adb server's own log.
func (m *Manager) adbUnusableAfterBoot(i int, booted time.Time) error {
	msg := fmt.Sprintf("ModAVD 实例 %d 的安卓已经开好了（模拟器报告开机完成），但之后 %.0f 秒里私有 adb（端口 %d）一直用不了",
		i, time.Since(booted).Seconds(), m.Root.AdbServerPort())
	if last := adb.ServerLogLast(m.Root); last != "" {
		msg += "；adb 自己的日志最后一行：" + last
	}
	return errs.New(errs.Timeout, msg+"。日志见 "+m.Root.LogsDir())
}

// qemuAfterSetup is the qemu process to report once the after-boot setup is done: the one the scan finds now, else
// the one from the boot wait while it still runs (both the snapshot and the PID + creation time check must agree, so
// a failed snapshot is no exit, as in waitBoot). gone: it has exited (old is returned for the message then).
func qemuAfterSetup(old, now host.Process, found bool) (host.Process, bool) {
	if found {
		return now, false
	}
	return old, !old.Alive()
}

// errAdoptedGone is the cause of waitBoot's error when the qemu it adopted exited before Android was up.
var errAdoptedGone = errors.New("adopted qemu exited before boot completed")

// relaunchAfterAdoptedGone: the start boots the instance itself after the qemu it adopted exited before Android was
// up (err is errAdoptedGone). That is a guest which was powering off and no longer answered adb when the start looked
// (the window's ×, measured 11-02: adb goes about 1.3 s after the click, qemu 5–8 s), so only in the first round,
// never for the boot watch, only when the exit came within shutdownExitWait of the adopt, and only when no stop of
// this instance was seen meanwhile. A qemu adopted for longer, or ended by a stop (modavd stop, AUTO-MAS stopping a
// start it gave up on), was turned off on purpose: booting it again would overrule that.
func relaunchAfterAdoptedGone(round int, watch bool, err error, adoptedFor time.Duration, stopSeen bool) bool {
	return round == 1 && !watch && errors.Is(err, errAdoptedGone) && adoptedFor <= shutdownExitWait && !stopSeen
}

// shutdownExitWait is how long a start waits for the qemu of a guest that is powering off to exit. The emulator's own
// safe shutdown (the window's ×) gives Android 15 s and ends itself 25 s after the click at the latest; AUTO-MAS's
// close and modavd stop end it within 28 s. A variable for tests.
var shutdownExitWait = 30 * time.Second

// guestShuttingDown: the guest of instance i has been told to power off (sys.powerctl, see poweringOff) without a stop
// of ours: the window's × (the emulator asks Android to shut down and quits once it is off), AUTO-MAS's own close,
// another tool. qemu keeps running until Android is down (5 to 20 s) and sys.boot_completed stays 1 while adb still
// answers, so a start would otherwise adopt an instance that is about to vanish and report it online. false when adb
// does not answer.
func (m *Manager) guestShuttingDown(ctx context.Context, i int) bool {
	res := m.shell(ctx, i, "getprop sys.powerctl", statusAdbTimeout)
	return res.Code == 0 && poweringOff(res.Out)
}

// poweringOff: v, a value of sys.powerctl, powers the guest off ("shutdown" or "shutdown,<reason>"). A reboot keeps
// qemu running, and the boot wait sees it through.
func poweringOff(v string) bool {
	v = strings.TrimSpace(v)
	return v == "shutdown" || strings.HasPrefix(v, "shutdown,")
}

// waitShutdown waits up to shutdownExitWait for qemu p of instance i, whose guest is powering off, to exit and then
// scans for the instance's qemu again: found when someone launched it meanwhile (the start adopts that one). Busy when
// p is still there at the end.
func (m *Manager) waitShutdown(ctx context.Context, i int, p host.Process, prog *progressor) (host.Process, bool, error) {
	m.Log.Infof("ModAVD 实例 %d 正在关机（模拟器 pid %d 还没退出），等它关完再开机", i, p.PID)
	prog.emit("launch", fmt.Sprintf("实例 %d 正在关机，等它关完再开机", i), progressLaunch/2)
	if !waitQemuExit(ctx, p, shutdownExitWait) {
		if err := ctxErr(ctx); err != nil {
			return host.Process{}, false, err
		}
		return host.Process{}, false, errs.WithDetails(errs.Newf(errs.Busy, "ModAVD 实例 %d 正在关机，等了 %.0f 秒模拟器（pid %d）还没退出", i,
			shutdownExitWait.Seconds(), p.PID).WithAdvice(fmt.Sprintf("稍后再开机；一直关不掉的话先 modavd stop %d", i)), map[string]string{errs.Reason: "stopping"})
	}
	return m.findQemu(i)
}

// setupAdvice is added to the error of a start that ended after launching qemu (or adopting an unfinished boot). It
// names the marker's deadline (until; zero when unknown) as local time and the rules of the guest library fixes'
// zygote restart (owedMountGate), so nobody expects the next start to restart zygote under a running game or install, or to
// still finish the setup after the deadline (that start adopts the boot as an ordinary running instance).
func setupAdvice(until, now time.Time) string {
	const fix = "挂这些补丁要重启 zygote，只在前台没有应用、也没有安装正在进行时才挂（不然就留到之后再启动一次，或下次开机）"
	switch {
	case until.IsZero():
		return "模拟器没有关：再启动一次这个实例，就会接着做完这次开机后的设置，其中包括客体库补丁；" + fix
	case !now.Before(until):
		return "模拟器没有关；这次开机接着做设置的期限（" + clockText(until, now) + "）已过，客体库补丁等下次开机再挂"
	}
	return "模拟器没有关：在 " + clockText(until, now) + " 前再启动一次这个实例，就会接着做完这次开机后的设置，" +
		"过了这个时间客体库补丁就等下次开机再挂；" + fix
}

// clockText is t as local time for the messages: "15:04" on now's day, else with the date ("10 月 29 日 00:05"; a
// timeout_s near its maximum gives a deadline past midnight). Seconds are cut, so the time named is never later than
// t. "" for a zero t.
func clockText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = t.Local(), now.Local()
	if ty, tm, td := t.Date(); ty != now.Year() || tm != now.Month() || td != now.Day() {
		return fmt.Sprintf("%d 月 %d 日 %s", int(tm), td, t.Format("15:04"))
	}
	return t.Format("15:04")
}

// withSetupAdvice adds advice (setupAdvice) to err (a copy; the cause is kept). Not for LaunchFailed and
// InsufficientMemory: the emulator is gone.
func withSetupAdvice(err error, advice string) error {
	e := errs.As(err)
	if e == nil || e.Code == errs.LaunchFailed || e.Code == errs.InsufficientMemory {
		return err
	}
	c := *e
	if c.Advice == "" {
		c.Advice = advice
	} else {
		c.Advice += "；" + advice
	}
	return &c
}

// setupMarker is the content of setupMarkerPath: the boot whose after-boot setup is not finished. Written as soon
// as emulator.exe runs (qemu is not there yet, and a cancel can come first), completed with qemu once the boot wait
// finds it. Process identity is PID plus creation time, so a reused PID never matches.
type setupMarker struct {
	QemuPID         uint32 `json:"qemu_pid,omitempty"`
	QemuCreated     uint64 `json:"qemu_created,omitempty"`
	EmulatorPID     uint32 `json:"emulator_pid,omitempty"`
	EmulatorCreated uint64 `json:"emulator_created,omitempty"`
	LogFile         string `json:"log_file,omitempty"` // the emulator output of that boot
	// ValidUntil (Unix seconds) is the deadline set by the start that launched the boot: qemu's start plus
	// setupMarkerWindow of that start's boot timeout. 0 in markers of an older modavd (SetupMarkerMaxAge then).
	ValidUntil int64 `json:"valid_until,omitempty"`
	// AliveMS (host.MonotonicMS): the last time a start working on this boot said so (written at the launch or the take-over
	// and refreshed during the boot wait). While no ready record of the boot exists, the marker holds status at starting
	// only for bootSetupCap after it (bootready.go). 0: not held.
	AliveMS int64 `json:"alive_ms,omitempty"`
	// Round: the start attempt that works on this boot (overlay.go, "The round"); written before that start's boot screen is
	// started. Status reads the setup record of this round only while that start holds the lock.
	Round string `json:"round,omitempty"`
}

// until is ValidUntil as a time; zero when unset.
func (s setupMarker) until() time.Time {
	if s.ValidUntil == 0 {
		return time.Time{}
	}
	return time.Unix(s.ValidUntil, 0)
}

// deadline is when this marker stops being picked up for q's boot: ValidUntil, else (an older marker)
// SetupMarkerMaxAge after q started; zero when neither is known.
func (s setupMarker) deadline(q host.Process) time.Time {
	if u := s.until(); !u.IsZero() {
		return u
	}
	if started := q.StartedAt(); !started.IsZero() {
		return started.Add(SetupMarkerMaxAge)
	}
	return time.Time{}
}

// matches: q is the qemu process of the boot this marker names. Recorded qemu: same PID and creation time. Only the
// emulator.exe recorded: q is its child (emulator.exe waits for the qemu it starts) and not older than it.
func (s setupMarker) matches(q host.Process) bool {
	if q.PID == 0 || q.Created == 0 {
		return false
	}
	if s.QemuPID != 0 {
		return s.QemuPID == q.PID && s.QemuCreated == q.Created
	}
	return s.EmulatorPID != 0 && s.EmulatorCreated != 0 && q.PPID == s.EmulatorPID && q.Created >= s.EmulatorCreated
}

// SetupMarkerGrace is what a marker's deadline gives on top of the boot timeout of the start that wrote it: the
// after-boot setup plus time to call start again. A boot older than that has most likely been put to use (a game
// launched by AUTO-MAS, a user in the window), whatever the marker says.
const SetupMarkerGrace = 10 * time.Minute

// SetupMarkerMaxAge is the window of a start with the default boot timeout, and the fallback for markers without
// ValidUntil (written by an older modavd, which always used it).
const SetupMarkerMaxAge = DefaultBootTimeout + SetupMarkerGrace

// setupMarkerWindow is how long after qemu started the marker of a start with this boot timeout is picked up. A
// start with a longer timeout gives up later, so its marker has to last longer too (with a fixed 15 minutes a 30
// minute timeout_s would leave a marker that is stale before the start that wrote it has even timed out); a shorter
// one keeps the default window, so a quick timeout does not take away the time to call start again.
func setupMarkerWindow(timeout time.Duration) time.Duration {
	return max(timeout, DefaultBootTimeout) + SetupMarkerGrace
}

// setupMarkerUse decides what an adopting start does with the marker it read (ok=false: none). use: the marker names
// q's boot and now is before its deadline (setupMarker.deadline), so the setup is finished as on a fresh boot.
// stale: it names q's boot but the deadline has passed (or is unknown); it is deleted then, like a marker of another
// boot.
func setupMarkerUse(mk setupMarker, ok bool, q host.Process, now time.Time) (use, stale bool) {
	if !ok || !mk.matches(q) {
		return false, false
	}
	until := mk.deadline(q)
	if until.IsZero() || !now.Before(until) {
		return false, true
	}
	return true, false
}

// readSetupMarker reads the marker of instance i; ok=false when there is none or it cannot be parsed (a torn write
// counts as none: the start that wrote it cannot be matched any more).
func (m *Manager) readSetupMarker(i int) (setupMarker, bool) {
	var s setupMarker
	b, err := os.ReadFile(m.setupMarkerPath(i))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return setupMarker{}, false
	}
	return s, s.QemuPID != 0 || s.EmulatorPID != 0
}

// setupMarkerWrite stores the marker file (a variable: the tests make it fail).
var setupMarkerWrite = writeFileReplace

// writeSetupMarker replaces the marker (temp file + rename, so a reader never sees half of it). Only starts write it,
// under the start lock. A failure costs only the "finish it next time" path: a warning.
func (m *Manager) writeSetupMarker(i int, s setupMarker) error {
	b, err := json.Marshal(s)
	if err == nil {
		err = setupMarkerWrite(m.setupMarkerPath(i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		m.Log.Warnf("实例 %d 记不下「这次开机后的设置还没做完」（%v）：这次启动要是被打断，再启动一次补不上客体库补丁", i, err)
	}
	return err
}

// removeSetupMarker deletes the marker of instance i. A marker left behind after a finished setup would make the
// next start of this boot run the one-time parts again (harmless while the guest library fixes are mounted: the
// script finds the mounts; otherwise only with no app in the foreground, no install running and before its deadline), so a failure
// is a warning.
func (m *Manager) removeSetupMarker(i int) {
	path := m.setupMarkerPath(i)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.Log.Warnf("实例 %d 删不掉开机设置标记 %s: %v", i, path, err)
	}
}

// sleepOrDone sleeps d; false when ctx ended first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
