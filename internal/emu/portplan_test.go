//go:build windows

package emu

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

// The fake adb and qemu of the port plan tests (ModAVD/ModAVD#75): this test binary copied as adb.exe or as
// qemu-system-x86_64-headless.exe. MODAVD_EMU_FAKE_ADB=server listens on MODAVD_EMU_FAKE_ADB_PORT, which is all
// EnsureServer looks for (a listening adb.exe); =client appends its arguments to MODAVD_EMU_FAKE_ADB_LOG and answers
// as a guest that has booted; =qemu just runs with a qemu's command line (-avd mas_<i> -ports <console>,<adb>),
// listening on MODAVD_EMU_FAKE_ADB_PORT when that is set, as qemu does on its adb port.
const (
	fakeAdbEnv     = "MODAVD_EMU_FAKE_ADB"
	fakeAdbLogEnv  = "MODAVD_EMU_FAKE_ADB_LOG"
	fakeAdbPortEnv = "MODAVD_EMU_FAKE_ADB_PORT"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case LogcatCaptureArg:
			limit := logcatSegmentBytes
			if os.Getenv(fakeLogcatStreamEnv) != "" {
				limit = 128
			}
			os.Exit(runLogcatCapture(os.Args[2:], os.Stderr, limit))
		case win.SpawnHelperArg:
			os.Exit(win.SpawnHelperMain(os.Args[2:], os.Stdout))
		case fakeLogcatLaunchArg:
			r, err := root.New(os.Args[2])
			if err != nil {
				os.Exit(1)
			}
			i, _ := strconv.Atoi(os.Args[3])
			pid, _ := strconv.ParseUint(os.Args[4], 10, 32)
			created, _ := strconv.ParseUint(os.Args[5], 10, 64)
			qemuScan = func([]int) map[int]win.Process {
				return map[int]win.Process{i: {PID: uint32(pid), Created: created}}
			}
			win.OrphanHelper, _ = os.Executable()
			path := New(r, nil).ensureLogcat(context.Background(), i)
			fmt.Println(path)
			if path == "" {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	switch os.Getenv(fakeAdbEnv) {
	case "server", "qemu":
		os.Exit(fakeAdbServerMain())
	case "client":
		os.Exit(fakeAdbClientMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeAdbServerMain listens on MODAVD_EMU_FAKE_ADB_PORT (if set) and waits, at most 2 minutes, until it is ended.
func fakeAdbServerMain() int {
	time.AfterFunc(2*time.Minute, func() { os.Exit(0) }) // never outlives a test that could not end it
	port := os.Getenv(fakeAdbPortEnv)
	if port == "" {
		select {}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return 1
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		c.Close()
	}
}

// copyTestExe copies this test binary to path (the fake adb / qemu).
func copyTestExe(t *testing.T, path string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// startFake starts exe (a copy of this binary) in mode with args, listening on listen when it is not 0, and ends it
// (by its PID) when the test is done. It returns once the process runs (and listens).
func startFake(t *testing.T, exe, mode string, listen int, args ...string) win.Process {
	t.Helper()
	env := append(os.Environ(), fakeAdbEnv+"="+mode)
	if listen != 0 {
		if _, up, _ := win.ListenerPID(listen); up {
			t.Skipf("port %d is in use on this machine", listen)
		}
		env = append(env, fakeAdbPortEnv+"="+strconv.Itoa(listen))
	}
	p, err := win.StartDetached(exe, args, win.StartOptions{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if proc, err := os.FindProcess(int(p.PID)); err == nil {
			proc.Kill()
			proc.Wait()
		}
	})
	if listen != 0 {
		for deadline := time.Now().Add(10 * time.Second); ; {
			if pid, up, _ := win.ListenerPID(listen); up && pid == p.PID {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the fake %s does not listen on %d", mode, listen)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return p
}

func fakeAdbClientMain(args []string) int {
	if slices.Contains(args, "logcat") && os.Getenv(fakeLogcatStreamEnv) != "" {
		return fakeLogcatStream(args)
	}
	line, _ := json.Marshal(args)
	if f, err := os.OpenFile(os.Getenv(fakeAdbLogEnv), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	if handled, code := fakeProxyGuest(args); handled {
		return code
	}
	all := strings.Join(args, " ")
	switch {
	case strings.Contains(all, "get-state"):
		fmt.Println("device")
	case strings.Contains(all, "sys.boot_completed"):
		if os.Getenv(fakeAdbNoBootEnv) == "" {
			fmt.Println("1")
		}
	case strings.Contains(all, "KEYCODE_HOME"): // guestHomeScript: what is in front
		fmt.Println(cmp.Or(os.Getenv(fakeAdbHomeEnv), "home"))
	case strings.Contains(all, guestShutdownCommand):
		code, _ := strconv.Atoi(os.Getenv(fakeAdbSvcExitEnv))
		return code
	case strings.Contains(all, "logcat -d -b all"): // bootdiag.go
		fmt.Println("11-04 10:00:00.000 E zygote: fake")
	case strings.HasSuffix(all, "shell getprop"):
		fmt.Println("[sys.boot_completed]: []")
	}
	return 0
}

// The fake adb client's answers for the stop tests: what guestHomeScript prints ("app" / "home", default "home") and
// the exit code of the shutdown command (default 0).
const (
	fakeAdbHomeEnv    = "MODAVD_EMU_FAKE_ADB_HOME"
	fakeAdbSvcExitEnv = "MODAVD_EMU_FAKE_ADB_SVC_EXIT"
	fakeAdbNoBootEnv  = "MODAVD_EMU_FAKE_ADB_NOBOOT" // set: sys.boot_completed stays empty (a boot that never ends)
)

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
	copyTestExe(t, adbExe)
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

	startFake(t, adbExe, "server", server)
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
	copyTestExe(t, filepath.Join(r.Dir, "tools", "modavd.exe"))
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

// Legacy plans may not start another instance, but status and a normal shutdown must still reach private adb.
// Only qemu discovery and its exit are simulated; prepare, HOME, sync and shutdown run the real adb client path.
func TestLegacyPortPlanNormalShutdown(t *testing.T) {
	for _, base := range []int{5500, 49100} {
		t.Run(strconv.Itoa(base), func(t *testing.T) {
			r, log := portPlanRoot(t)
			m := New(r, logx.Nop())
			six := 6
			ctx := context.Background()
			if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(r.MetadataPath())
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), `"portBase": 21000`, fmt.Sprintf(`"portBase": %d`, base)))
			if err := os.WriteFile(r.MetadataPath(), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if r.Ports().Base != base || r.CheckPorts() == nil || r.CheckAdbPorts() != nil {
				t.Fatalf("legacy plan: %d, start %v, adb %v", r.Ports().Base, r.CheckPorts(), r.CheckAdbPorts())
			}
			self, ok := win.FindProcess(uint32(os.Getpid()))
			if !ok {
				t.Fatal("cannot read self")
			}
			savedScan, savedOps := qemuScan, stopOpsOf
			qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: self} }
			stopOpsOf = func(mm *Manager, ctx context.Context, i int, _ win.Process) stopOps {
				ops := mm.stopOpsFor(ctx, i, win.Process{PID: 0xFFFFFFFC})
				ops.consoleKill = func(time.Duration) error { t.Fatal("unexpected console kill"); return nil }
				ops.forceKill = func(time.Duration) error { t.Fatal("unexpected force kill"); return nil }
				return ops
			}
			t.Cleanup(func() { qemuScan, stopOpsOf = savedScan, savedOps })
			info, err := m.Info(ctx, 6)
			if err != nil || !info.Booted || info.Serial != fmt.Sprintf("emulator-%d", base+60) {
				t.Fatalf("Info = %+v, %v", info, err)
			}
			res, err := m.Stop(ctx, 6, StopOptions{})
			if err != nil || res.Method != "graceful" || !res.Synced {
				t.Fatalf("Stop = %+v, %v", res, err)
			}
			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			var commands []string
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				var args []string
				if err := json.Unmarshal([]byte(line), &args); err != nil {
					t.Fatal(err)
				}
				if len(args) < 4 || args[0] != "-P" || args[1] != strconv.Itoa(r.AdbServerPort()) ||
					args[2] != "-s" || args[3] != fmt.Sprintf("emulator-%d", base+60) {
					t.Fatalf("wrong adb target: %q", args)
				}
				if n := slices.Index(args, "shell"); n >= 0 {
					commands = append(commands, strings.Join(args[n+1:], " "))
				}
			}
			if !slices.Equal(commands[len(commands)-3:], []string{guestHomeScript, "sync", guestShutdownCommand}) {
				t.Fatalf("shutdown commands = %q", commands)
			}
		})
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
	// nor does anything else that would start or use an adb server: the production one, here
	if _, err := m.EnsureServers(ctx); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("EnsureServers = %v", err)
	}
	if _, err := m.AdbPassthrough(ctx, 6, []string{"devices"}, nil, io.Discard, io.Discard); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("AdbPassthrough = %v", err)
	}
}

// portPlanTestRoot is a root with portBase 21000 (servers 21099 / 21098) and the given instances (files only).
// Nothing here runs adb.
func portPlanTestRoot(t *testing.T, indexes ...int) (*Manager, *root.Root) {
	t.Helper()
	dir := opsTestTempDir(t)
	meta := `{"portBase": 21000, "adbServerPort": 21099, "scriptAdbServerPort": 21098}`
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, logx.Nop())
	for _, i := range indexes {
		if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &i}); err != nil {
			t.Fatal(err)
		}
	}
	return m, r
}

// fakeQemu starts a fake qemu of instance i on console (listening on its adb port console+1 when listen) from exeDir.
// When avdDir is not "", it also writes the qemu's emulator runtime file naming avdDir, which is what
// adb.QemuBelongsTo goes by (the test points LOCALAPPDATA, under which that file lives, at a directory of its own).
func fakeQemu(t *testing.T, exeDir string, i, console int, listen bool, avdDir string) win.Process {
	t.Helper()
	exe := filepath.Join(exeDir, "qemu-system-x86_64-headless.exe")
	if _, err := os.Stat(exe); err != nil {
		copyTestExe(t, exe)
	}
	adbPort := 0
	if listen {
		adbPort = console + 1
	}
	p := startFake(t, exe, "qemu", adbPort, "-avd", root.AvdName(i), "-ports", fmt.Sprintf("%d,%d", console, console+1),
		"-grpc", strconv.Itoa(console+2))
	if avdDir != "" {
		ini := filepath.Join(adb.EmulatorDiscoveryDir(), fmt.Sprintf("pid_%d.ini", p.PID))
		if err := os.MkdirAll(filepath.Dir(ini), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ini, []byte("avd.dir="+avdDir+"\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestLaunchPlanPorts: a boot of instance 6 on a root with portBase 21000 gives the emulator -ports 21060,21061 and
// -grpc 21062: on the default plan's 20060 it would sit on the production root's ports, the very thing portBase is for.
func TestLaunchPlanPorts(t *testing.T) {
	r, _ := portPlanRoot(t)
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	inst, err := m.instance(6)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.launchPlanFor(context.Background(), inst, 3072, false)
	if errs.CodeOf(err) == errs.Busy {
		t.Skipf("ports of instance 6 in use on this machine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	args := " " + strings.Join(plan.args, " ") + " "
	if !strings.Contains(args, " -ports 21060,21061 ") || !strings.Contains(args, " -grpc 21062 ") {
		t.Errorf("emulator arguments: %s", args)
	}
}

// TestQemuMatchedByPortPlan: on a root with portBase 21000, this root's qemu of instance 6 is the one on -ports 21060
// (and listening on 21061), not one of mas_6 on the default plan's 20060; a qemu of another root's mas_7 on 21070 is
// that other root's instance on our ports; the host tuning counts our qemu's vCPUs from our instance.
func TestQemuMatchedByPortPlan(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t)) // the fakes' emulator runtime files
	m, r := portPlanTestRoot(t, 6, 7)
	exeDir := testutil.TempDir(t)
	ours := fakeQemu(t, exeDir, 6, 21060, true, filepath.Join(r.AvdHome(), "mas_6.avd"))
	fakeQemu(t, exeDir, 6, 20060, false, "") // a mas_6 on the default plan's ports, not under our SDK: not ours
	other := fakeQemu(t, exeDir, 7, 21070, true, filepath.Join(testutil.TempDir(t), "avd", "mas_7.avd"))

	if found, err := scanQemuOf([]*root.Root{r}, []int{6, 7}); err != nil || found[6].PID != ours.PID || found[7].PID != 0 {
		t.Errorf("scanQemuOf = %v, error=%v, want 6 -> pid %d and no 7", found, err, ours.PID)
	}
	if p, ok := adb.QemuOnAdbPort(r, 21060); !ok || p.PID != ours.PID {
		t.Errorf("QemuOnAdbPort(21060) = %v %v, want pid %d", p, ok, ours.PID)
	}
	if p, where, found := m.otherRootInstance(r.Ports(), 7); !found || p.PID != other.PID || !strings.HasSuffix(where, `\mas_7.avd`) {
		t.Errorf("otherRootInstance(7) = %v %q %v, want pid %d", p, where, found, other.PID)
	}
	if p, _, found := m.otherRootInstance(r.Ports(), 6); found {
		t.Errorf("otherRootInstance(6) = pid %d: our own qemu taken for another root's", p.PID)
	}
	counted := 0
	for _, q := range m.otherQemus(win.Process{}, 2) {
		if q.Cores == avd.DefaultCPU {
			counted++
		}
	}
	if counted == 0 {
		t.Errorf("otherQemus did not count our qemu's %d vCPUs from instance 6", avd.DefaultCPU)
	}
}

// TestBootingPortPlan: the adb client's "is it booting" hook maps emulator-21060 to instance 6 of a root with portBase
// 21000 (and emulator-20060, the default plan's, to none of its instances).
func TestBootingPortPlan(t *testing.T) {
	m, _ := portPlanTestRoot(t, 6)
	l, ok, err := win.TryLock(m.lockPath(6, "boot"))
	if err != nil || !ok {
		t.Fatalf("boot lock: %v %v", ok, err)
	}
	defer l.Unlock()
	if !m.ADB.Booting("emulator-21060") {
		t.Error("emulator-21060 not booting while instance 6 boots")
	}
	if m.ADB.Booting("emulator-20060") {
		t.Error("emulator-20060 (the default plan's) taken for instance 6")
	}
}

// TestWindowOfInstanceQemu: showing or hiding the window acts on the qemu process of that instance, nothing else
// (the windows are no longer looked up by a title with the console port in it).
func TestWindowOfInstanceQemu(t *testing.T) {
	m, _ := portPlanTestRoot(t, 6)
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("cannot read our own process")
	}
	saved, savedSet := qemuScan, setEmulatorWindowsVisible
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: self} }
	var pids []uint32
	setEmulatorWindowsVisible = func(pid uint32, visible bool) (int, error) {
		pids = append(pids, pid)
		return 1, nil
	}
	t.Cleanup(func() { qemuScan, setEmulatorWindowsVisible = saved, savedSet })
	if res, err := m.SetWindowVisible(context.Background(), 6, false); err != nil || res.Windows != 1 {
		t.Fatalf("SetWindowVisible = %+v, %v", res, err)
	}
	if len(pids) != 1 || pids[0] != self.PID {
		t.Errorf("windows looked for in processes %v, want [%d]", pids, self.PID)
	}
}

// TestBridgeScriptPortPlan: the bridge script gets instance 6's serial in the root's plan, and the root's server.
func TestBridgeScriptPortPlan(t *testing.T) {
	m, r := portPlanTestRoot(t, 6)
	script := filepath.Join(r.Dir, "tools", bridgeScriptName)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "param([Parameter(Position=0)][string]$Action, [string]$Serial, [int]$AdbPort, [string]$Adb, [string]$Payload, [switch]$Reboot)\r\n" +
		"\"$Action $Serial $AdbPort\"\r\n"
	if err := os.WriteFile(script, []byte(fake), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := m.runBridgeScript(context.Background(), 6, "verify", false)
	if err != nil || run.Code != 0 || run.Last != "verify emulator-21060 21099" {
		t.Errorf("runBridgeScript = %+v, %v", run, err)
	}
}
