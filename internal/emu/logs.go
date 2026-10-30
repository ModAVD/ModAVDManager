//go:build windows

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
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// The two kinds of files in the logs dir that are ours to prune: emulator output mas_<i>-<time>.log and guest logcat
// logcat-mas_<i>-<time>.txt. Nothing else there is touched.
var managedLogPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^mas_\d+-.+\.log$`),
	regexp.MustCompile(`^logcat-mas_\d+-.+\.txt$`),
}

// pruneMu: several instances of one process booting at once prune the same directory; other processes are covered
// by tolerating files that vanish meanwhile.
var pruneMu sync.Mutex

type logFile struct {
	path  string
	name  string
	mtime time.Time
	size  int64
}

// pruneLogs deletes old logs before a new one is created (AUTO-MAS host.prune_logs): per instance keep the newest 10
// emulator logs (mas_<i>-*.log) and 10 logcats (logcat-mas_<i>-*.txt); then while the closed files of both kinds in
// the whole logs dir exceed 1 GiB, delete from the oldest. Files in use are skipped and not counted.
func (m *Manager) pruneLogs(i int) {
	pruneMu.Lock()
	defer pruneMu.Unlock()
	if _, _, err := pruneLogDir(m.Root.LogsDir(), i, logKeepPerInstance, logsDirMaxBytes, m.Log); err != nil {
		m.Log.Warnf("实例 %d 清理旧日志失败: %v", i, err)
	}
}

// pruneLogDir is AUTO-MAS _prune_logs_locked with the limits as parameters: returns the files removed and the bytes
// freed. Files being written cannot be deleted on Windows; they are skipped and, for the size limit, not counted:
// otherwise long runs of several instances whose open logs alone exceed the limit would delete every previous log
// at each boot.
func pruneLogDir(dir string, i, keep int, maxBytes int64, log *logx.Logger) (removed int, freed int64, err error) {
	files, err := managedLogFiles(dir)
	if err != nil {
		return 0, 0, err
	}
	avdName := root.AvdName(i)
	gone := map[string]bool{}

	for _, kind := range [][2]string{{avdName + "-", ".log"}, {"logcat-" + avdName + "-", ".txt"}} {
		var mine []logFile
		for _, f := range files {
			if strings.HasPrefix(f.name, kind[0]) && strings.HasSuffix(f.name, kind[1]) {
				mine = append(mine, f)
			}
		}
		sortLogFiles(mine, true)
		if len(mine) <= keep {
			continue
		}
		for _, f := range mine[keep:] {
			result := tryUnlink(f.path, log)
			if result != unlinkFailed {
				gone[f.path] = true
			}
			if result == unlinkRemoved {
				removed++
				freed += f.size
			}
		}
	}

	var rest []logFile
	var total int64
	for _, f := range files {
		if !gone[f.path] {
			rest = append(rest, f)
			total += f.size
		}
	}
	if total > maxBytes {
		var closed []logFile
		for _, f := range rest {
			switch busy, vanished := fileInUse(f.path); {
			case vanished:
			case busy:
				log.Debugf("旧日志 %s 正在写，不算进总量", f.name)
			default:
				closed = append(closed, f)
			}
		}
		sortLogFiles(closed, false)
		var closedTotal int64
		for _, f := range closed {
			closedTotal += f.size
		}
		for _, f := range closed {
			if closedTotal <= maxBytes {
				break
			}
			result := tryUnlink(f.path, log)
			// removed, removed by someone else, or just opened for writing: none of them counts as closed any more
			closedTotal -= f.size
			if result == unlinkRemoved {
				removed++
				freed += f.size
			}
		}
	}

	if removed > 0 {
		log.Infof("实例 %d 建新日志前清理旧日志 %d 份，释放 %.1f MB（%s）", i, removed, float64(freed)/1024/1024, dir)
	}
	return removed, freed, nil
}

// managedLogFiles lists the regular files of both kinds (no symlinks) with mtime and size; none when dir is missing.
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
		if !isManagedLog(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{path: filepath.Join(dir, e.Name()), name: e.Name(), mtime: info.ModTime(),
			size: info.Size()})
	}
	return files, nil
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

// fileInUse: a handle without delete sharing holds the file (the emulator's and logcat's output handles are such
// handles), so it cannot be deleted. A rename onto itself needs delete access like a delete, so it finds those files
// without changing them (AUTO-MAS _in_use). vanished when the file is not there.
func fileInUse(path string) (busy, vanished bool) {
	err := os.Rename(path, path)
	if err == nil {
		return false, false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
		return false, true
	}
	return true, false
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

// findLogcatProcesses are the host's `adb -P <port> -s <serial> logcat` processes, whoever started them: a record of
// our own would not do, a capture an earlier modavd started keeps running.
func (m *Manager) findLogcatProcesses(serial string) ([]win.Process, error) {
	procs, err := win.ListProcesses()
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(m.Root.AdbServerPort())
	var found []win.Process
	for _, p := range procs {
		if !strings.EqualFold(p.Name, "adb.exe") {
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
// A capture client older than the private server now listening was connected to an earlier server that is gone: it
// gets no more data but need not exit (10-04: still there 7 s after a re-registration). The re-registration hook
// ends such clients, but it runs in the background and a short-lived CLI process may exit before it is done; so they
// are ended here too instead of counting as a running capture (AUTO-MAS's watchdog re-checks every 30 s; modavd has
// none, so the guest log would otherwise stay off for the rest of the boot).
func (m *Manager) ensureLogcat(ctx context.Context, i int) string {
	logcatMu.Lock()
	defer logcatMu.Unlock()
	serial := root.Serial(i)
	running, err := m.findLogcatProcesses(serial)
	if err != nil {
		m.Log.Warnf("实例 %d 读不出宿主进程表，这次不起客体日志落盘（免得起两份）: %v", i, err)
		return ""
	}
	if len(running) > 0 {
		live, stale := splitLogcatClients(running, m.privateServerProcess())
		if len(stale) > 0 && ctx.Err() == nil {
			m.Log.Infof("实例 %d 的客体日志落盘（pid %s）连着已经不在的私有 adb server，结束掉", i, pidList(stale))
			// ended first: openLogFile below then no longer sees their files as open
			endProcesses(stale, 5*time.Second)
		}
		if len(live) > 0 {
			return openLogFile(m.Root.LogsDir(), "logcat-"+root.AvdName(i)+"-", ".txt")
		}
	}
	if ctx.Err() != nil {
		return ""
	}
	m.pruneLogs(i)
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(m.Root.LogsDir(), "logcat-"+root.AvdName(i)+"-"+stamp+".txt")
	exe := m.Root.AdbExe()
	// StartOrphan creates the file and its directory
	if _, err := win.StartOrphan(exe, logcatArgs(m.Root.AdbServerPort(), serial), win.StartOptions{
		Dir:        filepath.Dir(exe),
		Env:        m.ADB.Env(),
		OutputPath: path,
	}); err != nil {
		m.Log.Warnf("实例 %d 客体日志落盘没起来: %v", i, err)
		return ""
	}
	m.Log.Infof("实例 %d 客体日志 → %s", i, path)
	return path
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

// logcatAfterReregister restarts the host logcat capture after serial was registered again with a new private
// server (the old adb logcat client is connected to a server that is gone). Hooked into adb.Client.OnReregistered.
//
// Being re-registered means the private server was replaced: the old clients get no more data but do not
// necessarily exit (10-04: still there 7 s after a re-registration), and would make ensureLogcat think the capture
// runs. So they are ended first (AUTO-MAS _logcat_after_reregister).
func (m *Manager) logcatAfterReregister(serial string) {
	port, ok := consolePortOfSerial(serial)
	if !ok {
		return
	}
	i, ok := root.IndexOfConsolePort(port)
	if !ok || !root.ValidIndex(i) {
		return
	}
	if _, err := avd.Get(m.Root, i); err != nil {
		return
	}
	stale, err := m.findLogcatProcesses(serial)
	if err != nil {
		return
	}
	endProcesses(stale, 5*time.Second)
	m.ensureLogcat(context.Background(), i)
}

// privateServerProcess is the adb server listening on the private port, zero when none, when the listener table or
// the process cannot be read, or when what listens there is not adb.exe (no server of ours to judge clients by).
func (m *Manager) privateServerProcess() win.Process {
	pid, ok, err := win.ListenerPID(m.Root.AdbServerPort())
	if err != nil || !ok {
		return win.Process{}
	}
	p, ok := win.FindProcess(pid)
	if !ok || !strings.EqualFold(p.Name, "adb.exe") {
		return win.Process{}
	}
	return p
}

// splitLogcatClients sorts capture clients by the server they can be talking to. stale: created before server, so
// connected to an earlier server that is gone. Except the client that started this server itself (adb's
// fork-server makes the server its child, created a moment after it). Unknown creation times, or no known server,
// count as live: never end a capture that may be working.
func splitLogcatClients(clients []win.Process, server win.Process) (live, stale []win.Process) {
	for _, c := range clients {
		if server.Created != 0 && c.Created != 0 && c.Created < server.Created && server.PPID != c.PID {
			stale = append(stale, c)
		} else {
			live = append(live, c)
		}
	}
	return live, stale
}

// endProcesses kills procs and waits up to timeout in all for them to be gone.
func endProcesses(procs []win.Process, timeout time.Duration) {
	for _, p := range procs {
		p.Kill()
	}
	deadline := time.Now().Add(timeout)
	for _, p := range procs {
		if left := time.Until(deadline); left > 0 {
			p.Wait(left)
		}
	}
}

// pidList is "12, 34" for the log.
func pidList(procs []win.Process) string {
	pids := make([]string, len(procs))
	for k, p := range procs {
		pids[k] = strconv.FormatUint(uint64(p.PID), 10)
	}
	return strings.Join(pids, ", ")
}
