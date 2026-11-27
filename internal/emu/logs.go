package emu

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/avd"
	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// What in the logs dir is ours to prune: the files emulator output mas_<i>-<time>.log and guest logcat
// logcat-mas_<i>-<time>.txt, bridge logs modavd-bridge-<i>-<time>.log, and failed boots mas_<i>-<time>-bootfail.
// Nothing else there is touched.
var (
	managedLogPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^mas_\d+-.+\.log$`),
		regexp.MustCompile(`^logcat-mas_\d+-.+\.txt$`),
		regexp.MustCompile(`^modavd-bridge-\d+-[0-9]{8}-[0-9]{6}\.log$`),
	}
	bootDiagDirPattern = regexp.MustCompile(`^mas_\d+-[0-9]{8}-[0-9]{6}` + bootDiagSuffix + `$`)
)

type logFile struct {
	info  fs.FileInfo // identity selected for deletion; never rebind a candidate after a path replacement
	path  string
	name  string
	mtime time.Time
	size  int64 // a directory's: its files together
	dir   bool  // a failed boot's directory
}

// pruneLogs deletes old logs before a new one is created (AUTO-MAS host.prune_logs): per instance keep the newest 10
// emulator logs (mas_<i>-*.log), 10 logcats (logcat-mas_<i>-*.txt), 10 bridge logs, and the newest 3 failed boots
// (mas_<i>-*-bootfail); then while the closed files of these kinds in the whole logs dir exceed 1 GiB, delete from the
// oldest. Files in use are skipped and not counted.
func (m *Manager) pruneLogs(i int) {
	removed, freed, err := m.cleanupDiagnostics(context.Background(), i)
	if err != nil {
		m.Log.Warnf("实例 %d 清理旧日志失败: %v", i, err)
	}
	if removed > 0 {
		m.Log.Infof("实例 %d 建新日志前清理旧日志 %d 份，释放 %.1f MB（%s）", i, removed, float64(freed)/1024/1024, m.Root.LogsDir())
	}
}

// pruneLogDir is the ordinary-policy entry used by filesystem regression tests.
func pruneLogDir(dir string, i, keep int, maxBytes int64, log *logx.Logger) (int, int64, error) {
	files, err := managedLogFiles(dir)
	if err != nil {
		return 0, 0, err
	}
	plan := planCleanup(closedLogs(files), i, keep, maxBytes, nil, false, time.Now())
	removed, freed, err := applyCleanup(plan, log)
	if removed > 0 {
		log.Infof("实例 %d 建新日志前清理旧日志 %d 份，释放 %.1f MB（%s）", i, removed, float64(freed)/1024/1024, dir)
	}
	return removed, freed, err
}

// managedLogFiles lists the managed regular files and the failed boots' directories (no symlinks) with mtime and
// size; none when dir is missing.
func managedLogFiles(dir string) ([]logFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var files []logFile
	for _, e := range entries {
		isDir := e.IsDir() && bootDiagDirPattern.MatchString(e.Name())
		if !isDir && (!isManagedLog(e.Name()) || !e.Type().IsRegular()) {
			continue
		}
		info, err := snapshotLog(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != isDir {
			continue
		}
		f := logFile{path: filepath.Join(dir, e.Name()), name: e.Name(), mtime: info.ModTime(), size: info.Size(), dir: isDir, info: info}
		if isDir {
			f.size = dirSize(f.path)
		}
		files = append(files, f)
	}
	return files, nil
}

// dirSize is the size of the regular files directly in dir (a failed boot's directory has no subdirectories).
func dirSize(dir string) int64 {
	entries, _ := os.ReadDir(dir)
	var n int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil {
			n += info.Size()
		}
	}
	return n
}

func isManagedLog(name string) bool {
	for _, p := range managedLogPatterns {
		if p.MatchString(name) {
			return true
		}
	}
	return false
}

// sortLogFiles orders by (mtime, name), newest first when desc.
func sortLogFiles(files []logFile, desc bool) {
	sort.SliceStable(files, func(a, b int) bool {
		if desc {
			a, b = b, a
		}
		fa, fb := files[a], files[b]
		if !fa.mtime.Equal(fb.mtime) {
			return fa.mtime.Before(fb.mtime)
		}
		return fa.name < fb.name
	})
}

type unlinkResult int

const (
	unlinkRemoved unlinkResult = iota
	unlinkFailed               // still there (open in another process)
	unlinkGone                 // not there any more (someone else deleted it)
)

// removeLog removes f: tryUnlink for a file, the whole tree for a failed boot's directory.
func removeLog(f logFile, log *logx.Logger) unlinkResult {
	if f.info != nil {
		return unlinkRetained(f, log)
	}
	if !f.dir {
		return tryUnlink(f.path, log)
	}
	if _, err := os.Lstat(f.path); errors.Is(err, fs.ErrNotExist) {
		return unlinkGone
	}
	if err := os.RemoveAll(f.path); err != nil {
		log.Debugf("旧的开机失败现场 %s 删不掉，跳过: %v", f.name, err)
		return unlinkFailed
	}
	return unlinkRemoved
}

func tryUnlink(path string, log *logx.Logger) unlinkResult {
	err := os.Remove(path)
	if err == nil {
		return unlinkRemoved
	}
	if errors.Is(err, fs.ErrNotExist) {
		return unlinkGone
	}
	// another process deleting it at the same moment makes Windows say access denied, not "not found": look again
	if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
		return unlinkGone
	}
	// a file still being written (emulator, logcat) cannot be deleted on Windows: next time
	log.Debugf("旧日志 %s 删不掉，跳过: %v", filepath.Base(path), err)
	return unlinkFailed
}

// fileInUse checks native open-vnode ownership on macOS. Windows retains its
// rename-to-self delete-sharing probe (AUTO-MAS _in_use). vanished means missing.
func fileInUse(path string) (busy, vanished bool) {
	return platformFileInUse(path)
}

// ---- logcat capture ------------------------------------------------------------------------------------------------

// logcatArgs is the capture's adb command line (AUTO-MAS start_logcat).
func logcatArgs(serverPort int, serial string) []string {
	return []string{"-P", strconv.Itoa(serverPort), "-s", serial, "logcat", "-v", "threadtime", "-b",
		"main,system,crash,events"}
}

// logcatCmdlineMatches: an adb command line with "-P <port>", "-s <serial>" and "logcat" (AUTO-MAS
// find_logcat_processes).
func logcatCmdlineMatches(cmdline []string, port, serial string) bool {
	hasPort, hasSerial, hasLogcat := false, false, false
	for k, a := range cmdline {
		if a == "logcat" {
			hasLogcat = true
		}
		if k+1 < len(cmdline) {
			switch {
			case a == "-P" && cmdline[k+1] == port:
				hasPort = true
			case a == "-s" && cmdline[k+1] == serial:
				hasSerial = true
			}
		}
	}
	return hasPort && hasSerial && hasLogcat
}

var logcatListProcesses = host.ListProcesses

// findLogcatProcesses recognizes any matching adb capture, including AUTO-MAS watchdog clients.
// Discovery never grants permission to terminate a process; only ownedCapture does.
func (m *Manager) findLogcatProcesses(serial string) ([]host.Process, error) {
	procs, err := logcatListProcesses()
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(m.Root.AdbServerPort())
	var found []host.Process
	for _, p := range procs {
		if !strings.EqualFold(p.Name, root.CurrentPlatform().ADB) {
			continue
		}
		cmdline, err := p.CommandLine()
		if err != nil {
			continue
		}
		if logcatCmdlineMatches(cmdline, port, serial) {
			found = append(found, p)
		}
	}
	return found, nil
}

// logcatMu: within one process the boot path and a re-registration hook (another goroutine) may both find no capture
// and start one each.
var logcatMu sync.Mutex

// ensureLogcat starts a capture to <root>\logs\logcat-mas_<i>-<time>.txt unless one runs for this instance
// (AUTO-MAS ensure_logcat). Returns the capture file: the new one, or the newest open logcat of this instance when one
// was already running ("" when unknown). The capture ends by itself when the emulator goes away.
//
// A helper's PID and creation time, its root-local executable and its exact adb child identity establish ownership.
// The helper observes policy and private-server changes and replaces only its own stream.
func (m *Manager) ensureLogcat(ctx context.Context, i int) string {
	r, err := m.Root.SnapshotRuntime()
	if err != nil {
		m.Log.Warnf("实例 %d 无法确认日志采集的 SDK/adb 目标，本次不新起采集: %v", i, err)
		return ""
	}
	return m.withRuntime(r).ensureLogcatRuntime(ctx, i)
}

func (m *Manager) ensureLogcatRuntime(ctx context.Context, i int) string {
	logcatMu.Lock()
	defer logcatMu.Unlock()
	serial := m.Root.Ports().Serial(i)
	running, err := m.findLogcatProcesses(serial)
	// Another process may have started the writer but not adb yet, or killed stale adb while its writer drains.
	// Let that transition finish before deciding whether to create a replacement.
	for deadline := time.Now().Add(5 * time.Second); err == nil && len(running) == 0 && m.lockHeld(i, "logcat"); {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return ""
		}
		time.Sleep(20 * time.Millisecond)
		running, err = m.findLogcatProcesses(serial)
	}
	if err != nil {
		m.Log.Warnf("实例 %d 读不出宿主进程表，这次不起客体日志落盘（免得起两份）: %v", i, err)
		return ""
	}
	if len(running) > 0 {
		state, _ := m.readCaptureState(i)
		if _, ours := m.ownedCapture(i, state); !ours {
			m.warnExternalCapture(i, m.diagnosticPolicy(i))
		}
		return openLogFile(m.Root.LogsDir(), "logcat-"+root.AvdName(i)+"-", ".txt")
	}
	if ctx.Err() != nil {
		return ""
	}
	q, found, err := m.qemuOf(i)
	if err != nil || !found || q.Created == 0 || !q.Alive() {
		m.Log.Warnf("实例 %d 找不到仍在运行的 qemu，这次不起客体日志采集", i)
		return ""
	}
	// The writer must outlive this CLI, just like adb. The helper owns both the pipe and the rotating files.
	exe := filepath.Join(m.Root.Dir, "tools", root.CurrentPlatform().CLI())
	token, err := captureRuntimeToken(m.Root)
	if err != nil {
		return ""
	}
	p, err := host.StartOrphan(exe, []string{LogcatCaptureArg, m.Root.Dir, strconv.Itoa(i),
		strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10), token}, host.StartOptions{Env: m.ADB.Env()})
	if err != nil {
		m.Log.Warnf("实例 %d 客体日志落盘没起来: %v", i, err)
		return ""
	}
	// Wait for the helper to hold the output before reporting it. Its per-instance lock also covers the interval
	// before adb appears in the process table, including starts from another manager process.
	for deadline := time.Now().Add(10 * time.Second); ; {
		if clients, err := m.findOwnedLogcatProcesses(serial); err == nil && len(clients) > 0 {
			if path := openLogFile(m.Root.LogsDir(), "logcat-"+root.AvdName(i)+"-", ".txt"); path != "" {
				m.Log.Infof("实例 %d 客体日志 → %s", i, path)
				return path
			}
		}
		if !p.Alive() || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if clients, err := m.findLogcatProcesses(serial); err == nil && len(clients) > 0 {
		m.warnExternalCapture(i, m.diagnosticPolicy(i))
		return ""
	}
	m.Log.Warnf("实例 %d 客体日志采集没有就绪", i)
	return ""
}

// openLogFile is the newest <prefix>*<suffix> of the logs dir that some process holds open (a running capture, the
// emulator's output), "" when none.
func openLogFile(dir, prefix, suffix string) string {
	files, err := managedLogFiles(dir)
	if err != nil {
		return ""
	}
	var mine []logFile
	for _, f := range files {
		if strings.HasPrefix(f.name, prefix) && strings.HasSuffix(f.name, suffix) {
			mine = append(mine, f)
		}
	}
	sortLogFiles(mine, true)
	for _, f := range mine {
		if busy, _ := fileInUse(f.path); busy {
			return f.path
		}
	}
	return ""
}

// logcatAfterReregister reapplies the latest guest policy and waits for the owned helper's new server connection.
// Independent logcat -d calls and streams started by other programs are never terminated here.
func (m *Manager) logcatAfterReregister(serial string) {
	port, ok := consolePortOfSerial(serial)
	if !ok {
		return
	}
	i, ok := m.Root.Ports().IndexOfConsole(port)
	if !ok || !root.ValidIndex(i) {
		return
	}
	if _, err := avd.Get(m.Root, i); err != nil {
		return
	}
	if _, err := m.applyDiagnosticGuest(context.Background(), i); err != nil {
		m.Log.Warnf("实例 %d 重新登记后恢复日志缓冲区失败: %v", i, err)
	}
	// The helper watches server identity and replaces only its own adb stream.
	m.ensureLogcat(context.Background(), i)
	if err := m.waitDiagnosticCapture(context.Background(), i, 10*time.Second); err != nil {
		m.Log.Warnf("实例 %d 重新登记后日志采集尚未恢复: %v", i, err)
	}
}

// privateServerProcess is the adb server listening on the private port, zero when none, when the listener table or
// the process cannot be read, or when what listens there is not adb.exe (no server of ours to judge clients by).
func (m *Manager) privateServerProcess() host.Process {
	pid, ok, err := host.ListenerPID(m.Root.AdbServerPort())
	if err != nil || !ok {
		return host.Process{}
	}
	p, ok := host.FindProcess(pid)
	if !ok || !strings.EqualFold(p.Name, root.CurrentPlatform().ADB) {
		return host.Process{}
	}
	return p
}
