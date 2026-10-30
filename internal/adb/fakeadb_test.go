//go:build windows

package adb

// A stand-in adb.exe, so PrepareForClose and Reregister can be tested without a real adb server or emulator: this test
// binary, copied to <root>\sdk\platform-tools\adb.exe (the listener check wants a process named adb.exe), runs as
//   - the server (MODAVD_FAKE_ADB=server): listens on MODAVD_FAKE_ADB_PORT and records each "host:emulator:<port>"
//     it is sent in the state file;
//   - the client (MODAVD_FAKE_ADB=client, reaching it through Client.Env): "start-server" starts the server detached,
//     "get-state" answers "device" once the state file has the serial (registered, or preset), else real adb's
//     "device not found" error.
//
// The server port is a free ephemeral one; the test refuses to go on unless the root reads it back exactly, so no
// real adb server (5037, 20049, 20050, 20089) is ever involved.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

const (
	fakeAdbModeEnv  = "MODAVD_FAKE_ADB"
	fakeAdbStateEnv = "MODAVD_FAKE_ADB_STATE"
	fakeAdbPortEnv  = "MODAVD_FAKE_ADB_PORT"
)

func TestMain(m *testing.M) {
	switch os.Getenv(fakeAdbModeEnv) {
	case "server":
		os.Exit(fakeAdbServer())
	case "client":
		os.Exit(fakeAdbClient(os.Args[1:]))
	}
	os.Exit(m.Run())
}

var fakeStateMu sync.Mutex

func appendState(path, line string) {
	fakeStateMu.Lock()
	defer fakeStateMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func stateLines(path string) []string {
	b, _ := os.ReadFile(path)
	return strings.Fields(strings.ReplaceAll(string(b), "\n", " "))
}

func fakeAdbServer() int {
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv(fakeAdbPortEnv))
	if err != nil {
		return 1
	}
	state := os.Getenv(fakeAdbStateEnv)
	go func() { // never outlive the test by much
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return 1
		}
		go func() {
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, _ := io.ReadAll(conn) // Register closes the connection itself
			if msg := string(b); len(msg) > 4 {
				if port, ok := strings.CutPrefix(msg[4:], "host:emulator:"); ok {
					appendState(state, "registered:"+port)
				}
			}
		}()
	}
}

func fakeAdbClient(args []string) int {
	var port, serial string
	for len(args) >= 2 && (args[0] == "-P" || args[0] == "-s") {
		if args[0] == "-P" {
			port = args[1]
		} else {
			serial = args[1]
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return 1
	}
	switch args[0] {
	case "start-server":
		exe, err := os.Executable()
		if err != nil {
			return 1
		}
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), fakeAdbModeEnv+"=server", fakeAdbPortEnv+"="+port)
		if err := cmd.Start(); err != nil {
			return 1
		}
		cmd.Process.Release()
		// like adb: return once the server accepts
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
				c.Close()
				return 0
			}
		}
		return 1
	case "get-state":
		console, _ := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
		for _, line := range stateLines(os.Getenv(fakeAdbStateEnv)) {
			if line == "device:"+serial || line == "registered:"+strconv.Itoa(console+1) {
				fmt.Println("device")
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "error: device '%s' not found\n", serial)
		return 1
	}
	return 1
}

type fakeAdb struct {
	c     *Client
	logs  *logCollector
	state string
	port  int
}

func (f *fakeAdb) registered() []string {
	var out []string
	for _, line := range stateLines(f.state) {
		if strings.HasPrefix(line, "registered:") {
			out = append(out, line)
		}
	}
	return out
}

// reset sets the state file to lines and forgets the log so far.
func (f *fakeAdb) reset(t *testing.T, lines ...string) {
	t.Helper()
	if err := os.WriteFile(f.state, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	f.logs.mu.Lock()
	f.logs.msgs = nil
	f.logs.mu.Unlock()
}

func (f *fakeAdb) killServer(t *testing.T) {
	t.Helper()
	pid, up, err := win.ListenerPID(f.port)
	if err != nil || !up {
		return
	}
	if p, ok := win.FindProcess(pid); ok && isAdbName(p.Name) {
		p.Kill()
		p.Wait(5 * time.Second)
	}
}

// newFakeAdb is a root whose private server is the fake adb on a free port.
func newFakeAdb(t *testing.T) *fakeAdb {
	t.Helper()
	dir := tempDir(t)
	port := freePort(t)
	script := freePort(t)
	for _, p := range []int{port, script} {
		if p == 5037 || p == root.DefaultAdbServerPort || p == root.DefaultScriptAdbServerPort || p == 20089 ||
			(p >= 20000 && p <= 20099) {
			t.Skipf("free port %d is one of the reserved ones", p)
		}
	}
	meta := fmt.Sprintf(`{"adbServerPort": %d, "scriptAdbServerPort": %d}`, port, script)
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.AdbServerPort(); got != port {
		t.Fatalf("测试根目录的 adbServerPort 读成了 %d（应为 %d），不往下测", got, port)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.AdbExe()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.AdbExe(), b, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeAdb{logs: &logCollector{}, state: filepath.Join(dir, "fake-adb-state.txt"), port: port}
	f.reset(t)
	t.Setenv(fakeAdbModeEnv, "client")
	t.Setenv(fakeAdbStateEnv, f.state)
	f.c = New(r, logx.New(f.logs.sink))
	t.Cleanup(func() { f.killServer(t) })
	return f
}

// stubQemu makes qemuOnAdbPort answer with p / ok and record its calls.
func stubQemu(t *testing.T, p win.Process, ok bool) *[]int {
	t.Helper()
	var calls []int
	var mu sync.Mutex
	qemuOnAdbPort = func(r *root.Root, console int) (win.Process, bool) {
		mu.Lock()
		calls = append(calls, console)
		mu.Unlock()
		if r == nil {
			t.Errorf("qemuOnAdbPort without a root")
		}
		return p, ok
	}
	t.Cleanup(func() { qemuOnAdbPort = QemuOnAdbPort })
	return &calls
}

func TestPrepareForCloseFakeAdb(t *testing.T) {
	f := newFakeAdb(t)
	ctx := context.Background()
	const console = 20080
	serial := "emulator-" + strconv.Itoa(console)
	if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok {
		t.Fatalf("fake server did not start: %v %v", ok, err)
	}

	t.Run("listening and known", func(t *testing.T) {
		f.reset(t, "device:"+serial)
		calls := stubQemu(t, win.Process{}, false)
		if reason := f.c.PrepareForClose(ctx, console, 4*time.Second); reason != "" {
			t.Errorf("PrepareForClose = %q", reason)
		}
		if len(*calls) != 0 || len(f.registered()) != 0 {
			t.Errorf("a known instance was checked (%v) or registered (%v)", *calls, f.registered())
		}
	})

	t.Run("listening, unknown, not our qemu", func(t *testing.T) {
		f.reset(t)
		calls := stubQemu(t, win.Process{}, false)
		reason := f.c.PrepareForClose(ctx, console, 4*time.Second)
		if want := "adb 端口 20081 上不是这台实例的模拟器，不替它登记"; reason != want {
			t.Errorf("PrepareForClose = %q, want %q", reason, want)
		}
		if len(*calls) != 1 || (*calls)[0] != console {
			t.Errorf("qemu check calls = %v", *calls)
		}
		if got := f.registered(); len(got) != 0 {
			t.Errorf("registered %v for a qemu that is not ours", got)
		}
	})

	t.Run("listening, unknown, our qemu", func(t *testing.T) {
		f.reset(t)
		stubQemu(t, win.Process{PID: 1}, true)
		start := time.Now()
		if reason := f.c.PrepareForClose(ctx, console, 4*time.Second); reason != "" {
			t.Errorf("PrepareForClose = %q", reason)
		}
		t.Logf("registered within %v", time.Since(start))
		if got := f.registered(); len(got) != 1 || got[0] != "registered:20081" {
			t.Errorf("registrations = %v", got)
		}
		if f.logs.count("私有 adb server（端口 "+strconv.Itoa(f.port)+"）上没有 "+serial+"，关机前替它登记") != 1 ||
			f.logs.count("关机前已向私有 adb server 登记 "+serial) != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})

	t.Run("listening, get-state does not answer in time", func(t *testing.T) {
		// Not "device not found": left to the shutdown commands that follow, as before.
		f.reset(t)
		calls := stubQemu(t, win.Process{PID: 1}, true)
		if reason := f.c.PrepareForClose(ctx, console, time.Millisecond); reason != "" {
			t.Errorf("PrepareForClose with 1 ms = %q", reason)
		}
		if len(*calls) != 0 || len(f.registered()) != 0 {
			t.Errorf("calls %v, registrations %v", *calls, f.registered())
		}
	})

	t.Run("server down", func(t *testing.T) {
		f.killServer(t)
		f.reset(t)
		stubQemu(t, win.Process{PID: 1}, true)
		if reason := f.c.PrepareForClose(ctx, console, 10*time.Second); reason != "" {
			t.Errorf("PrepareForClose = %q", reason)
		}
		if got := f.registered(); len(got) != 1 {
			t.Errorf("registrations = %v", got)
		}
		if f.logs.count("关机前已向新起的私有 adb server 登记 "+serial) != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})
}

func TestReregisterFakeAdb(t *testing.T) {
	f := newFakeAdb(t)
	ctx := context.Background()
	const serial = "emulator-20080"
	if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok {
		t.Fatalf("fake server did not start: %v %v", ok, err)
	}
	old := win.Process{PID: 1, Created: filetimeOf(time.Now().Add(-2 * time.Hour))}

	t.Run("another root's qemu", func(t *testing.T) {
		f.reset(t)
		calls := stubQemu(t, win.Process{}, false)
		if f.c.Reregister(ctx, serial) {
			t.Error("Reregister = true")
		}
		if len(*calls) != 1 || len(f.registered()) != 0 {
			t.Errorf("calls %v, registrations %v", *calls, f.registered())
		}
	})

	t.Run("young qemu", func(t *testing.T) {
		f.reset(t)
		stubQemu(t, win.Process{PID: 1, Created: filetimeOf(time.Now())}, true)
		if f.c.Reregister(ctx, serial) || len(f.registered()) != 0 {
			t.Errorf("a qemu younger than 60 s was registered: %v", f.registered())
		}
	})

	t.Run("ours, hook waited for", func(t *testing.T) {
		f.reset(t)
		stubQemu(t, old, true)
		hooked := make(chan string, 1)
		f.c.OnReregistered = func(s string) {
			time.Sleep(300 * time.Millisecond) // a hook that outlives the command
			hooked <- s
		}
		defer func() { f.c.OnReregistered = nil }()
		if !f.c.Reregister(ctx, serial) {
			t.Fatalf("Reregister = false: %q", f.logs.msgs)
		}
		if got := f.registered(); len(got) != 1 || got[0] != "registered:20081" {
			t.Errorf("registrations = %v", got)
		}
		f.c.WaitHooks(5 * time.Second)
		select {
		case s := <-hooked:
			if s != serial {
				t.Errorf("hook ran for %q", s)
			}
		default:
			t.Error("WaitHooks returned before the hook was done")
		}
	})

	t.Run("Run re-registers once", func(t *testing.T) {
		f.reset(t)
		stubQemu(t, old, true)
		res := f.c.Run(ctx, RunOptions{Serial: serial, Timeout: 10 * time.Second, Reregister: true}, "get-state")
		if res.Code != 0 || res.Out != "device" || len(f.registered()) != 1 {
			t.Errorf("Run = %d %q, registrations %v", res.Code, res.Out, f.registered())
		}
	})
}
