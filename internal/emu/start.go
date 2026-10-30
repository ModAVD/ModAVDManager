//go:build windows

package emu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

// Progress values: one scale for the whole call so they only ever grow (MCP requires strictly increasing progress).
// The boot wait spreads over bootProgressFrom..bootProgressTo by elapsed time.
const (
	progressTotal      = 1000
	progressLaunch     = 50
	bootProgressFrom   = 100
	bootProgressTo     = 600
	progressBooted     = 610
	progressTune       = 620
	progressIme        = 700
	progressFirstBoot  = 800
	progressVulkanFix  = 900
	bootReportInterval = 5 * time.Second
	// how long a start waits for a boot lock nobody should hold (the start lock is ours)
	bootLockWait = 10 * time.Second
)

// progressor sends Progress to the caller with Done strictly increasing.
type progressor struct {
	fn   func(Progress)
	last int
}

func (p *progressor) emit(step, msg string, done int) {
	if p == nil || p.fn == nil {
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

// Start boots instance i (or adopts it when already running), runs the per-boot setup and, once, the first-boot
// setup, and returns when the instance is online. See StartOptions / StartResult.
//
// Port of AUTO-MAS AvdManager.open (without launching an app): one start per instance at a time, and a start waits
// for a stop of the same instance to finish first (otherwise it would take an instance that is shutting down for a
// running one). Already running: no launch; the boot wait and the per-boot steps run again, the Vulkan fix is only
// checked (mounting it restarts zygote and would end the apps running on it). On cancel the emulator is left
// running: ending qemu without a guest shutdown is a power cut.
//
// Unlike AUTO-MAS (one long-running process), a start here can end before its setup is done: an MCP call cancelled,
// a boot slower than the timeout. A start that launches qemu therefore leaves the setup marker (setupMarkerPath) on
// disk until the whole after-boot setup has run; a later start that adopts that same qemu process before the marker's
// deadline (setupMarkerWindow after qemu started: the launching start's own boot timeout counts) does the setup as on
// a fresh boot, and the error of the interrupted start names that deadline. The guest may be in use by then all the
// same (AUTO-MAS adopted it and launched a game, an MCP client went on with shell / install_apk, a CLI user pressed
// Ctrl+C on a slow start --window and started playing), so that start mounts the Vulkan fix (a zygote restart, ending
// every app) only while no app is in the foreground and no install holds the guest lock; otherwise it leaves the
// marker, warns and reports SetupPending.
//
// A start that finishes records the guest boot it set up and leaves a boot watch on its qemu (bootwatch.go): when
// the guest reboots on its own (qemu stays), the watch runs this start again, and so does any start that finds the
// recorded boot_id changed. The per-boot steps then run again and the Vulkan fix is owed as above.
func (m *Manager) Start(ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
	t0 := time.Now()
	inst, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	col := &logx.Collector{}
	mm := m.withLog(col.Sink())
	// The execution bridge (bridge.go): an instance about to be launched boots writable already, so the install after
	// the setup needs one cold boot fewer.
	bridge := opt.watchQemu == nil && !opt.NoBridge && m.bridgeWanted(i)
	if _, running := m.findQemu(i); bridge && !running {
		if err := writeBridgeMarker(m.Root, i); err != nil {
			mm.Log.Warnf("实例 %d 没放上执行桥的开机标记: %v", i, err)
		}
	}
	res, err := mm.start(ctx, inst, opt, t0)
	if err != nil {
		return nil, err
	}
	res.Warnings = col.Messages()
	// The execution bridge (bridge.go): its marker is there and the guest still runs the stock translator. After the
	// whole setup, because installing takes a cold boot or two; and only on a boot this start launched, never on a
	// running instance it adopted (someone may be playing on it: the install restarts adbd and boots it cold).
	if bridge && res.Launched && m.bridgePending(ctx, i) {
		return m.finishBridge(ctx, i, opt, res)
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
		stopLock.Unlock()
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	res := &StartResult{}
	var emulator, adopted *win.Process
	var bootLock *win.FileLock
	var launchedAt time.Time
	// marker: this boot's after-boot setup is owed (this call launched qemu, or adopts the qemu of a start that ended
	// before finishing it); its one-time parts then run as on a fresh boot. nil = an ordinary adopt.
	var marker *setupMarker
	mode := setupAdopted
	p, running := m.findQemu(i)
	if w := opt.watchQemu; w != nil && (!running || p.PID != w.PID || p.Created != w.Created) {
		// the watched qemu is gone (or replaced): a boot watch never launches an emulator
		return nil, errs.Newf(errs.LaunchFailed, "实例 %d 的模拟器（pid %d）已不在运行", i, w.PID)
	}
	if running {
		m.Log.Infof("魔改 AVD 实例 %d 已在运行（pid %d）", i, p.PID)
		prog.emit("launch", fmt.Sprintf("实例 %d 已在运行（pid %d），不重新开机", i, p.PID), progressLaunch)
		adopted = &p
		mk, ok := m.readSetupMarker(i)
		now := time.Now()
		switch use, stale := setupMarkerUse(mk, ok, p, now); {
		case use:
			m.Log.Infof("实例 %d 这次开机后的设置还没做完（拉起它的那次启动被取消或超时了），这次接着做完", i)
			dirty := false
			if mk.QemuPID == 0 {
				// matched by its parent emulator.exe: from now on by qemu itself
				mk.QemuPID, mk.QemuCreated = p.PID, p.Created
				dirty = true
			}
			if mk.ValidUntil == 0 {
				// written by an older modavd: keep the deadline it was just judged by, so the messages can name it
				mk.ValidUntil = mk.deadline(p).Unix()
				dirty = true
			}
			if dirty {
				m.writeSetupMarker(i, mk)
			}
			marker = &mk
			mode = setupOwed
			if regularFile(mk.LogFile) {
				res.LogFile = mk.LogFile // exactly this boot's emulator log (the IME channel is read from it)
			}
		case stale:
			// this boot has run long enough to be in use; the interrupted start is long past
			why := "读不出这次开机的时间"
			if d := mk.deadline(p); !d.IsZero() {
				why = "接着做设置的期限 " + clockText(d, now) + " 已过"
			}
			m.Log.Infof("实例 %d 的开机设置标记已过期（%s），按已在运行的实例处理，客体 Vulkan 驱动补丁等下次开机再挂", i, why)
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
		plan, err := m.prepareLaunch(ctx, inst, opt.Window)
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
		p, err := m.startEmulator(i, plan)
		if err != nil {
			bootLock.Unlock()
			return nil, err
		}
		launchedAt = time.Now()
		emulator = &p
		res.Launched = true
		mode = setupLaunched
		res.LogFile = plan.logPath
		// written before qemu shows up (a cancel may come first): until then it names qemu by its parent emulator.exe,
		// and the deadline counts from the launch (qemu starts right after it; onQemu moves it to qemu's own start)
		marker = &setupMarker{EmulatorPID: p.PID, EmulatorCreated: p.Created, LogFile: plan.logPath,
			ValidUntil: launchedAt.Add(window).Unix()}
		m.writeSetupMarker(i, *marker)
		prog.emit("launch", fmt.Sprintf("实例 %d 的模拟器已拉起（pid %d），等安卓开机", i, p.PID), progressLaunch)
	}
	// owed: errors from here on leave a boot whose setup the next start finishes
	owed := func(err error) error {
		if marker == nil {
			return err
		}
		return withSetupAdvice(err, setupAdvice(marker.until(), time.Now()))
	}
	onQemu := func(q win.Process) {
		if marker != nil && marker.QemuPID == 0 && q.Created != 0 {
			marker.QemuPID, marker.QemuCreated = q.PID, q.Created
			// only a launched boot gets here (an owed marker has its qemu recorded already): count from qemu itself
			if started := q.StartedAt(); mode == setupLaunched && !started.IsZero() {
				marker.ValidUntil = started.Add(window).Unix()
			}
			m.writeSetupMarker(i, *marker)
		}
	}

	qemu, err := m.waitBoot(ctx, i, timeout, emulator, adopted, res.LogFile, prog, onQemu)
	bootLock.Unlock()
	if err != nil {
		return nil, owed(err)
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
			marker = &setupMarker{QemuPID: qemu.PID, QemuCreated: qemu.Created, LogFile: res.LogFile,
				ValidUntil: time.Now().Add(SetupMarkerGrace).Unix()}
			m.writeSetupMarker(i, *marker)
		}
	}

	var owedUntil time.Time // the deadline the deferred-mount warning names
	if marker != nil {
		owedUntil = marker.until()
	}
	ab, err := m.afterBoot(ctx, inst, mode, owedUntil, res.LogFile, prog)
	if err != nil {
		return nil, owed(err)
	}
	res.FirstBoot = ab.FirstBoot
	res.ZygoteRestarted = ab.ZygoteRestarted
	res.LogcatFile = ab.LogcatFile

	// The setup ran on adb alone, whose failures are only warnings: qemu may have gone meanwhile (a stop, which does
	// not wait for a start; a host GPU driver reset). Reporting online with the old PID would send the caller on to a
	// device that is not there; modavd has no watchdog that would correct it later (AUTO-MAS has).
	now, found := m.findQemu(i)
	qemu, gone := qemuAfterSetup(qemu, now, found)
	// The setup is over (done, or its boot is gone), unless the Vulkan fix was left out because an app was in the
	// foreground or an install was running: the marker then stays, so a start after that (before its deadline) still
	// mounts it.
	if gone || !ab.SetupPending {
		m.removeSetupMarker(i)
	}
	if gone {
		return nil, errs.Newf(errs.LaunchFailed, "魔改 AVD 实例 %d 在开机后设置期间退出了（模拟器 pid %d）：%s", i,
			qemu.PID, orText(logTail(res.LogFile, 8), "日志见 "+m.Root.LogsDir()))
	}
	res.SetupPending = ab.SetupPending
	// The boot read before the setup, so a guest that rebooted during it is set up again by the watch. Unknown (adb
	// failed): the old record stays, which at worst makes the watch set this boot up once more.
	if bootID != "" {
		m.writeBootRecord(i, bootRecord{QemuPID: qemu.PID, QemuCreated: qemu.Created, BootID: bootID})
	}
	if opt.watchQemu == nil {
		m.startBootWatch(i, qemu)
	}

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

// waitBoot polls sys.boot_completed every second until it is 1 (AUTO-MAS _wait_boot). An instance this call launched
// is not re-registered meanwhile (it has not registered with the private server yet, which is normal); an adopted
// one may have missed a server restart and is. Fails when the deadline passes, when emulator.exe has exited while
// no qemu is there, or when the adopted qemu is gone. onQemu, when set, is told the qemu process each time the scan
// finds it (the setup marker records it the first time).
func (m *Manager) waitBoot(ctx context.Context, i int, timeout time.Duration, emulator, adopted *win.Process,
	logPath string, prog *progressor, onQemu func(win.Process)) (win.Process, error) {
	launched := emulator != nil
	started := time.Now()
	deadline := started.Add(timeout)
	nextReport := started.Add(bootReportInterval)
	for {
		if err := ctxErr(ctx); err != nil {
			return win.Process{}, err
		}
		p, found := m.findQemu(i)
		if found && onQemu != nil {
			// before the getprop, which may take seconds: a cancel then must find qemu already recorded
			onQemu(p)
		}
		if found && m.bootCompleted(ctx, i, statusAdbTimeout, !launched) {
			return p, nil
		}
		if err := ctxErr(ctx); err != nil {
			return win.Process{}, err
		}
		if !time.Now().Before(deadline) {
			return win.Process{}, errs.Newf(errs.Timeout, "魔改 AVD 实例 %d 启动超时（%.0f 秒内安卓没有启动完成），日志见 %s",
				i, timeout.Seconds(), m.Root.LogsDir())
		}
		if !found && emulator != nil && !emulator.Alive() {
			code := "未知"
			if c, ok := emulator.ExitCode(); ok {
				code = strconv.FormatUint(uint64(c), 10)
			}
			return win.Process{}, errs.Newf(errs.LaunchFailed, "魔改 AVD 实例 %d 启动失败（emulator.exe 退出码 %s）：%s",
				i, code, logTail(logPath, 8))
		}
		// both the process snapshot and the PID + creation time check must agree, so a failed snapshot is no exit
		if !found && adopted != nil && !adopted.Alive() {
			return win.Process{}, errs.Newf(errs.LaunchFailed, "魔改 AVD 实例 %d 的模拟器（pid %d）在等安卓开机时退出了",
				i, adopted.PID)
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
			return win.Process{}, ctxErr(ctx)
		}
	}
}

// qemuAfterSetup is the qemu process to report once the after-boot setup is done: the one the scan finds now, else
// the one from the boot wait while it still runs (both the snapshot and the PID + creation time check must agree, so
// a failed snapshot is no exit, as in waitBoot). gone: it has exited (old is returned for the message then).
func qemuAfterSetup(old, now win.Process, found bool) (win.Process, bool) {
	if found {
		return now, false
	}
	return old, !old.Alive()
}

// setupAdvice is added to the error of a start that ended after launching qemu (or adopting an unfinished boot). It
// names the marker's deadline (until; zero when unknown) as local time and the rules of the Vulkan fix's zygote
// restart (owedMountGate), so nobody expects the next start to restart zygote under a running game or install, or to
// still finish the setup after the deadline (that start adopts the boot as an ordinary running instance).
func setupAdvice(until, now time.Time) string {
	const fix = "这个补丁要重启 zygote，只在前台没有应用、也没有安装正在进行时才挂（不然就留到之后再启动一次，或下次开机）"
	switch {
	case until.IsZero():
		return "模拟器没有关：再启动一次这个实例，就会接着做完这次开机后的设置，其中包括客体 Vulkan 驱动补丁；" + fix
	case !now.Before(until):
		return "模拟器没有关；这次开机接着做设置的期限（" + clockText(until, now) + "）已过，客体 Vulkan 驱动补丁等下次开机再挂"
	}
	return "模拟器没有关：在 " + clockText(until, now) + " 前再启动一次这个实例，就会接着做完这次开机后的设置，" +
		"过了这个时间客体 Vulkan 驱动补丁就等下次开机再挂；" + fix
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

// withSetupAdvice adds advice (setupAdvice) to err (a copy; the cause is kept). Not for LaunchFailed: the emulator is
// gone.
func withSetupAdvice(err error, advice string) error {
	e := errs.As(err)
	if e == nil || e.Code == errs.LaunchFailed {
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
func (s setupMarker) deadline(q win.Process) time.Time {
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
func (s setupMarker) matches(q win.Process) bool {
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
func setupMarkerUse(mk setupMarker, ok bool, q win.Process, now time.Time) (use, stale bool) {
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

// writeSetupMarker replaces the marker (temp file + rename, so a reader never sees half of it). Only starts write it,
// under the start lock. A failure costs only the "finish it next time" path: a warning.
func (m *Manager) writeSetupMarker(i int, s setupMarker) {
	b, err := json.Marshal(s)
	if err == nil {
		err = writeFileReplace(m.setupMarkerPath(i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		m.Log.Warnf("实例 %d 记不下「这次开机后的设置还没做完」（%v）：这次启动要是被打断，再启动一次补不上客体 Vulkan 驱动补丁", i, err)
	}
}

// removeSetupMarker deletes the marker of instance i. A marker left behind after a finished setup would make the
// next start of this boot run the one-time parts again (harmless while the Vulkan fix is mounted: the script finds
// the mount; otherwise only with no app in the foreground, no install running and before its deadline), so a failure
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
