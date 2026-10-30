//go:build windows

package emu

// Guest-only reboots. `adb reboot`, Restart in the guest's power menu or a guest kernel panic restart Android inside
// the same qemu process. The per-boot tuning of afterBoot goes with the old kernel (dirty writeback back to the
// kernel's 3000 / 500, no ARM translation cpuinfo binding, no Vulkan fix mount, no fastpath / idlepoll daemons, the
// logcat capture ends), and modavd, which has no long-running process of its own, would not notice: afterBoot runs
// only in Start.
//
// So a start that finishes records the boot it set up (bootRecord: the qemu process and the guest kernel's boot_id,
// a new random UUID at every kernel boot) and leaves a boot watch: this executable again, detached like the emulator
// (`<exe> __bootwatch <root> <i> <qemu pid> <qemu created>`), living exactly as long as that qemu process. Every
// bootWatchInterval it compares the booted guest's boot_id with the record; another one means the guest rebooted under
// the same qemu, and the watch runs the start of an adopted instance on it (Start with StartOptions.watchQemu). That
// start sees the same mismatch and treats the boot as owed (setupOwed): the per-boot steps run again and the Vulkan
// fix is mounted while nobody uses the guest (owedMountGate). The one-time first-boot setup is not repeated
// (mas-avd.json initialized).
//
// The watch never waits: while another start or a stop runs it looks again at the next tick, and a start that sets up
// a boot writes its record, so a boot is never set up twice. Its messages go to <root>\logs\modavd-bootwatch.log (one
// JSON object per line, as `--json` on stderr).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// BootWatchArg is the hidden subcommand both executables route to RunBootWatch.
const BootWatchArg = "__bootwatch"

// bootWatchInterval: how often the watch reads the guest's boot_id (one adb shell). A game started within this long
// after a guest reboot may still meet the kernel defaults. A variable for tests.
var bootWatchInterval = 5 * time.Second

const (
	bootWatchLogName = "modavd-bootwatch.log"
	// bootWatchLogMax: past this a watch about to start moves the log to .1 (one backup).
	bootWatchLogMax = 4 << 20
	// guestBootTimeout bounds the one adb shell of guestBoot.
	guestBootTimeout = statusAdbTimeout
	// bootWatchReregisterAfter: a guest-only reboot takes the instance off the private server for about a second, and
	// the emulator registers it again itself (10-29: offline, gone, offline, device 13 s after `adb reboot`); a
	// re-registration then only races it and warns. A server that was restarted has lost the instance for good: the
	// watch registers it again (which also restarts the logcat capture) once it has been missing this long, and then
	// at most this often.
	bootWatchReregisterAfter = 30 * time.Second
)

// guestBootScript prints sys.boot_completed and the guest kernel's boot_id; both readable without root.
const guestBootScript = "getprop sys.boot_completed; cat /proc/sys/kernel/random/boot_id"

// guestBootRead, when a test sets it, replaces the adb shell behind guestBoot.
var guestBootRead func(i int, reregister bool) (booted bool, bootID string, missing bool)

// guestBoot: whether Android has booted, and the kernel's boot_id ("" when it cannot be read). reregister as
// adb.RunOptions; missing: the private server does not know the instance (in the end, after any re-registration).
func (m *Manager) guestBoot(ctx context.Context, i int, reregister bool) (booted bool, bootID string, missing bool) {
	if guestBootRead != nil {
		return guestBootRead(i, reregister)
	}
	serial := root.Serial(i)
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: serial, Timeout: guestBootTimeout, Reregister: reregister}, "shell",
		guestBootScript)
	booted, bootID = parseGuestBoot(res.Out)
	return booted, bootID, res.Code != 0 && adb.DeviceMissing(serial, res.Out)
}

// parseGuestBoot reads guestBootScript's output: booted when its first line is "1" (getprop prints an empty line for
// an unset property), bootID the first line that is a UUID. adb's error texts give neither.
func parseGuestBoot(out string) (booted bool, bootID string) {
	lines := outputLines(out)
	for k, line := range lines {
		line = strings.TrimSpace(line)
		if k == 0 && line == "1" {
			booted = true
		}
		if bootID == "" && isUUID(line) {
			bootID = strings.ToLower(line)
		}
	}
	return booted, bootID
}

// isUUID: 8-4-4-4-12 hex digits.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for k, r := range s {
		switch k {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f' || 'A' <= r && r <= 'F') {
				return false
			}
		}
	}
	return true
}

// bootRecord is the content of bootRecordPath: the boot a finished start set up, i.e. its qemu process (PID +
// creation time) and the guest kernel's boot_id read right after Android had booted.
type bootRecord struct {
	QemuPID     uint32 `json:"qemu_pid"`
	QemuCreated uint64 `json:"qemu_created"`
	BootID      string `json:"boot_id"`
}

// names: q is the qemu process this record was written for.
func (r bootRecord) names(q win.Process) bool {
	return r.QemuPID != 0 && r.QemuCreated != 0 && r.QemuPID == q.PID && r.QemuCreated == q.Created
}

// guestRebooted: the record names q and the guest now runs another kernel boot (bootID; "" = unknown, no reboot).
func (r bootRecord) guestRebooted(q win.Process, bootID string) bool {
	return r.names(q) && r.BootID != "" && bootID != "" && bootID != r.BootID
}

// bootRecordPath is <root>\avd\.mas_<i>.boot.
func (m *Manager) bootRecordPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".boot")
}

// readBootRecord: ok=false when there is none or it cannot be parsed.
func (m *Manager) readBootRecord(i int) (bootRecord, bool) {
	var r bootRecord
	b, err := os.ReadFile(m.bootRecordPath(i))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return bootRecord{}, false
	}
	return r, r.QemuPID != 0
}

// writeBootRecord replaces the record (temp file + rename). A failure is a warning: a guest-only reboot of this boot
// is then not noticed (or the next start sets the boot up as already running).
func (m *Manager) writeBootRecord(i int, r bootRecord) {
	b, err := json.Marshal(r)
	if err == nil {
		err = writeFileReplace(m.bootRecordPath(i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		m.Log.Warnf("实例 %d 记不下这次开机的客体 boot_id（%v）：客体自己重启后不会自动补做开机调优", i, err)
	}
}

// shortBootID is the first block of a boot_id, for the messages.
func shortBootID(id string) string {
	if k := strings.IndexByte(id, '-'); k > 0 {
		return id[:k]
	}
	return orText(id, "?")
}

// bootWatchArgv is the watch's command line: this executable with BootWatchArg (tests replace it).
var bootWatchArgv = func(rootDir string, i int, q win.Process) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, BootWatchArg, rootDir, strconv.Itoa(i), strconv.FormatUint(uint64(q.PID), 10),
		strconv.FormatUint(q.Created, 10)}, nil
}

// startBootWatch starts the boot watch of qemu q (RunBootWatch), detached and out of our process tree, unless one runs
// for this instance already (its lock is held: the watch of this same qemu, since a watch ends with its qemu). A
// failure is a warning.
func (m *Manager) startBootWatch(i int, q win.Process) {
	if m.lockHeld(i, "watch") {
		return
	}
	argv, err := bootWatchArgv(m.Root.Dir, i, q)
	if err == nil {
		logPath := filepath.Join(m.Root.LogsDir(), bootWatchLogName)
		rotateLog(logPath, bootWatchLogMax)
		var p win.Process
		if p, err = win.StartOrphan(argv[0], argv[1:], win.StartOptions{OutputPath: logPath}); err == nil {
			m.Log.Debugf("实例 %d 开机守护已起（pid %d，日志 %s）", i, p.PID, logPath)
			return
		}
	}
	m.Log.Warnf("实例 %d 开机守护没起来（客体自己重启后不会自动补做开机调优，要再 start 一次）: %v", i, err)
}

// rotateLog moves path to path.1 once it reaches max bytes. A rename that fails (another watch has the file open)
// is tried again at the next start.
func rotateLog(path string, max int64) {
	if st, err := os.Stat(path); err == nil && st.Size() >= max {
		os.Rename(path, path+".1")
	}
}

// RunBootWatch is `<exe> __bootwatch <root> <i> <qemu pid> <qemu created>` (see the comment at the top of this file),
// started by startBootWatch. It holds <root>\avd\.mas_<i>.watch.lock for its whole life (a second watch of the
// instance exits at once) and returns when that qemu process exits. Messages go to w (stderr: the watch log). Returns
// the exit code.
func RunBootWatch(args []string, w io.Writer) int {
	log := logx.New(logx.JSONSink(w, logx.Info))
	if len(args) != 4 {
		log.Errorf("开机守护的参数不对: %q", args)
		return 2
	}
	i, err1 := strconv.Atoi(args[1])
	pid, err2 := strconv.ParseUint(args[2], 10, 32)
	created, err3 := strconv.ParseUint(args[3], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil || !root.ValidIndex(i) || pid == 0 || created == 0 {
		log.Errorf("开机守护的参数不对: %q", args)
		return 2
	}
	r, err := root.New(args[0])
	if err != nil {
		log.Errorf("开机守护找不到根目录 %s: %v", args[0], err)
		return errs.ExitCode(err)
	}
	// the emulator's re-registration and logcat capture start detached processes through this executable too
	if exe, err := os.Executable(); err == nil {
		win.OrphanHelper = exe
	}
	m := New(r, log)
	q := win.Process{PID: uint32(pid), Created: created}

	l, free, err := win.TryLock(m.lockPath(i, "watch"))
	if err != nil {
		log.Errorf("实例 %d 开机守护的锁文件打不开: %v", i, err)
		return 1
	}
	if !free {
		return 0 // the watch of this qemu runs already
	}
	defer l.Unlock()
	if !q.Alive() {
		return 0
	}
	log.Infof("实例 %d 开机守护已起（qemu pid %d）：客体自己重启后补做开机后调优", i, q.PID)
	watch := &bootWatch{m: m, i: i, q: q}
	// Canceled as soon as qemu exits: a set-up the watch started after a guest reboot holds the start lock and would
	// otherwise wait out its adb timeouts against a gone guest (10-29: about 110 s after a stop), refusing delete and
	// start of the instance meanwhile.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !q.Wait(time.Second) {
			if ctx.Err() != nil {
				return
			}
		}
		cancel()
	}()
	for !q.Wait(bootWatchInterval) {
		watch.tick(ctx, time.Now())
	}
	m.Wait(10 * time.Second)
	log.Infof("实例 %d 的模拟器（qemu pid %d）已退出，开机守护结束", i, q.PID)
	return 0
}

// bootWatch is the watch of qemu q of instance i.
type bootWatch struct {
	m *Manager
	i int
	q win.Process
	// missingSince: since when the private server has not known the instance (zero: it did at the last look), or
	// since the watch last registered it again (bootWatchReregisterAfter).
	missingSince time.Time
}

// tick is one look of the watch at now: nothing while a start or a stop of the instance runs, or while the record does
// not name q (no start has finished on it); otherwise, once Android has booted with another boot_id than the
// record's, the start that sets the boot up again.
func (w *bootWatch) tick(ctx context.Context, now time.Time) {
	m, i, q := w.m, w.i, w.q
	if m.stopping(i) || m.lockHeld(i, "start") {
		return
	}
	rec, ok := m.readBootRecord(i)
	if !ok || !rec.names(q) {
		return
	}
	reregister := !w.missingSince.IsZero() && now.Sub(w.missingSince) >= bootWatchReregisterAfter
	booted, bootID, missing := m.guestBoot(ctx, i, reregister)
	switch {
	case !missing:
		w.missingSince = time.Time{}
	case reregister || w.missingSince.IsZero():
		w.missingSince = now
	}
	if !booted || !rec.guestRebooted(q, bootID) {
		return
	}
	m.Log.Infof("实例 %d 的客体自己重启过（模拟器 pid %d 没变，boot_id %s → %s），补做开机后调优", i, q.PID,
		shortBootID(rec.BootID), shortBootID(bootID))
	res, err := m.Start(ctx, i, StartOptions{watchQemu: &q})
	switch {
	case errs.CodeOf(err) == errs.Busy:
		m.Log.Infof("实例 %d 正在开机或关机，开机后调优等它做完再看", i)
	case err != nil:
		m.Log.Warnf("实例 %d 补做开机后调优没做完: %v", i, err)
	default:
		m.Log.Infof("实例 %d 客体重启后的开机后调优已补做（%.1f 秒，%d 条警告）", i, res.TotalSeconds, len(res.Warnings))
	}
}

// removeBootRecord deletes the record of instance i (after Delete); best effort.
func (m *Manager) removeBootRecord(i int) {
	if err := os.Remove(m.bootRecordPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.Log.Debugf("实例 %d 删不掉开机记录: %v", i, err)
	}
}

// watchStartLock takes the start lock for the watch's start without waiting, and checks that no stop runs: errs.Busy
// otherwise (the watch looks again later).
func (m *Manager) watchStartLock(i int) (*win.FileLock, error) {
	path := m.lockPath(i, "start")
	l, free, err := win.TryLock(path)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
	}
	if !free {
		return nil, errs.Newf(errs.Busy, "实例 %d 正由另一次启动开机或做开机后设置", i)
	}
	if m.stopping(i) {
		l.Unlock()
		return nil, errs.Newf(errs.Busy, "实例 %d 正在关机", i)
	}
	return l, nil
}
