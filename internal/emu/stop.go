package emu

// Shutdown. Faithful port of AUTO-MAS manager.py close / _close / _graceful_shutdown / _sync_before_kill /
// _console_kill / _force_kill.
//
// The default sends the foreground app home, syncs, lets Android shut itself down (svc power shutdown) and waits for
// qemu to exit by itself; only when that fails it falls back to sync + console kill + terminate. A console kill or a
// terminate ends qemu without the guest's shutdown: whatever is still in the guest page cache is lost (10-03: nine
// Star Rail hot-update manifests became 0 bytes and showed up as "网络请求超时" and a black screen), and what a game
// writes between the sync and qemu's exit is lost too. Disk write-through only covers data already written to the
// virtual disk.
//
// An app in the foreground (not the home screen) first goes to the background with HOME and gets homeWait to finish
// what it writes on pause (Unity's OnApplicationPause): the shutdown itself leaves it about 2 s between its onPause
// and init's SIGKILL. The sync then comes before the shutdown command, while adbd surely answers: once Android is
// shutting down, adbd is ended early and the fallback's sync only gets "device offline", the console often no longer
// answers either, and the terminate is a power cut (ModAVD/ModAVD#88). svc power shutdown is Android's own shutdown
// (PowerManager, the ShutdownThread): the shutdown broadcast, the foreground activity paused and stopped, then init; it
// runs as shell, no su. Setting sys.powerctl, as before, hands the guest to init directly, and apps are ended without
// either.
//
// Time limits: AUTO-MAS gives close 30 s in all, see the shutdown constants in manager.go (worst case 28 s,
// stopBudget). The graceful path counts from the first look for qemu: Android gets until gracefulDeadline, and the
// fallback the rest of the budget (fallbackKillTimeout). adb commands here never re-register (that costs 5 s more),
// and starting the private server counts against the command's own limit (TotalTimeout).
//
// A stop that comes while a start holds the start lock but has not launched qemu yet (prechecks, the private server
// ...) waits for that qemu, up to startingQemuWait (10 s), and keeps holding the stop lock all the while. A start
// checks the stop lock once, right after taking its start lock, and waits for a running stop there: one that had not
// got past that check when this stop took the lock cannot launch until this stop is over (it then boots the instance
// as it was asked to), and one that had launches and its qemu is ended here. Letting go of the lock during the wait
// would let a later start launch and be stopped by a stop that came before it, or another stop slip in between.
//
// A qemu that appears during that wait has just been launched: Android has not booted and adbd is not up, so neither
// the graceful shutdown nor a sync can work, and nothing of the guest is at risk yet. That path (runLaunchingStop,
// Method launching_kill) neither prepares the private adb nor tries either of them: straight to the console kill, then
// the terminate, and the wait counts against the budget. With W the measured time from the first look to the end of
// the wait (at most startingQemuWait plus one process scan), K the console kill's share (sending it, at most 3 s, and
// the exit wait) and the terminate wait of 5 s:
//
//	W + K + 5 <= 28, K = launchingKillTimeout(W) = min(3 + 12, max(28 - 5 - W, 3))
//
// With W <= 8 the console path's own 3 + 12 s fit as they are; at W = 10, K is 13 s. The floor of 3 s (the kill
// command's own limit) would only break the budget for a W past 20 s, which the 10 s wait never reaches. The path
// ignores Force: there is nothing to sync. When nothing appears and the start lock is still held at the end, the stop
// is errs.Busy; when the start lets go without a qemu, the instance is offline.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

// stopBudget is the worst case of every stop path, counted from the first look for qemu (see the shutdown constants
// in manager.go; the caller in AUTO-MAS gives close 30 s in all).
const stopBudget = 28 * time.Second

// guestShutdownCommand asks Android to shut down through its framework (see the header comment).
const guestShutdownCommand = "svc power shutdown"

// guestHomeScript presses HOME unless nothing or the home screen is in front (the top resumed activity is the home
// activity's package), and says which: "app" when it pressed it, "home" when not.
const guestHomeScript = `home=$(cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.HOME | tail -n 1); ` +
	`top=$(dumpsys activity activities | grep -m 1 topResumedActivity); ` +
	`case "$top" in ""|*=null*|*" ${home%%/*}/"*) echo home ;; *) input keyevent KEYCODE_HOME && echo app ;; esac`

// startingQemuWait / startingQemuPoll: how long and how often a stop looks for the qemu of a start that holds the
// start lock but has not launched it yet. homeWait: how long an app sent home gets before the sync (see the header
// comment). Variables for tests.
var (
	startingQemuWait = 10 * time.Second
	startingQemuPoll = 500 * time.Millisecond
	homeWait         = 2 * time.Second
)

// stopOpsOf builds the side effects of a stop on qemu p (Manager.stopOpsFor); a variable so a test can drive Stop
// without an emulator.
var stopOpsOf = (*Manager).stopOpsFor

// Stop shuts instance i down (see StopResult.Method). Stopping an offline instance is not an error, but stopping one
// whose start has not launched qemu yet is errs.Busy (see waitStartingQemu). errs.Internal when Windows refuses to
// end qemu (see qemuNotEnded). An external stop of a creating instance is errs.Busy unless CancelCreate is explicit;
// Force only selects the shutdown method. Internal setup shutdowns and bridge restarts use creating instead.
func (m *Manager) Stop(ctx context.Context, i int, opt StopOptions) (*StopResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	c := &logx.Collector{}
	mm := m.withLog(c.Sink())
	l, err := mm.lock(ctx, i, "stop", stopLockWait, "正由另一次关闭在关")
	if err != nil {
		return nil, err
	}
	defer l.Unlock()

	// Check after taking the stop lock: a creation may have begun while we waited for another stop.
	state, _, _ := mm.creation(i)
	creating := state == creationCreating && !opt.creating
	if creating && !opt.CancelCreate {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在新建（后台开机做初始化），不能关机", i).
			WithAdvice(fmt.Sprintf("等新建做完；进度用 modavd status 查看；确实要打断请用 modavd stop %d --cancel-create", i))
	}
	if creating {
		if err := os.WriteFile(mm.createCancelPath(i), nil, 0o600); err != nil {
			return nil, errs.Wrap(errs.Internal, err, "请求取消新建失败")
		}
	}

	started := time.Now()
	res := &StopResult{Index: i, Name: in.Name, InterruptedCreate: creating}
	p, ok, err := mm.qemuOf(i)
	if err != nil {
		return nil, err
	}
	// launching: p appeared while we waited for a start, so Android cannot be up yet (see the header comment).
	launching := false
	if !ok && mm.lockHeld(i, "start") {
		// A start of this instance is under way but has not launched qemu yet (prechecks, the private server ...).
		// already_offline would be a success the start then overrides by booting the instance. Wait a little for its
		// qemu, still holding the stop lock: a start that has not got past its stop-lock check yet then cannot launch
		// before this stop is over, and no other stop can get in between (see the header comment).
		mm.Log.Infof("魔改 AVD 实例 %d 正在开机，等模拟器起来再关（最多 %.0f 秒）", i, startingQemuWait.Seconds())
		var found, starting bool
		p, found, starting, err = waitStartingQemu(ctx, startingQemuWait, startingQemuPoll,
			func() (win.Process, bool, error) { return mm.qemuOf(i) }, func() bool { return mm.lockHeld(i, "start") })
		if err != nil {
			return nil, err
		}
		if !found && starting {
			return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在开机，模拟器还没起来，稍后再关", i).
				WithAdvice("开机前的检查（硬件加速、Vulkan 等）可能要一两分钟")
		}
		ok = found
		// Only a qemu that really is that start's fresh one goes without a sync. One that was there all along (the
		// first scan can come back empty when the process snapshot fails) may be a booted guest: it takes the normal
		// path, sync included, even if that runs past the budget.
		launching = found && justLaunched(p, started, mm.booting(i))
		if found && !launching {
			mm.Log.Warnf("魔改 AVD 实例 %d 等开机时找到的模拟器不是刚拉起的（pid %d），按正常关机处理", i, p.PID)
		}
	}
	if !ok {
		res.Method = "already_offline"
		res.Warnings = c.Messages()
		return res, nil
	}
	// The qemu this stop handles: restart waits for this very process (PID and creation time) to be gone.
	res.PID, res.Process = int(p.PID), p
	ops := stopOpsOf(mm, ctx, i, p)
	var method string
	var synced bool
	if launching {
		method, synced, err = runLaunchingStop(ctx, mm.Log, i, launchingKillTimeout(time.Since(started)), ops)
	} else {
		method, synced, err = runStop(ctx, mm.Log, i, opt.Force, started, ops)
	}
	res.Method, res.Synced = method, synced
	res.Seconds = time.Since(started).Seconds()
	res.Warnings = c.Messages()
	if err != nil {
		return nil, err
	}
	return res, nil
}

// waitStartingQemu polls find every poll, for up to wait, while starting() says a start holds the start lock.
// found: its qemu appeared. stillStarting: no qemu, and the start lock is still held at the end; false when the start
// let go without one (it failed, or was canceled before launching), so the instance is simply offline. A cancel ends
// the wait with errs.Canceled.
func waitStartingQemu(ctx context.Context, wait, poll time.Duration, find func() (win.Process, bool, error),
	starting func() bool) (p win.Process, found, stillStarting bool, err error) {
	deadline := time.Now().Add(wait)
	for {
		if p, ok, err := find(); err != nil {
			return win.Process{}, false, false, err
		} else if ok {
			return p, true, false, nil
		}
		if !starting() {
			// A start that launched qemu and then failed leaves it running (a cancel never ends qemu): look once
			// more, it may have appeared between the two checks.
			p, ok, err := find()
			return p, ok, false, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return win.Process{}, false, true, nil
		}
		select {
		case <-ctx.Done():
			return win.Process{}, false, true, ctxErr(ctx)
		case <-time.After(min(poll, left)):
		}
	}
}

// justLaunched: qemu p, found while waiting for a start that began waiting at waitStarted, is that start's freshly
// launched one. The start holds the boot lock from just before the launch until sys.boot_completed (booting), and a
// qemu launched during the wait was created after it began (2 s of slack for the clocks of the snapshot).
func justLaunched(p win.Process, waitStarted time.Time, booting bool) bool {
	if booting {
		return true
	}
	at := p.StartedAt()
	return !at.IsZero() && !at.Before(waitStarted.Add(-2*time.Second))
}

// launchingKillTimeout is the console kill's share (sending it + the exit wait) on the launching path after elapsed
// since the first look: what stopBudget leaves after elapsed and the terminate wait, at most the console path's own
// consoleKillTimeout + closeTimeout, at least the consoleKillTimeout the command itself may take (only a
// startingQemuWait raised past 20 s could exceed the budget then).
func launchingKillTimeout(elapsed time.Duration) time.Duration {
	return min(max(stopBudget-forceKillWait-elapsed, consoleKillTimeout), consoleKillTimeout+closeTimeout)
}

// gracefulWindow is how long Android may take to shut down when the shutdown command goes out elapsed after the
// first look: until gracefulDeadline, which leaves the fallback its share of stopBudget (see fallbackKillTimeout).
func gracefulWindow(elapsed time.Duration) time.Duration {
	return max(gracefulDeadline-elapsed, 0)
}

// fallbackKillTimeout is the console kill's share (sending it + the exit wait) on the fallback after a failed graceful
// shutdown, elapsed after the first look: what stopBudget leaves after elapsed and the terminate wait, so a sync that
// failed at once (adbd already gone, ModAVD/ModAVD#88) leaves it more. At least fallbackKillMin (it falls back at
// gracefulDeadline, and the sync takes at most fallbackSyncTimeout), at most fallbackKillMax (a graceful shutdown that
// failed early).
func fallbackKillTimeout(elapsed time.Duration) time.Duration {
	return min(max(stopBudget-fallbackForceKillWait-elapsed, fallbackKillMin), fallbackKillMax)
}

// stopOps are the side effects of a stop, separated so the path selection can be tested without an emulator.
type stopOps struct {
	// prepare makes the private adb server usable for this instance; "" when it is, else why not.
	prepare func() string
	// home sends an app in the foreground home within timeout (guestHomeScript): sent, and adb's output when the
	// command failed ("" when it ran).
	home func(timeout time.Duration) (bool, string)
	// graceful asks Android to shut down and waits up to window for qemu to exit by itself; "" when it did, else why
	// not.
	graceful func(window time.Duration) string
	// sync runs `sync` in the guest within timeout; ok and adb's output.
	sync func(timeout time.Duration) (bool, string)
	// consoleKill sends the console kill and waits for qemu to exit, both within timeout.
	consoleKill func(timeout time.Duration) error
	alive       func() bool
	// forceKill terminates qemu (and its children) and waits up to wait for it to be gone (qemuKill.run). nil when it
	// is gone, and also when it was terminated but is still exiting after the wait (a warning then); an error only when
	// Windows refuses to end it at all (see qemuNotEnded), errs.Canceled when the wait was canceled.
	forceKill func(wait time.Duration) error
}

// runStop picks the path like AUTO-MAS _close and returns StopResult.Method and Synced; started is the first look for
// qemu, which the graceful path's time limits count from. A cancel never escalates: at every step that would end qemu
// harder it returns errs.Canceled instead (the instance may still be shutting down). When Windows refuses even the
// terminate, the forceKill error is returned: AUTO-MAS's close raises then too (psutil.AccessDenied out of
// kill_process_tree), and a success would report a qemu that keeps running as stopped.
func runStop(ctx context.Context, log *logx.Logger, i int, force bool, started time.Time, ops stopOps) (string, bool, error) {
	// Starting the private server and registering the instance count against the budget too: when that fails
	// there is neither a graceful shutdown nor a sync, only the console.
	problem := ops.prepare()

	if force {
		synced := syncBeforeKill(log, i, problem, syncTimeout, ops.sync)
		if err := stopCanceled(ctx, i); err != nil {
			return "force", synced, err
		}
		if err := ops.forceKill(forceKillWait); err != nil {
			return "force", synced, err
		}
		log.Infof("魔改 AVD 实例 %d 已强制关闭", i)
		return "force", synced, nil
	}

	if problem != "" {
		log.Warnf("魔改 AVD 实例 %d 私有 adb 用不了，不能正常关机也不能 sync，直接用控制台关: %s", i, problem)
		return endQemu(ctx, log, i, "console_kill", false, consoleKillTimeout+closeTimeout, forceKillWait, ops)
	}
	// The foreground app goes home first and gets homeWait; the sync below takes what it wrote along (see the header
	// comment). A failure is only a warning.
	if err := sendHome(ctx, log, i, ops.home); err != nil {
		return "graceful", false, err
	}
	// Before the shutdown command, while adbd surely answers (see the header comment). A failure is only a warning.
	presynced := syncBeforeShutdown(log, i, ops.sync)
	if err := stopCanceled(ctx, i); err != nil {
		return "graceful", false, err
	}
	reason := ops.graceful(gracefulWindow(time.Since(started)))
	if reason == "" {
		log.Infof("魔改 AVD 实例 %d 正常关机完成（%.1f s）", i, time.Since(started).Seconds())
		return "graceful", true, nil
	}
	if err := stopCanceled(ctx, i); err != nil {
		return "graceful", false, err
	}
	log.Warnf("魔改 AVD 实例 %d 正常关机没成（%s），退回 sync + 控制台 kill", i, reason)
	synced := syncBeforeKill(log, i, "", fallbackSyncTimeout, ops.sync)
	return endQemu(ctx, log, i, "sync_kill", presynced || synced, fallbackKillTimeout(time.Since(started)),
		fallbackForceKillWait, ops)
}

// sendHome sends an app in the foreground home and waits homeWait for it; nothing when the home screen is in front.
// errs.Canceled when the wait is canceled.
func sendHome(ctx context.Context, log *logx.Logger, i int, home func(time.Duration) (bool, string)) error {
	sent, failure := home(homeTimeout)
	if failure != "" {
		log.Warnf("魔改 AVD 实例 %d 关机前按 HOME 失败，照常关机: %s", i, failure)
		return nil
	}
	if !sent {
		return nil
	}
	log.Infof("魔改 AVD 实例 %d 前台的应用已退到后台，等 %.0f 秒让它保存再关机", i, homeWait.Seconds())
	select {
	case <-ctx.Done():
		return stopCanceled(ctx, i)
	case <-time.After(homeWait):
		return nil
	}
}

// syncBeforeShutdown: `sync` right before the shutdown command, so what the guest wrote until then reaches the
// virtual disk even when the shutdown then runs past its time and qemu has to be ended. Whether it did.
func syncBeforeShutdown(log *logx.Logger, i int, sync func(time.Duration) (bool, string)) bool {
	started := time.Now()
	ok, output := sync(shutdownSyncTimeout)
	if ok {
		log.Debugf("魔改 AVD 实例 %d 发关机命令前 sync 完成（%.0f ms）", i, float64(time.Since(started).Microseconds())/1000)
	} else {
		log.Warnf("魔改 AVD 实例 %d 发关机命令前 sync 失败，照常关机: %s", i, output)
	}
	return ok
}

// runLaunchingStop ends a qemu that appeared while Stop waited for a start that had not launched it yet: straight to
// the console kill within killTimeout, then the terminate (see the header comment). Method launching_kill (its own
// name, not console_kill: that one means the private adb of a booted guest was unusable and data may be lost; here
// there is no guest data yet), never synced.
func runLaunchingStop(ctx context.Context, log *logx.Logger, i int, killTimeout time.Duration, ops stopOps) (string, bool, error) {
	log.Infof("魔改 AVD 实例 %d 的模拟器刚起来，安卓还没开机，不做正常关机和 sync，直接从控制台关", i)
	return endQemu(ctx, log, i, "launching_kill", false, killTimeout, forceKillWait, ops)
}

// endQemu is the end of every path but force: the console kill (sending it and the exit wait within killTimeout),
// then, when qemu still runs, the terminate with forceWait, which appends "+terminate" to method.
func endQemu(ctx context.Context, log *logx.Logger, i int, method string, synced bool, killTimeout, forceWait time.Duration,
	ops stopOps) (string, bool, error) {
	if err := stopCanceled(ctx, i); err != nil {
		return method, synced, err
	}
	if err := ops.consoleKill(killTimeout); err != nil {
		log.Warnf("实例 %d 控制台 kill 失败，改为强制结束: %v", i, err)
	}
	if ops.alive() {
		if err := stopCanceled(ctx, i); err != nil {
			return method, synced, err
		}
		log.Warnf("魔改 AVD 实例 %d 未在 %.0f 秒内退出，强制结束", i, killTimeout.Seconds())
		method += "+terminate"
		if err := ops.forceKill(forceWait); err != nil {
			return method, synced, err
		}
	}
	log.Infof("魔改 AVD 实例 %d 已关闭", i)
	return method, synced, nil
}

// syncBeforeKill: `sync` before a console kill / terminate, so the guest page cache reaches the virtual disk. A
// failure is only a warning. problem != "" means the private adb is unusable (then it is the failure).
func syncBeforeKill(log *logx.Logger, i int, problem string, timeout time.Duration, sync func(time.Duration) (bool, string)) bool {
	started := time.Now()
	ok, output := false, problem
	if problem == "" {
		ok, output = sync(timeout)
	}
	if ok {
		log.Infof("魔改 AVD 实例 %d 关机前 sync 完成（%.0f ms）", i, float64(time.Since(started).Microseconds())/1000)
	} else {
		log.Warnf("魔改 AVD 实例 %d 关机前 sync 失败，照常关机: %s", i, output)
	}
	return ok
}

func stopCanceled(ctx context.Context, i int) error {
	if err := ctx.Err(); err != nil {
		return errs.Wrap(errs.Canceled, err, fmt.Sprintf("已取消（实例 %d 可能仍在关机）", i))
	}
	return nil
}

// stopOpsFor are the real side effects on qemu process p of instance i.
func (m *Manager) stopOpsFor(ctx context.Context, i int, p win.Process) stopOps {
	ports := m.Root.Ports()
	console, serial := ports.Console(i), ports.Serial(i)
	closeRun := func(timeout time.Duration, args ...string) adb.Result {
		return m.ADB.Run(ctx, adb.RunOptions{Serial: serial, Timeout: timeout, Reregister: false, TotalTimeout: true}, args...)
	}
	return stopOps{
		prepare: func() string { return m.ADB.PrepareForClose(ctx, console, adbServerStartOnClose) },
		home: func(timeout time.Duration) (bool, string) {
			res := closeRun(timeout, "shell", guestHomeScript)
			if res.Code != 0 {
				return false, fmt.Sprintf("返回码 %d: %s", res.Code, res.Out)
			}
			return strings.TrimSpace(res.Out) == "app", ""
		},
		graceful: func(window time.Duration) string {
			// Android's own shutdown (see the header comment). The command waits for the device to go off: adb loses
			// adbd on the way (exit 255, about 2.4 s after it), or it runs all of window while the framework takes
			// its time, so a code other than 0 says nothing about the shutdown and only qemu's exit counts. svc coming
			// back by itself with 0 (a usage text, an unknown subcommand) means no shutdown is under way: waiting
			// would only use up the budget.
			started := time.Now()
			res := closeRun(window, "shell", guestShutdownCommand)
			ran := time.Since(started)
			if res.Code == 0 && p.Alive() {
				return fmt.Sprintf("关机命令自己返回了，安卓没有开始关机: %s", res.Out)
			}
			if remaining := window - ran; remaining > 0 {
				waitQemuExit(ctx, p, remaining)
			}
			if p.Alive() {
				// one that failed before the window was over says why; one that ran all of it only timed out
				if res.Code != 0 && res.Out != "" && ran < window {
					return fmt.Sprintf("%.0f 秒内没有自己退出；关机命令返回码 %d: %s", window.Seconds(), res.Code, res.Out)
				}
				return fmt.Sprintf("%.0f 秒内没有自己退出", window.Seconds())
			}
			return ""
		},
		sync: func(timeout time.Duration) (bool, string) {
			res := closeRun(timeout, "shell", "sync")
			return res.Code == 0, res.Out
		},
		consoleKill: func(timeout time.Duration) error {
			started := time.Now()
			if _, err := adb.Console(ctx, console, "kill", min(consoleKillTimeout, timeout)); err != nil {
				return err
			}
			if remaining := timeout - time.Since(started); remaining > 0 {
				waitQemuExit(ctx, p, remaining)
			}
			return nil
		},
		alive: p.Alive,
		forceKill: func(wait time.Duration) error {
			return qemuKill{
				alive:    p.Alive,
				kill:     func() error { return win.KillTree(p) },
				waitExit: func(d time.Duration) bool { return waitQemuExit(ctx, p, d) },
				refused:  func() bool { return terminateRefused(p) },
			}.run(ctx, m.Log, i, p.PID, wait)
		},
	}
}

// qemuKill are the side effects of AUTO-MAS _force_kill on one qemu, separated for tests: alive (Process.Alive), kill
// (win.KillTree), waitExit (waitQemuExit) and refused (terminateRefused).
type qemuKill struct {
	alive    func() bool
	kill     func() error
	waitExit func(time.Duration) bool
	refused  func() bool
}

// run is AUTO-MAS _force_kill on qemu pid of instance i: terminate it and its children, wait up to wait for it to
// exit, and succeed unless Windows demonstrably refuses to end it.
//
// Like _force_kill, which swallows psutil.TimeoutExpired and reports OFFLINE, a terminated qemu that is still exiting
// after the wait is a success (with a warning): Windows ends a process only once all its threads have left the
// kernel, and a WHPX qemu releasing gigabytes of guest memory can take longer than the 2-5 s waits. restart waits for
// the exit itself before it boots again (cli waitQemuGone), stop --all only reports.
//
// A kill error is not a refusal by itself. After the console kill qemu is often already exiting, and TerminateProcess
// then answers access denied (STATUS_PROCESS_IS_TERMINATING) on a handle that has the right to terminate; psutil's
// proc_kill swallows exactly that error, so AUTO-MAS never sees it. Only when qemu still runs after the wait and
// Windows will not even open it for terminating (a qemu started elevated, e.g. by AUTO-MAS running as administrator,
// while modavd is not; psutil raises AccessDenied there) is it errs.Internal with the administrator hint.
func (q qemuKill) run(ctx context.Context, log *logx.Logger, i int, pid uint32, wait time.Duration) error {
	if !q.alive() {
		return nil
	}
	killErr := q.kill()
	// Still wait after a refused kill: an earlier console kill may be ending it.
	if q.waitExit(wait) {
		return nil
	}
	if err := stopCanceled(ctx, i); err != nil {
		return err
	}
	if killErr != nil {
		if q.refused() {
			if !q.alive() {
				return nil // it got there meanwhile
			}
			return qemuNotEnded(i, pid, killErr)
		}
		log.Debugf("结束实例 %d 的 qemu（pid %d）时 Windows 答复: %v（它已经在退出）", i, pid, killErr)
	}
	log.Warnf("魔改 AVD 实例 %d 的 qemu（pid %d）已强制结束，还在退出（等了 %.0f 秒）；稍后用 modavd status %d 确认它已退出",
		i, pid, wait.Seconds(), i)
	return nil
}

// terminateRefused: Windows will not open p for terminating, so no kill of ours can even begin (it runs at a higher
// integrity level, e.g. started by AUTO-MAS running as administrator). false when it can be opened: a kill that
// failed anyway met a process that is already exiting (see qemuKill.run). false too when p is gone: its PID is
// unused, or names a newer process (the creation time differs).
func terminateRefused(p win.Process) bool {
	return win.TerminateRefused(p)
}

// qemuNotEnded is the error of a stop after which qemu pid still runs because Windows refused to terminate it
// (killErr).
func qemuNotEnded(i int, pid uint32, killErr error) error {
	return errs.Wrap(errs.Internal, killErr, fmt.Sprintf("魔改 AVD 实例 %d 的 qemu（pid %d）没能结束：%v", i, pid, killErr)).
		WithAdvice("它可能是以管理员身份启动的（例如 AUTO-MAS 以管理员身份运行时）；用管理员权限再关一次，或在任务管理器里结束它")
}

// waitQemuExit waits up to d for p to exit, in short slices so a cancel is noticed. Returns whether it is gone. Where
// SYNCHRONIZE is denied, Process.Wait / Alive read the exit code instead, so a qemu whose exit code has become
// readable counts as gone too.
func waitQemuExit(ctx context.Context, p win.Process, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return !p.Alive()
		}
		if p.Wait(min(remaining, 200*time.Millisecond)) {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
}
