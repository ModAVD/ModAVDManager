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
	"slices"
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
			msg, err := readHexPrefixed(conn)
			if err != nil {
				return
			}
			if msg == "host:devices" { // the serials registered with this server (ShareWithScriptServer)
				var list strings.Builder
				for _, line := range stateLines(state) {
					if p, ok := strings.CutPrefix(line, "on:"+os.Getenv(fakeAdbPortEnv)+":"); ok {
						n, _ := strconv.Atoi(p)
						fmt.Fprintf(&list, "emulator-%d\tdevice\n", n-1)
					}
				}
				fmt.Fprintf(conn, "OKAY%04x%s", list.Len(), list.String())
				return
			}
			if port, ok := strings.CutPrefix(msg, "host:emulator:"); ok {
				appendState(state, "registered:"+port)
				appendState(state, "on:"+os.Getenv(fakeAdbPortEnv)+":"+port)
			}
			io.Copy(io.Discard, conn) // Register closes the connection itself
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
		switch fakeDeviceState(serial) {
		case "device":
			fmt.Println("device")
			return 0
		case "offline":
			fmt.Fprintln(os.Stderr, "error: device offline")
			return 1
		}
		fmt.Fprintf(os.Stderr, "error: device '%s' not found\n", serial)
		return 1
	case "reconnect": // the server drops serial's connection; an emulator's does not come back by itself
		appendState(os.Getenv(fakeAdbStateEnv), "kicked:"+serial)
		fmt.Printf("reconnecting %s [offline]\n", serial)
		return 0
	case "partial-hang": // a command that gives some output, then hangs (RunOptions.Stdout)
		fmt.Println("first half")
		time.Sleep(30 * time.Second)
		return 0
	case "partial-fail": // some output, a message on stderr, exit code 3
		fmt.Println("what came")
		fmt.Fprintln(os.Stderr, "boom")
		return 3
	}
	return 1
}

// fakeDeviceState is what the fake server has of serial, by the state file read in order (the last line about it
// wins): "device:<serial>" and "offline:<serial>" preset it, "kicked:<serial>" (reconnect) drops it, and a
// "registered:<adb port>" adds it as device, or as offline when the file has "adbd-down:<serial>" (adbd does not
// answer). "" when the server does not have it.
func fakeDeviceState(serial string) string {
	console, _ := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	lines := stateLines(os.Getenv(fakeAdbStateEnv))
	registered := "device"
	if slices.Contains(lines, "adbd-down:"+serial) {
		registered = "offline"
	}
	state := ""
	for _, line := range lines {
		switch line {
		case "device:" + serial:
			state = "device"
		case "offline:" + serial:
			state = "offline"
		case "kicked:" + serial:
			state = ""
		case "registered:" + strconv.Itoa(console+1):
			state = registered
		}
	}
	return state
}

func (f *fakeAdb) kicked() []string {
	var out []string
	for _, line := range stateLines(f.state) {
		if strings.HasPrefix(line, "kicked:") {
			out = append(out, line)
		}
	}
	return out
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
	if err := os.WriteFile(f.state, []byte(strings.Join(append(lines, ""), "\n")), 0o644); err != nil {
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
		if state, reason := f.c.PrepareForClose(ctx, console, 4*time.Second); reason != "" || state != CloseDevice {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
		}
		if len(*calls) != 0 || len(f.registered()) != 0 || len(f.kicked()) != 0 {
			t.Errorf("a known instance was checked (%v), registered (%v) or dropped (%v)", *calls, f.registered(),
				f.kicked())
		}
	})

	t.Run("listening, unknown, not our qemu", func(t *testing.T) {
		f.reset(t)
		calls := stubQemu(t, win.Process{}, false)
		state, reason := f.c.PrepareForClose(ctx, console, 4*time.Second)
		if want := "adb 端口 20081 上不是这台实例的模拟器，不替它登记"; reason != want || state != CloseNotFound {
			t.Errorf("PrepareForClose = %q, %q, want %q", state, reason, want)
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
		if state, reason := f.c.PrepareForClose(ctx, console, 4*time.Second); reason != "" || state != CloseRegistered {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
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
		if state, reason := f.c.PrepareForClose(ctx, console, time.Millisecond); reason != "" || state != CloseUnknown {
			t.Errorf("PrepareForClose with 1 ms = %q, %q", state, reason)
		}
		if len(*calls) != 0 || len(f.registered()) != 0 {
			t.Errorf("calls %v, registrations %v", *calls, f.registered())
		}
	})

	t.Run("server down", func(t *testing.T) {
		f.killServer(t)
		f.reset(t)
		stubQemu(t, win.Process{PID: 1}, true)
		if state, reason := f.c.PrepareForClose(ctx, console, 10*time.Second); reason != "" || state != CloseRegistered {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
		}
		if got := f.registered(); len(got) != 1 {
			t.Errorf("registrations = %v", got)
		}
		if f.logs.count("关机前已向新起的私有 adb server 登记 "+serial) != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})

	// ModAVD/ModAVD#292: an instance the server shows offline. Left as it was, HOME, sync and the shutdown command all
	// fail on it and qemu is ended without a sync; it gets one try: dropped (reconnect), registered again, online.
	t.Run("offline, recovers", func(t *testing.T) {
		if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok {
			t.Fatalf("fake server: %v %v", ok, err)
		}
		f.reset(t, "offline:"+serial)
		stubQemu(t, win.Process{PID: 1}, true)
		start := time.Now()
		state, reason := f.c.PrepareForClose(ctx, console, 4*time.Second)
		if reason != "" || state != CloseRecovered {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
		}
		if took := time.Since(start); took > 4*time.Second {
			t.Errorf("took %v", took)
		}
		if got := f.kicked(); len(got) != 1 || got[0] != "kicked:"+serial {
			t.Errorf("dropped %v", got)
		}
		if got := f.registered(); len(got) != 1 || got[0] != "registered:20081" {
			t.Errorf("registrations = %v", got)
		}
		if f.logs.count("上 "+serial+" 是 offline，关机前断开重连一次") != 1 || f.logs.count("关机前 "+serial+" 已重新连上") != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})

	t.Run("offline, adbd does not answer", func(t *testing.T) {
		f.reset(t, "offline:"+serial, "adbd-down:"+serial)
		stubQemu(t, win.Process{PID: 1}, true)
		start := time.Now()
		state, reason := f.c.PrepareForClose(ctx, console, 10*time.Second)
		// still offline: left to the shutdown commands, not the console path
		if reason != "" || state != CloseOffline {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
		}
		// the try takes at most offlineRecoverWait after the first get-state, however long the timeout
		if took := time.Since(start); took > offlineRecoverWait+3*time.Second {
			t.Errorf("took %v, want the first get-state and at most %v", took, offlineRecoverWait)
		}
		if len(f.kicked()) != 1 || len(f.registered()) != 1 {
			t.Errorf("dropped %v, registered %v: want one try", f.kicked(), f.registered())
		}
		if f.logs.count("断开重连后仍然不在线") != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})

	t.Run("offline, not our qemu", func(t *testing.T) {
		f.reset(t, "offline:"+serial)
		stubQemu(t, win.Process{}, false)
		if state, reason := f.c.PrepareForClose(ctx, console, 4*time.Second); reason != "" || state != CloseOffline {
			t.Errorf("PrepareForClose = %q, %q", state, reason)
		}
		if len(f.kicked()) != 0 || len(f.registered()) != 0 {
			t.Errorf("another program's port: dropped %v, registered %v", f.kicked(), f.registered())
		}
	})

	t.Run("offline, the try fits what is left of the timeout", func(t *testing.T) {
		f.reset(t, "offline:"+serial, "adbd-down:"+serial)
		stubQemu(t, win.Process{PID: 1}, true)
		start := time.Now()
		if state, _ := f.c.PrepareForClose(ctx, console, 800*time.Millisecond); state != CloseOffline {
			t.Errorf("state = %q", state)
		}
		if took := time.Since(start); took > 800*time.Millisecond+time.Second {
			t.Errorf("took %v with an 800 ms timeout", took)
		}
	})
}

// What the boot watch uses to tell a restarted private server (ModAVD/ModAVD#292): ServerStarts counts the servers
// this process started, ServerPID names who listens now. And the script server: whether it listens, starting it, and
// registering the instance with it once (as emulator-<console>), not again while it lists the instance.
func TestServerStartsAndScriptServer(t *testing.T) {
	f := newFakeAdb(t)
	ctx := context.Background()
	if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok {
		t.Fatalf("fake server: %v %v", ok, err)
	}
	starts, pid := f.c.ServerStarts(), f.c.ServerPID()
	if pid == 0 {
		t.Fatal("ServerPID = 0 with the fake server listening")
	}
	if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok || f.c.ServerStarts() != starts {
		t.Fatalf("a listening server counted as started again: %v %v, %d -> %d", ok, err, starts, f.c.ServerStarts())
	}
	f.killServer(t)
	if got := f.c.ServerPID(); got != 0 {
		t.Errorf("ServerPID = %d with no server", got)
	}
	if ok, err := f.c.EnsureServer(ctx, 10*time.Second); err != nil || !ok {
		t.Fatalf("restart: %v %v", ok, err)
	}
	if f.c.ServerStarts() != starts+1 {
		t.Errorf("ServerStarts %d -> %d after a restart", starts, f.c.ServerStarts())
	}
	if got := f.c.ServerPID(); got == 0 || got == pid {
		t.Errorf("ServerPID %d -> %d after a restart", pid, got)
	}

	scriptPort := f.c.Root.ScriptAdbServerPort()
	if up, err := f.c.ScriptServerUp(); err != nil || up {
		t.Fatalf("script server up before it was started: %v %v", up, err)
	}
	if registered, err := f.c.ShareWithScriptServer(ctx, 20080); err != nil || registered {
		t.Fatalf("registered without a script server: %v %v", registered, err)
	}
	if ok, err := f.c.EnsureScriptServer(ctx, 10*time.Second); err != nil || !ok {
		t.Fatalf("script server: %v %v", ok, err)
	}
	t.Cleanup(func() {
		if pid, up, _ := win.ListenerPID(scriptPort); up {
			if p, ok := win.FindProcess(pid); ok && isAdbName(p.Name) {
				p.Kill()
				p.Wait(5 * time.Second)
			}
		}
	})
	if f.c.ServerStarts() != starts+1 {
		t.Error("the script server counted as a private server start")
	}
	if up, err := f.c.ScriptServerUp(); err != nil || !up {
		t.Fatalf("ScriptServerUp = %v %v", up, err)
	}
	if registered, err := f.c.ShareWithScriptServer(ctx, 20080); err != nil || !registered {
		t.Fatalf("ShareWithScriptServer = %v %v", registered, err)
	}
	if registered, err := f.c.ShareWithScriptServer(ctx, 20080); err != nil || registered {
		t.Errorf("registered again while listed: %v %v", registered, err)
	}
	want := "on:" + strconv.Itoa(scriptPort) + ":20081"
	if n := strings.Count(strings.Join(stateLines(f.state), " "), want); n != 1 {
		t.Errorf("%s registered %d times: %v", want, n, stateLines(f.state))
	}
}

func TestStateOf(t *testing.T) {
	const serial = "emulator-20080"
	for _, c := range []struct {
		res  Result
		want string
	}{
		{Result{Out: "device"}, CloseDevice},
		{Result{Code: 1, Out: "error: device offline"}, CloseOffline},
		{Result{Out: "offline"}, CloseOffline},
		{Result{Code: 1, Out: "error: device 'emulator-20080' not found"}, CloseNotFound},
		{Result{Code: 1, Out: "error: device 'emulator-20090' not found"}, CloseUnknown},
		{Result{Code: 1, Out: "error: device unauthorized."}, CloseUnknown},
		{Result{Code: -1, Out: "adb get-state 超时（5 秒）"}, CloseUnknown},
		{Result{Out: "bootloader"}, CloseUnknown},
	} {
		if got := stateOf(serial, c.res); got != c.want {
			t.Errorf("stateOf(%+v) = %q, want %q", c.res, got, c.want)
		}
	}
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

	// ModAVD/ModAVD#292: an offline instance (the server keeps the connection, so registering it again would be
	// ignored) is dropped first, then registered; the boot guards still apply.
	t.Run("offline, recovers", func(t *testing.T) {
		f.reset(t, "offline:"+serial)
		stubQemu(t, old, true)
		if !f.c.Reregister(ctx, serial) {
			t.Fatalf("Reregister = false: %q", f.logs.msgs)
		}
		if len(f.kicked()) != 1 || len(f.registered()) != 1 {
			t.Errorf("dropped %v, registered %v", f.kicked(), f.registered())
		}
		if f.logs.count(serial+" 是 offline，断开重连一次") != 1 || f.logs.count(serial+" 已重新连上私有 adb server") != 1 {
			t.Errorf("logs = %q", f.logs.msgs)
		}
	})

	t.Run("offline, young qemu", func(t *testing.T) {
		f.reset(t, "offline:"+serial)
		stubQemu(t, win.Process{PID: 1, Created: filetimeOf(time.Now())}, true)
		if f.c.Reregister(ctx, serial) || len(f.kicked()) != 0 || len(f.registered()) != 0 {
			t.Errorf("a qemu younger than 60 s was touched: dropped %v, registered %v", f.kicked(), f.registered())
		}
	})

	t.Run("offline, adbd does not answer", func(t *testing.T) {
		f.reset(t, "offline:"+serial, "adbd-down:"+serial)
		stubQemu(t, old, true)
		start := time.Now()
		if f.c.Reregister(ctx, serial) {
			t.Error("Reregister = true for a device that stays offline")
		}
		if took := time.Since(start); took > reregisterWait+2*time.Second {
			t.Errorf("took %v", took)
		}
		if len(f.kicked()) != 1 || len(f.registered()) != 1 {
			t.Errorf("dropped %v, registered %v: want one try", f.kicked(), f.registered())
		}
	})

	t.Run("Stdout keeps what came before a timeout or a failure", func(t *testing.T) {
		var out strings.Builder
		start := time.Now()
		res := f.c.Run(ctx, RunOptions{Serial: serial, Timeout: time.Second, Stdout: &out}, "partial-hang")
		if res.Code != -1 || !strings.Contains(res.Out, "超时") || out.String() != "first half\n" {
			t.Errorf("hang: %d %q, stdout %q", res.Code, res.Out, out.String())
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("hang returned after %v", d)
		}
		out.Reset()
		res = f.c.Run(ctx, RunOptions{Serial: serial, Timeout: 10 * time.Second, Stdout: &out}, "partial-fail")
		if res.Code != 3 || res.Out != "boom" || res.Stderr != "boom" || out.String() != "what came\n" {
			t.Errorf("fail: %d %q %q, stdout %q", res.Code, res.Out, res.Stderr, out.String())
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
