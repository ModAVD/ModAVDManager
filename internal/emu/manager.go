//go:build windows

// Package emu is the instance lifecycle on one ModAVD root: list / info / create / update / delete, start (boot +
// the per-boot tuning + the one-time first-boot setup), stop (Android shutdown, falling back to sync + kill), host
// checks. The CLI and the MCP server are thin layers over Manager.
//
// Port of AUTO-MAS app/utils/common/emulator2/avd/{manager,precheck}.py, minus what belongs to AUTO-MAS itself
// (launching games, MAA / MaaFW wiring, the freeze watchdog of a long-running process, updates). Every step and
// timeout there was measured on real instances; the reasons are kept in the comments of the port.
//
// Concurrency: AUTO-MAS serializes open / close per instance in memory. modavd runs as separate processes, so the
// same rules use lock files in <root>\avd (see lockPath): a start waits for another start and for a stop to finish;
// a stop waits for another stop but not for a start (an instance can be stopped while it boots).
package emu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/guest"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// Timeouts, from AUTO-MAS constants.py where they exist there.
const (
	DefaultBootTimeout = 300 * time.Second
	bootPollInterval   = time.Second
	statusAdbTimeout   = 5 * time.Second
	// how long a start waits for an earlier start / stop of the same instance
	startLockWait = DefaultBootTimeout + 300*time.Second
	stopLockWait  = 60 * time.Second

	// Shutdown (the caller in AUTO-MAS gives close 30 s in all, so the worst case stays under 28 s):
	// graceful 4 + 15 = 19, falling back to sync 2 + console kill 5 + terminate wait 2 => 28;
	// private adb unusable: 4 + console kill 3 + exit wait 12 + terminate wait 5 = 24; Force: 4 + sync 3 + 5 = 12.
	adbServerStartOnClose  = 4 * time.Second
	gracefulShutdown       = 15 * time.Second
	syncTimeout            = 3 * time.Second
	consoleKillTimeout     = 3 * time.Second
	closeTimeout           = 12 * time.Second
	forceKillWait          = 5 * time.Second
	fallbackSyncTimeout    = 2 * time.Second
	fallbackKillTimeout    = 5 * time.Second
	fallbackForceKillWait  = 2 * time.Second
	hostMemoryOverheadMB   = 1536
	minFreeDiskGBToBoot    = 8
	accelCheckTimeout      = 60 * time.Second
	vulkanProbeTimeout     = 30 * time.Second
	logKeepPerInstance     = 10
	logsDirMaxBytes        = 1 << 30
	guestDirtyExpireCs     = 200
	guestDirtyWritebackCs  = 100
	balloonReportingOrder  = 9
	logcatBufferSize       = "16M"
	guestTmpDir            = "/data/local/tmp"
	sdcardMountWait        = 60 * time.Second
	emulatorShmNamePrefix  = "SHM_videmulator"
	installLauncherTimeout = 180 * time.Second
)

// Manager works on one root. Log gets the progress messages.
type Manager struct {
	Root *root.Root
	Log  *logx.Logger
	ADB  *adb.Client
}

func New(r *root.Root, log *logx.Logger) *Manager {
	if log == nil {
		log = logx.Nop()
	}
	m := &Manager{Root: r, Log: log, ADB: adb.New(r, log)}
	m.ADB.Booting = func(serial string) bool {
		port, ok := consolePortOfSerial(serial)
		if !ok {
			return false
		}
		i, ok := root.IndexOfConsolePort(port)
		return ok && m.booting(i)
	}
	m.ADB.OnReregistered = m.logcatAfterReregister
	return m
}

// withLog returns a copy of m whose logger also feeds sink (used to collect warnings of one call). Its adb client
// shares m's hook tracking, so m.Wait also waits for the hooks the copy started.
func (m *Manager) withLog(sink logx.Sink) *Manager {
	c := *m
	c.Log = m.Log.Tee(sink)
	c.ADB = m.ADB.Derive(c.Log, c.logcatAfterReregister)
	return &c
}

// Wait waits up to timeout for the background work of this Manager and of the copies it made for single calls: the
// hooks that restart the logcat capture after a re-registration (adb.Client.WaitHooks). A short-lived process (the
// CLI after a command, MCP once its client is gone) calls it before it exits; otherwise an exit between ending the
// old capture and starting the new one leaves the instance without a guest log until its next start.
func (m *Manager) Wait(timeout time.Duration) {
	if m.ADB != nil {
		m.ADB.WaitHooks(timeout)
	}
}

func consolePortOfSerial(serial string) (int, bool) {
	rest, ok := strings.CutPrefix(serial, "emulator-")
	if !ok || rest == "" {
		return 0, false
	}
	port := 0
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		port = port*10 + int(ch-'0')
	}
	return port, true
}

func shmName(i int) string { return emulatorShmNamePrefix + strconv.Itoa(root.ConsolePort(i)) }

// ---- qemu processes ----------------------------------------------------------------------------------------------

// cmdlineMatches: "-avd <name>" and "-ports <console>,..." both present (AUTO-MAS _cmdline_matches). Neither alone
// is enough (an emulator started by another tool may use our ports under another name, or a mas_<i> name on other
// ports), and both together still do not tell roots apart: every root has the same names and the same port plan.
// adb.QemuBelongsTo does that.
func cmdlineMatches(cmdline []string, avdName string, console int) bool {
	hasAvd, hasPort := false, false
	want := strconv.Itoa(console)
	for k := 0; k+1 < len(cmdline); k++ {
		next := cmdline[k+1]
		switch cmdline[k] {
		case "-avd":
			if next == avdName {
				hasAvd = true
			}
		case "-ports":
			if first, _, _ := strings.Cut(next, ","); first == want {
				hasPort = true
			}
		}
	}
	return hasAvd && hasPort
}

func isQemuName(name string) bool { return strings.HasPrefix(strings.ToLower(name), "qemu-system") }

// scanQemuOf finds, with one process snapshot, the qemu process (qemu-system-x86_64[-headless].exe) of each of the
// indexes whose command line matches and that adb.QemuBelongsTo one of roots. The same-numbered instance of another
// root is not found: it is not ours to stop, re-register or adopt (a start then fails on its busy ports).
func scanQemuOf(roots []*root.Root, indexes []int) map[int]win.Process {
	found := map[int]win.Process{}
	if len(indexes) == 0 || len(roots) == 0 {
		return found
	}
	procs, err := win.ListProcesses()
	if err != nil {
		return found
	}
	for _, p := range procs {
		if !isQemuName(p.Name) {
			continue
		}
		cmdline, err := p.CommandLine()
		if err != nil {
			continue
		}
		for _, i := range indexes {
			if !cmdlineMatches(cmdline, root.AvdName(i), root.ConsolePort(i)) {
				continue
			}
			for _, r := range roots {
				if adb.QemuBelongsTo(r, p, i) {
					found[i] = p
					break
				}
			}
		}
	}
	return found
}

// scanQemu finds the qemu processes of the given indexes of m's root with one process snapshot.
func (m *Manager) scanQemu(indexes []int) map[int]win.Process {
	if m.Root == nil {
		return map[int]win.Process{}
	}
	return scanQemuOf([]*root.Root{m.Root}, indexes)
}

// findQemu is the qemu process of instance i of m's root (always a real process scan, see qemuScan).
func (m *Manager) findQemu(i int) (win.Process, bool) {
	p, ok := m.scanQemu([]int{i})[i]
	return p, ok
}

// bootCompleted: `getprop sys.boot_completed` says 1. reregister=false while the instance is booting (it has not
// registered with the private server yet, which is normal then).
func (m *Manager) bootCompleted(ctx context.Context, i int, timeout time.Duration, reregister bool) bool {
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: timeout, Reregister: reregister},
		"shell", "getprop", "sys.boot_completed")
	return res.Code == 0 && strings.TrimSpace(res.Out) == "1"
}

// ---- guest commands ----------------------------------------------------------------------------------------------

func (m *Manager) shell(ctx context.Context, i int, command string, timeout time.Duration) adb.Result {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return m.ADB.Shell(ctx, root.Serial(i), command, timeout)
}

// su runs a guest script as root. These userdebug images allow `su 0` without `adb root` (which restarts adbd and
// drops the scripts' own adb connections, so it is never used). The script goes inside single quotes.
func (m *Manager) su(ctx context.Context, i int, script string, timeout time.Duration) adb.Result {
	if strings.Contains(script, "'") {
		return adb.Result{Code: -1, Out: "客体脚本里不能有单引号"}
	}
	return m.shell(ctx, i, "su 0 sh -c '"+script+"'", timeout)
}

// push copies a host file into the guest; a failure is logged as a warning.
func (m *Manager) push(ctx context.Context, i int, local, remote string) bool {
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: 60 * time.Second, Reregister: true},
		"push", local, remote)
	if res.Code != 0 {
		m.Log.Warnf("实例 %d 推送 %s 失败: %s", i, filepath.Base(local), tail(res.Out, 300))
	}
	return res.Code == 0
}

// pushGuestScript writes an embedded guest script to a host temp file and pushes it to /data/local/tmp.
func (m *Manager) pushGuestScript(ctx context.Context, i int, name string) (string, bool) {
	local, err := guest.LocalFile(filepath.Join(os.TempDir(), "modavd-guest"), name)
	if err != nil {
		m.Log.Warnf("实例 %d 写客体脚本 %s 失败: %v", i, name, err)
		return "", false
	}
	remote := guest.Remote(name)
	return remote, m.push(ctx, i, local, remote)
}

// tail keeps the last n bytes of s (on a rune boundary), whitespace-trimmed.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

// ---- locks -------------------------------------------------------------------------------------------------------

// lockPath is <root>\avd\.mas_<i>.<kind>.lock, kind "start" (held for a whole start), "boot" (from launch until
// Android booted: no re-registration then), "stop" or "watch" (held by the boot watch, see bootwatch.go).
func (m *Manager) lockPath(i int, kind string) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+"."+kind+".lock")
}

func (m *Manager) lock(ctx context.Context, i int, kind string, wait time.Duration, what string) (*win.FileLock, error) {
	path := m.lockPath(i, kind)
	l, ok, err := win.TryLock(path)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
	}
	if ok {
		return l, nil
	}
	m.Log.Infof("实例 %d %s，等它做完再继续", i, what)
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, errs.Wrap(errs.Canceled, err, "已取消")
		}
		l, ok, err = win.Lock(path, 500*time.Millisecond)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
		}
		if ok {
			return l, nil
		}
		if time.Now().After(deadline) {
			return nil, errs.Newf(errs.Busy, "实例 %d %s，等了 %.0f 秒仍没做完", i, what, wait.Seconds())
		}
	}
}

// booting: some modavd process is between launch and boot completion of instance i (holds its boot lock).
func (m *Manager) booting(i int) bool {
	l, ok, err := win.TryLock(m.lockPath(i, "boot"))
	if err != nil || !ok {
		return err == nil
	}
	l.Unlock()
	return false
}

// stopping: some modavd process is stopping instance i.
func (m *Manager) stopping(i int) bool {
	l, ok, err := win.TryLock(m.lockPath(i, "stop"))
	if err != nil || !ok {
		return err == nil
	}
	l.Unlock()
	return false
}

// setupMarkerPath is <root>\avd\.mas_<i>.setup: written by a start that launched qemu (it holds that qemu's PID and
// creation time) and removed once the whole after-boot setup is done. A later start that adopts the same qemu and
// finds the marker treats the boot as fresh: the cancelled / timed-out start never got to the one-time steps.
func (m *Manager) setupMarkerPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".setup")
}

// guestLockPath is <root>\avd\.mas_<i>.guest.lock, held while a modavd process does something in the guest that a
// zygote restart would break (InstallAPK). A start that owes the Vulkan fix of this boot (setup marker) mounts it, which
// restarts zygote, only when it can take this lock.
func (m *Manager) guestLockPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".guest.lock")
}

// removeLockFiles deletes the lock files, the setup marker and the boot record of instance i (after Delete); best
// effort.
func (m *Manager) removeLockFiles(i int) {
	for _, kind := range []string{"start", "boot", "stop", "watch", "bridge", "create"} {
		os.Remove(m.lockPath(i, kind))
	}
	os.Remove(m.setupMarkerPath(i))
	os.Remove(m.guestLockPath(i))
	m.removeBootRecord(i)
}

// ---- instances ---------------------------------------------------------------------------------------------------

func (m *Manager) instance(i int) (*avd.Instance, error) {
	if !root.ValidIndex(i) {
		return nil, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", i)
	}
	return avd.Get(m.Root, i)
}

func (m *Manager) rootInfo() RootInfo {
	info := RootInfo{
		Root:                m.Root.Dir,
		SDK:                 m.Root.RuntimeSDK(),
		LocalSDK:            m.Root.LocalSDKRoot() != "",
		AdbExe:              m.Root.AdbExe(),
		AdbServerPort:       m.Root.AdbServerPort(),
		ScriptAdbServerPort: m.Root.ScriptAdbServerPort(),
		Version:             m.Root.VersionText(),
	}
	if b, ok := m.Root.ModBuild(); ok {
		info.Build = b
	}
	return info
}

func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errs.Wrap(errs.Canceled, err, "已取消")
	}
	return nil
}
