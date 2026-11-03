//go:build windows

package emu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// The fake adb of the port plan tests (ModAVD/ModAVD#75): this test binary copied as adb.exe.
// MODAVD_EMU_FAKE_ADB=server listens on MODAVD_EMU_FAKE_ADB_PORT, which is all EnsureServer looks for (a listening
// adb.exe); =client appends its arguments to MODAVD_EMU_FAKE_ADB_LOG and answers as a guest that has booted.
const (
	fakeAdbEnv     = "MODAVD_EMU_FAKE_ADB"
	fakeAdbLogEnv  = "MODAVD_EMU_FAKE_ADB_LOG"
	fakeAdbPortEnv = "MODAVD_EMU_FAKE_ADB_PORT"
)

func TestMain(m *testing.M) {
	switch os.Getenv(fakeAdbEnv) {
	case "server":
		os.Exit(fakeAdbServerMain())
	case "client":
		os.Exit(fakeAdbClientMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeAdbServerMain() int {
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv(fakeAdbPortEnv))
	if err != nil {
		return 1
	}
	time.AfterFunc(2*time.Minute, func() { os.Exit(0) }) // never outlives a test that could not end it
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		c.Close()
	}
}

func fakeAdbClientMain(args []string) int {
	line, _ := json.Marshal(args)
	if f, err := os.OpenFile(os.Getenv(fakeAdbLogEnv), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	all := strings.Join(args, " ")
	switch {
	case strings.Contains(all, "get-state"):
		fmt.Println("device")
	case strings.Contains(all, "sys.boot_completed"):
		fmt.Println("1")
	}
	return 0
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// portPlanRoot is a root with portBase 21000 whose SDK (sdkRoot) holds the fake adb.exe, its private server
// listening; adb clients started from here on log their arguments to the returned file.
func portPlanRoot(t *testing.T) (*root.Root, string) {
	t.Helper()
	dir := opsTestTempDir(t)
	adbExe := filepath.Join(dir, "dev-sdk", "platform-tools", "adb.exe")
	if err := os.MkdirAll(filepath.Dir(adbExe), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adbExe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	server, script := freeTCPPort(t), freeTCPPort(t)
	sdk, _ := json.Marshal(filepath.Dir(filepath.Dir(adbExe)))
	meta := fmt.Sprintf(`{"portBase": 21000, "adbServerPort": %d, "scriptAdbServerPort": %d, "sdkRoot": %s}`,
		server, script, sdk)
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.AdbExe() != adbExe || r.AdbServerPort() != server || r.Ports().Base != 21000 || r.CheckPorts() != nil {
		t.Fatalf("root: adb %s, server %d, plan %d, check %v", r.AdbExe(), r.AdbServerPort(), r.Ports().Base,
			r.CheckPorts())
	}

	p, err := win.StartDetached(adbExe, nil, win.StartOptions{Env: append(os.Environ(), fakeAdbEnv+"=server",
		fakeAdbPortEnv+"="+strconv.Itoa(server))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if proc, err := os.FindProcess(int(p.PID)); err == nil {
			proc.Kill()
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, up, _ := win.ListenerPID(server); up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake adb server does not listen")
		}
		time.Sleep(50 * time.Millisecond)
	}
	log := filepath.Join(dir, "fake-adb.log")
	t.Setenv(fakeAdbEnv, "client")
	t.Setenv(fakeAdbLogEnv, log)
	return r, log
}

// TestPortPlanReachesEveryAdbCall: on a root with portBase 21000, everything that names instance 6 to adb names it
// emulator-21060, never emulator-200xx (the default plan's, i.e. the production root's instance of the same number).
// What status reports follows too.
func TestPortPlanReachesEveryAdbCall(t *testing.T) {
	r, log := portPlanRoot(t)
	ctx := context.Background()
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	// instance 6 runs: the process scan finds our own process for it (nothing below ends it)
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("cannot read our own process")
	}
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: self} }
	t.Cleanup(func() { qemuScan = saved })

	info, err := m.Info(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	if info.Serial != "emulator-21060" || info.AdbAddress != "127.0.0.1:21061" ||
		info.Ports != (Ports{Console: 21060, Adb: 21061, Grpc: 21062}) || info.Screencap.Shm != "SHM_videmulator21060" ||
		!info.Booted {
		t.Fatalf("info = %+v", info)
	}
	list, err := m.List(ctx)
	if err != nil || list.PortBase != 21000 || len(list.Instances) != 1 || list.Instances[0].Serial != "emulator-21060" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	calls := 0
	call := func(name string, f func()) { f(); calls++ }
	call("Shell", func() {
		if _, err := m.Shell(ctx, 6, "echo hi", 10*time.Second); err != nil {
			t.Errorf("Shell: %v", err)
		}
	})
	call("AdbPassthrough", func() {
		if _, err := m.AdbPassthrough(ctx, 6, []string{"get-serialno"}, nil, io.Discard, io.Discard); err != nil {
			t.Errorf("AdbPassthrough: %v", err)
		}
	})
	call("bootCompleted", func() { m.bootCompleted(ctx, 6, 10*time.Second, false) })
	call("guestBoot", func() { m.guestBoot(ctx, 6, false) })
	call("guestIO", func() { m.guestIO(ctx, 6).adb(10*time.Second, "get-state") })
	local := filepath.Join(r.Dir, "push-me.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	call("push", func() { m.push(ctx, 6, local, "/data/local/tmp/push-me.txt") })
	if err := os.MkdirAll(r.ComponentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.LauncherAPK(), []byte("not really an apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	call("installLauncher", func() { m.installLauncher(ctx, 6) })
	call("ensureLogcat", func() { m.ensureLogcat(ctx, 6) })

	// the logcat capture is started detached: give it a moment to write its line
	var lines []string
	for deadline := time.Now().Add(10 * time.Second); ; {
		b, _ := os.ReadFile(log)
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
		if strings.Contains(string(b), "logcat") || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	serials := map[string]int{}
	for _, line := range lines {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("fake adb log line %q: %v", line, err)
		}
		if len(args) < 2 || args[0] != "-P" || args[1] != strconv.Itoa(r.AdbServerPort()) {
			t.Errorf("adb %q: not on the private server %d", args, r.AdbServerPort())
		}
		for k := 0; k+1 < len(args); k++ {
			if args[k] == "-s" {
				serials[args[k+1]]++
			}
			if strings.HasPrefix(args[k], "emulator-200") || strings.HasPrefix(args[k+1], "emulator-200") {
				t.Errorf("adb %q names the default plan's instance", args)
			}
		}
	}
	if len(serials) != 1 || serials["emulator-21060"] < calls {
		t.Fatalf("serials %v after %d calls (log: %q)", serials, calls, lines)
	}
	t.Logf("%d adb runs, serials %v", len(lines), serials)
	if !strings.Contains(strings.Join(lines, "\n"), "logcat") {
		t.Errorf("no logcat capture was started: %q", lines)
	}
}

// TestPortBaseRefused: an invalid portBase, or a plan of one's own still on the production adb servers, starts
// nothing: create refuses before writing a file, picking an index refuses, and so does a boot.
func TestPortBaseRefused(t *testing.T) {
	ctx := context.Background()
	for _, meta := range []string{
		`{"portBase": 21050, "adbServerPort": 21099, "scriptAdbServerPort": 21098}`,
		`{"portBase": "abc", "adbServerPort": 21099, "scriptAdbServerPort": 21098}`,
		`{"portBase": 21000}`,
		`{"portBase": 21000, "adbServerPort": 21099, "scriptAdbServerPort": 20049}`,
	} {
		dir := opsTestTempDir(t)
		if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := root.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		m := New(r, logx.Nop())
		six := 6
		if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%s: Create = %v", meta, err)
		}
		if left := instanceFilesOnDisk(r, 6); len(left) > 0 {
			t.Errorf("%s: Create left %v", meta, left)
		}
		if _, err := m.FreeIndex(); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%s: FreeIndex = %v", meta, err)
		}
	}

	// a boot: the instance was made while mas-avd.json was fine
	dir := opsTestTempDir(t)
	write := func(meta string) {
		if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"portBase": 21000, "adbServerPort": 21099, "scriptAdbServerPort": 21098}`)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	inst, err := m.instance(6)
	if err != nil {
		t.Fatal(err)
	}
	write(`{"portBase": 21000, "adbServerPort": 20050, "scriptAdbServerPort": 21098}`)
	if _, err := m.prepareLaunch(ctx, inst, false); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("prepareLaunch = %v", err)
	}
}
