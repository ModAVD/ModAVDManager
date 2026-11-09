package adb

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/root"
	"modavd/internal/win"
)

func writeServerLog(t *testing.T, r *root.Root, text string) {
	t.Helper()
	if err := os.MkdirAll(r.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ServerLogPath(r), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Env points every adb server of the root (and one an adb client starts by itself) at logs\adb-server.log.
func TestEnvServerLogPath(t *testing.T) {
	r := &root.Root{Dir: tempDir(t)}
	want := "ANDROID_ADB_LOG_PATH=" + filepath.Join(r.Dir, "logs", "adb-server.log")
	if env := New(r, nil).Env(); !slices.Contains(env, want) {
		t.Errorf("no %s in the environment", want)
	}
}

// The tail is the newest start (its first line kept when there are more lines than fit), nothing for no log.
func TestServerLogTail(t *testing.T) {
	r := &root.Root{Dir: tempDir(t)}
	if got := ServerLogTail(r); got != nil || ServerLogLast(r) != "" {
		t.Fatalf("no log: %q", got)
	}
	var b strings.Builder
	b.WriteString("--- adb starting (pid 1) ---\r\nold line\r\n")
	b.WriteString("--- adb starting (pid 2) ---\r\n")
	for i := range 10 {
		b.WriteString("line " + string(rune('a'+i)) + "\r\n\r\n")
	}
	b.WriteString("could not install *smartsocket* listener: cannot bind to 127.0.0.1:20050  \r\n")
	writeServerLog(t, r, b.String())
	got := ServerLogTail(r)
	want := []string{"--- adb starting (pid 2) ---", "line e", "line f", "line g", "line h", "line i", "line j",
		"could not install *smartsocket* listener: cannot bind to 127.0.0.1:20050"}
	if !slices.Equal(got, want) {
		t.Errorf("tail\n got  %q\n want %q", got, want)
	}
	if last := ServerLogLast(r); last != want[len(want)-1] {
		t.Errorf("last %q", last)
	}

	// no start marker in what is read: the last lines
	writeServerLog(t, r, strings.Repeat("x\n", 20))
	if got := ServerLogTail(r); len(got) != serverLogTailLines {
		t.Errorf("without a marker: %q", got)
	}
}

// prepareServerLog makes logs\ and moves a log past serverLogMax to .1 (an older .1 goes).
func TestPrepareServerLog(t *testing.T) {
	r := &root.Root{Dir: tempDir(t)}
	prepareServerLog(r)
	if fi, err := os.Stat(r.LogsDir()); err != nil || !fi.IsDir() {
		t.Fatalf("logs dir: %v", err)
	}
	writeServerLog(t, r, "small\n")
	prepareServerLog(r)
	if _, err := os.Stat(ServerLogPath(r) + ".1"); err == nil {
		t.Fatal("a small log was rotated")
	}
	if err := os.WriteFile(ServerLogPath(r)+".1", []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeServerLog(t, r, strings.Repeat("y", serverLogMax+1))
	prepareServerLog(r)
	b, err := os.ReadFile(ServerLogPath(r) + ".1")
	if _, serr := os.Stat(ServerLogPath(r)); err != nil || len(b) != serverLogMax+1 || serr == nil {
		t.Fatalf("rotation: .1 %d bytes (%v), log still there: %v", len(b), err, serr == nil)
	}
}

// A server that did not listen is reported with the end of its log, once while the log says the same.
func TestReportServerLogOncePerContent(t *testing.T) {
	c, logs := hookClient(t)
	writeServerLog(t, c.Root, "--- adb starting (pid 7) ---\nfailed to bind\n")
	c.reportServerLog(privateServer, 20391, 20*time.Second)
	writeServerLog(t, c.Root, "--- adb starting (pid 8) ---\nfailed to bind\n")
	c.reportServerLog(privateServer, 20391, 20*time.Second)
	if n := logs.count("20 秒内没监听上；adb 自己的日志（logs\\adb-server.log）最后几行：\n  --- adb starting (pid 7) ---\n  failed to bind"); n != 1 || logs.count("没监听上") != 1 {
		t.Fatalf("%d reports: %q", n, logs.msgs)
	}
	writeServerLog(t, c.Root, "--- adb starting (pid 9) ---\nsomething else\n")
	c.reportServerLog(privateServer, 20391, 20*time.Second)
	if logs.count("something else") != 1 || logs.count("没监听上") != 2 {
		t.Fatalf("a new reason not reported: %q", logs.msgs)
	}
}

// A server this process saw listen that is gone: exited, or still there without listening. Reported once.
func TestReportGoneServer(t *testing.T) {
	c, logs := hookClient(t)
	writeServerLog(t, c.Root, "--- adb starting (pid 5) ---\nadb server killed by remote request\n")
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	key := serverKey(c.Root, 20391)
	serverState.Lock()
	serverState.started[key] = startedServer{pid: uint32(cmd.Process.Pid), created: 1, at: time.Now()}
	serverState.Unlock()
	c.reportGoneServer(privateServer, 20391)
	c.reportGoneServer(privateServer, 20391)
	if n := logs.count("已经退出了；adb 自己的日志（logs\\adb-server.log）最后几行：\n  --- adb starting (pid 5) ---\n  adb server killed by remote request"); n != 1 {
		t.Fatalf("exited: %d reports: %q", n, logs.msgs)
	}

	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("this process not found")
	}
	serverState.Lock()
	serverState.started[key] = startedServer{pid: self.PID, created: self.Created, at: time.Now()}
	serverState.Unlock()
	c.reportGoneServer(privateServer, 20391)
	if logs.count("还在，但不再监听") != 1 {
		t.Fatalf("alive: %q", logs.msgs)
	}
}
