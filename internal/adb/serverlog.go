package adb

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/host"
	"modavd/internal/root"
)

// The adb servers a root starts write their own log to <root>\logs\adb-server.log (ANDROID_ADB_LOG_PATH in Env), not
// to %TEMP%\adb.log, which every adb server of the Windows user shares. When a server does not come up, or one that
// came up is gone again, its last lines say why: cannot bind, killed by remote request, a fatal check. A log that
// just stops says that something ended the process from outside. Without it, a server that never listened was
// a 20 s wait with no reason, again and again (a user's create waited 300 s for an Android that had booted after 72 s).

// ServerLogName is the adb server log in the root's logs\.
const ServerLogName = "adb-server.log"

const (
	// serverLogMax: past this the log moves to .1 before the next server start (a running server keeps it open,
	// and then it stays). Every start writes a few lines; this is thousands of starts.
	serverLogMax = 2 << 20
	// serverLogTailRead and serverLogTailLines bound what ServerLogTail reads and returns.
	serverLogTailRead  = 64 << 10
	serverLogTailLines = 8
	serverStartMarker  = "--- adb starting"
)

// ServerLogPath is <root>\logs\adb-server.log.
func ServerLogPath(r *root.Root) string { return filepath.Join(r.LogsDir(), ServerLogName) }

// prepareServerLog makes the logs directory (adb cannot open its log without it, and a server that cannot open its
// log exits) and moves a log past serverLogMax to .1.
func prepareServerLog(r *root.Root) {
	path := ServerLogPath(r)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if fi, err := os.Stat(path); err == nil && fi.Size() > serverLogMax {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
}

// ServerLogTail is the newest server start in the log: its "--- adb starting (pid N) ---" line and the last lines
// after it, at most serverLogTailLines; nil when the log is missing or empty.
func ServerLogTail(r *root.Root) []string {
	f, err := root.OpenShared(ServerLogPath(r))
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > serverLogTailRead {
		if _, err := f.Seek(-serverLogTailRead, io.SeekEnd); err != nil {
			return nil
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	text := DecodeOutput(b)
	if i := strings.LastIndex(text, serverStartMarker); i >= 0 {
		text = text[i:]
	}
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > serverLogTailLines && strings.HasPrefix(lines[0], serverStartMarker) {
		lines = append(lines[:1:1], lines[len(lines)-(serverLogTailLines-1):]...)
	} else if len(lines) > serverLogTailLines {
		lines = lines[len(lines)-serverLogTailLines:]
	}
	return lines
}

// ServerLogLast is the last line of ServerLogTail, "" when there is none: the one-line reason for a message.
func ServerLogLast(r *root.Root) string {
	lines := ServerLogTail(r)
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// startedServer is a server this process started and saw listening.
type startedServer struct {
	pid     uint32
	created uint64
	at      time.Time
}

var serverState = struct {
	sync.Mutex
	started  map[string]startedServer // by serverKey
	reported map[string]string        // the log lines last reported, by serverKey
}{started: map[string]startedServer{}, reported: map[string]string{}}

func serverKey(r *root.Root, port int) string {
	return strings.ToLower(filepath.Clean(r.Dir)) + "|" + strconv.Itoa(port)
}

// rememberServer records who listens on port now, after this process started a server there.
func (c *Client) rememberServer(port int) {
	pid, ok, err := host.ListenerPID(port)
	if err != nil || !ok || pid == 0 {
		return
	}
	s := startedServer{pid: pid, at: time.Now()}
	if p, found := host.FindProcess(pid); found {
		s.created = p.Created
	}
	serverState.Lock()
	serverState.started[serverKey(c.Root, port)] = s
	serverState.Unlock()
}

// reportGoneServer: nothing listens on port, but this process saw a server it started listen there. Says whether
// that server has exited or is still there without listening, with the end of the log; forgets it.
func (c *Client) reportGoneServer(k serverKind, port int) {
	key := serverKey(c.Root, port)
	serverState.Lock()
	s, ok := serverState.started[key]
	delete(serverState.started, key)
	serverState.Unlock()
	if !ok {
		return
	}
	p, found := host.FindProcess(s.pid)
	what := "已经退出了"
	if found && (s.created == 0 || p.Created == s.created) {
		what = "还在，但不再监听"
	}
	c.Log.Warnf("ModAVD %s adb server（端口 %d，pid %d，%s 起来的）%s%s", k.label, port, s.pid,
		s.at.Format("15:04:05"), what, logTailText(ServerLogTail(c.Root)))
}

// reportServerLog says why a server started for port did not listen within timeout: the end of its log, once per
// distinct content (a server that fails the same way every 20 s is reported once).
func (c *Client) reportServerLog(k serverKind, port int, timeout time.Duration) {
	lines := ServerLogTail(c.Root)
	sig := ""
	if len(lines) > 1 {
		sig = strings.Join(lines[1:], "\n") // without the line that names the pid
	}
	key := serverKey(c.Root, port)
	serverState.Lock()
	same := len(lines) > 0 && serverState.reported[key] == sig
	serverState.reported[key] = sig
	serverState.Unlock()
	if same {
		return
	}
	c.Log.Warnf("ModAVD %s adb server（端口 %d）%.0f 秒内没监听上%s", k.label, port, timeout.Seconds(),
		logTailText(lines))
}

// logTailText is "; adb 自己的日志（logs\adb-server.log）最后几行：" and the lines, indented, or a note that the log
// has nothing.
func logTailText(lines []string) string {
	if len(lines) == 0 {
		return `；adb 自己的日志（logs\` + ServerLogName + `）里没有记录`
	}
	return fmt.Sprintf("；adb 自己的日志（logs\\%s）最后几行：\n  %s", ServerLogName, strings.Join(lines, "\n  "))
}
