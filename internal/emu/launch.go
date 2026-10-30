//go:build windows

package emu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// adbServerStartTimeout is how long a launch waits for the private server (AUTO-MAS ensure_adb_server default).
const adbServerStartTimeout = 20 * time.Second

// buildLaunchArgs is the emulator.exe argument list, item for item AUTO-MAS build_launch_args (each switch was
// verified on real instances; the long reasons are in its docstring):
//   - cold boot (-no-snapshot), like MAS does with LDPlayer / MuMu every round;
//   - the boot animation is turned off only when headless: a window has something to show meanwhile;
//   - -audio none and -feature -VirtioSndCard: no host sound and no guest sound card (the image's virtio-snd ignores
//     hw.audio*); qemu CPU on the 1999 main screen 147% -> 104%;
//   - -memory: the instance's own setting (avd.MemoryFor), hw.ramSize is left alone;
//   - the balloon (free page reporting) is always on; QuickbootFileBacked has to be off for it, or the guest RAM is
//     a file mapping whose pages qemu cannot drop;
//   - VulkanBatchedDescriptorSetUpdate: without it Endfield's render thread spent 67.5% in synchronous
//     vkAllocateDescriptorSets round trips;
//   - disk write-through: the guest sends no flushes, yet a hard kill keeps what was written (nobarrier broke 3/3
//     AVDs, this 6/6 survived). Everything after -qemu belongs to QEMU, so it comes last.
//
// The window is not an instance option: it is decided at boot by whoever starts it.
func buildLaunchArgs(avdName string, console, memoryMB int, headless bool) []string {
	args := []string{
		"-avd", avdName,
		"-no-snapshot",
		"-gpu", "host",
		"-ports", fmt.Sprintf("%d,%d", console, console+1),
		"-grpc", strconv.Itoa(console + 2),
		"-grpc-use-token",
		"-audio", "none",
		"-memory", strconv.Itoa(memoryMB),
	}
	if headless {
		args = append(args, "-no-window", "-no-boot-anim")
	}
	return append(args,
		"-feature", "-QuickbootFileBacked",
		"-feature", "-VirtioSndCard",
		"-feature", "VulkanBatchedDescriptorSetUpdate",
		"-qemu",
		"-global", "virtio-blk-device.write-cache=off",
		"-global", "virtio-blk-device.config-wce=off",
		"-device", "virtio-balloon-pci,free-page-reporting=on",
	)
}

// launchPlan is what prepareLaunch settled before emulator.exe is started.
type launchPlan struct {
	args     []string
	logPath  string
	memoryMB int
}

// prepareLaunch is AUTO-MAS _launch up to the Popen: components, host checks against the memory -memory will get,
// free ports, the private adb server, the fixed 720p display, old logs, the log file name. Errors stop the start.
func (m *Manager) prepareLaunch(ctx context.Context, inst *avd.Instance, window bool) (*launchPlan, error) {
	i := inst.Index
	if missing := m.Root.Missing(); len(missing) > 0 {
		return nil, m.Root.MissingError(missing)
	}
	// The same-numbered instance of another root (production D:\ModAVD next to a test root) runs on exactly our
	// ports. Said before the host checks, which would otherwise fail first (it takes the memory) with a message that
	// does not name the real cause.
	if p, where, found := m.otherRootInstance(i); found {
		return nil, otherRootBusy(i, p, where)
	}
	// the host check uses exactly the memory -memory passes below: the instance's own, not one guessed per game
	memoryMB := avd.MemoryFor(m.Root, i)
	if err := m.precheckBeforeLaunch(ctx, memoryMB); err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	port := root.ConsolePort(i)
	busy, err := win.BusyPorts([]int{port, port + 1, port + 2})
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("魔改 AVD 实例 %d 读不出系统的 TCP 监听表: %v", i, err))
	}
	if len(busy) > 0 {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 要用的端口 %s 已被占用，请先关闭占用它们的程序",
			i, joinInts(busy, ", "))
	}

	// The emulator registers with the private server once, at the end of its boot: it has to be up first.
	if ok, err := m.ADB.EnsureServer(ctx, adbServerStartTimeout); err != nil {
		return nil, err
	} else if !ok {
		m.Log.Warnf("私有 adb server（端口 %d）%.0f 秒内没起来，照常开机", m.Root.AdbServerPort(),
			adbServerStartTimeout.Seconds())
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	wrote, err := avd.ApplyDisplay(m.Root, i)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) {
			return nil, e
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("实例 %d 写显示设置（config.ini）失败: %v", i, err))
	}
	if wrote {
		m.Log.Infof("实例 %d 显示改回 720p、不显示导航栏（已写入 config.ini）", i)
	}

	headless := !window
	args := buildLaunchArgs(inst.Name, port, memoryMB, headless)
	m.pruneLogs(i)
	stamp := time.Now().Format("20060102-150405")
	logPath := filepath.Join(m.Root.LogsDir(), inst.Name+"-"+stamp+".log")

	mode := "带窗口"
	if headless {
		mode = "无头"
	}
	cpu := "?"
	if n, ok := inst.CPU(); ok {
		cpu = strconv.Itoa(n)
	}
	m.Log.Infof("启动魔改 AVD 实例 %d（%s，720p %d Hz，%d MB / %s 核，气球开，不带声卡，ANDROID_EMU_VK_HIDE_REBAR=1）: emulator %s",
		i, mode, inst.FPS(), memoryMB, cpu, strings.Join(args, " "))
	return &launchPlan{args: args, logPath: logPath, memoryMB: memoryMB}, nil
}

// otherRootInstance is the process listening on one of instance i's ports (console, adb, gRPC) when it is the
// same-numbered instance of another root: a qemu-system* whose command line has "-avd mas_<i>" and "-ports
// <console>,..." (cmdlineMatches) that adb.QemuBelongsTo does not give to m.Root. where names that instance: the
// avd.dir of its emulator runtime file (in adb.EmulatorDiscoveryDir, where qemu writes it and adb.QemuBelongsTo
// reads it), else its executable. found=false also when the listener table or a process cannot be read: the port
// check after the host checks still reports busy ports.
func (m *Manager) otherRootInstance(i int) (p win.Process, where string, found bool) {
	console := root.ConsolePort(i)
	ls, err := win.Listeners()
	if err != nil {
		return win.Process{}, "", false
	}
	seen := map[uint32]bool{}
	for _, l := range ls {
		if l.Port < console || l.Port > console+2 || l.PID == 0 || seen[l.PID] {
			continue
		}
		seen[l.PID] = true
		q, ok := win.FindProcess(l.PID)
		if !ok {
			continue
		}
		cmdline, err := q.CommandLine()
		if err != nil {
			continue
		}
		if otherRootQemu(q, cmdline, i, func(q win.Process) bool { return adb.QemuBelongsTo(m.Root, q, i) }) {
			return q, qemuWhere(runtimeAvdDir(adb.EmulatorDiscoveryDir(), q), cmdline), true
		}
	}
	return win.Process{}, "", false
}

// otherRootQemu: q (with its command line) is instance i of some root by name and ports, and ours says it is not
// m.Root's.
func otherRootQemu(q win.Process, cmdline []string, i int, ours func(win.Process) bool) bool {
	return isQemuName(q.Name) && cmdlineMatches(cmdline, root.AvdName(i), root.ConsolePort(i)) && !ours(q)
}

// otherRootBusy is the error of a start whose ports the same-numbered instance of another root holds.
func otherRootBusy(i int, p win.Process, where string) *errs.Error {
	return errs.Newf(errs.Busy, "魔改 AVD 实例 %d 的端口被另一个根目录的同编号实例占用（pid %d，%s）", i, p.PID, where).
		WithAdvice("先在那个根目录把它关掉再启动")
}

// qemuWhere names a qemu for the messages: avdDir when known, else its executable (argv[0]), else "路径读不到".
func qemuWhere(avdDir string, cmdline []string) string {
	if avdDir != "" {
		return avdDir
	}
	if len(cmdline) > 0 && cmdline[0] != "" {
		return cmdline[0]
	}
	return "路径读不到"
}

// runtimeAvdDir is the avd.dir= of q's runtime file pid_<pid>.ini in dir; "" when there is no current one (missing,
// unreadable, without that line, or older than q: a leftover of an earlier process with that PID).
func runtimeAvdDir(dir string, q win.Process) string {
	path := filepath.Join(dir, "pid_"+strconv.FormatUint(uint64(q.PID), 10)+".ini")
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	if started := q.StartedAt(); !started.IsZero() && st.ModTime().Before(started) {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range outputLines(string(b)) {
		if key, value, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(key), "avd.dir") {
			if v := strings.TrimSpace(value); v != "" {
				return v
			}
		}
	}
	return ""
}

// startEmulator starts emulator.exe with StartOrphan (AUTO-MAS launch_emulator): never as our child, so whoever ends
// modavd's process tree (an MCP client disconnecting, a job object) cannot cut the guest's power. Output goes to
// plan.logPath, the working directory is the emulator's own.
func (m *Manager) startEmulator(i int, plan *launchPlan) (win.Process, error) {
	exe := m.Root.EmulatorExe()
	cache := m.Root.ShaderCacheDir()
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return win.Process{}, errs.Wrap(errs.Internal, err, fmt.Sprintf("建着色器缓存目录 %s 失败: %v", cache, err))
	}
	if err := os.MkdirAll(filepath.Dir(plan.logPath), 0o755); err != nil {
		return win.Process{}, errs.Wrap(errs.Internal, err, fmt.Sprintf("建日志目录 %s 失败: %v",
			filepath.Dir(plan.logPath), err))
	}
	p, err := win.StartOrphan(exe, plan.args, win.StartOptions{
		Dir:        filepath.Dir(exe),
		Env:        m.emulatorEnv(cache),
		OutputPath: plan.logPath,
	})
	if err != nil {
		return win.Process{}, errs.Wrap(errs.LaunchFailed, err,
			fmt.Sprintf("魔改 AVD 实例 %d 启动失败（emulator.exe 没能运行）: %v", i, err))
	}
	return p, nil
}

// emulatorEnv is the private adb environment plus the emulator-only variables (AUTO-MAS launch_emulator):
//   - the host GPU driver's shader cache in <root>\shader-cache, apart from the user's other programs, so it can be
//     wiped by us (launcher -ShaderCache);
//   - ANDROID_EMU_VK_HIDE_REBAR=1: the host's "device local + host visible" memory type (NVIDIA ReBAR) is reported to
//     the guest as device local only. Endfield waited for write-combine drains on every vertex write there (title
//     screen 14.6 -> 60 fps); games that never touch that memory are unaffected, emulators before mas-27 ignore it.
//
// Appended after the private adb environment: win.StartOrphan / StartDetached keep the last of entries with the same
// name (compared case-insensitively), so these override a user's own values, as AUTO-MAS env.update does.
func (m *Manager) emulatorEnv(shaderCache string) []string {
	return append(m.ADB.Env(),
		"__GL_SHADER_DISK_CACHE=1",
		"__GL_SHADER_DISK_CACHE_PATH="+shaderCache,
		"__GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1",
		"ANDROID_EMU_VK_HIDE_REBAR=1",
	)
}

// logTail is the last n non-empty lines of the emulator log joined by " | " (AUTO-MAS _log_tail), for the error when
// emulator.exe exits before qemu shows up. Only the last 64 KiB are read.
func logTail(path string, n int) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const window = 64 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		if _, err := f.Seek(st.Size()-window, io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range outputLines(strings.ToValidUTF8(string(b), "\uFFFD")) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// fileContains reports whether the file holds needle (the emulator log's modavd_ime line).
func fileContains(path, needle string) bool {
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), needle)
}

func joinInts(v []int, sep string) string {
	s := make([]string, len(v))
	for k, n := range v {
		s[k] = strconv.Itoa(n)
	}
	return strings.Join(s, sep)
}

// outputLines splits like Python's str.splitlines for the separators adb output has (\r\n, \n, \r); no trailing
// empty element.
func outputLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func regularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
