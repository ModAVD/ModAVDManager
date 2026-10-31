//go:build windows

package adb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// Private test ports for the real adb test. Never the production servers (5037, 20049, 20050), the dev launcher's
// 20089, or an instance port.
const (
	testServerPort = 20077
	testScriptPort = 20076
)

// testSDK is an SDK whose adb.exe the test may run (only run, never modified): $MODAVD_TEST_SDK, else the package's.
func testSDK() string {
	for _, sdk := range []string{os.Getenv("MODAVD_TEST_SDK"), `D:\ModAVD\sdk`} {
		if sdk == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(sdk, "platform-tools", "adb.exe")); err == nil && !st.IsDir() {
			return sdk
		}
	}
	return ""
}

// realRoot is a temp root whose mas-avd.json points sdkRoot at the test SDK and moves both servers to the test ports.
// It refuses to go on unless root reads them back exactly.
func realRoot(t *testing.T) (*root.Root, string) {
	t.Helper()
	sdk := testSDK()
	if sdk == "" {
		t.Skip("没有可用的 adb.exe（MODAVD_TEST_SDK / D:\\ModAVD\\sdk）")
	}
	dir := tempDir(t)
	meta, _ := json.Marshal(map[string]any{
		"sdkRoot": sdk, "adbServerPort": testServerPort, "scriptAdbServerPort": testScriptPort,
	})
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatalf("root.New: %v", err)
	}
	if p := r.AdbServerPort(); p != testServerPort {
		t.Fatalf("测试根目录的 adbServerPort 读成了 %d（应为 %d），不跑真 adb", p, testServerPort)
	}
	if p := r.ScriptAdbServerPort(); p != testScriptPort {
		t.Fatalf("测试根目录的 scriptAdbServerPort 读成了 %d（应为 %d），不跑真 adb", p, testScriptPort)
	}
	adbExe := filepath.Join(sdk, "platform-tools", "adb.exe")
	if !strings.EqualFold(filepath.Clean(r.AdbExe()), adbExe) {
		t.Fatalf("AdbExe = %q, want %q", r.AdbExe(), adbExe)
	}
	return r, adbExe
}

// killServer ends the test server on port (adb -P <port> kill-server) and waits for the port to close.
func killServer(t *testing.T, adbExe string, port int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, adbExe, "-P", strconv.Itoa(port), "kill-server")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	t.Logf("adb -P %d kill-server: err=%v out=%q", port, err, strings.TrimSpace(string(out)))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if up, _ := listening(port); !up {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("端口 %d 在 kill-server 之后仍在监听", port)
}

type logCollector struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logCollector) sink(_ logx.Level, msg string) {
	l.mu.Lock()
	l.msgs = append(l.msgs, msg)
	l.mu.Unlock()
}

func (l *logCollector) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

func TestRealAdb(t *testing.T) {
	// Looks at the emulators running on this machine (their lock / runtime files, a real adb server): opt in.
	if os.Getenv("MODAVD_TEST_LIVE") == "" {
		t.Skip("set MODAVD_TEST_LIVE=1 to run tests that look at live emulators or start a real adb server")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	r, adbExe := realRoot(t)
	for _, p := range []int{testServerPort, testScriptPort} {
		up, err := listening(p)
		if err != nil {
			t.Fatalf("listener table: %v", err)
		}
		if up {
			t.Skipf("端口 %d 已有程序在监听，不是本测试起的，跳过真 adb 测试", p)
		}
	}
	logs := &logCollector{}
	c := New(r, logx.New(logs.sink, logx.TextSink(testWriter{t}, logx.Debug)))
	ctx := context.Background()
	t.Cleanup(func() {
		killServer(t, adbExe, testServerPort)
		killServer(t, adbExe, testScriptPort)
	})

	t.Run("conflict", func(t *testing.T) {
		// Something that is not adb on the private / script port: PortConflict, nothing is started.
		self := win.ProcessName(uint32(os.Getpid()))
		for _, k := range []struct {
			port   int
			kind   serverKind
			ensure func(context.Context, time.Duration) (bool, error)
		}{{testServerPort, privateServer, c.EnsureServer}, {testScriptPort, scriptServer, c.EnsureScriptServer}} {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(k.port))
			if err != nil {
				t.Fatal(err)
			}
			ok, err := k.ensure(ctx, 2*time.Second)
			want := c.portConflict(k.kind, k.port, uint32(os.Getpid()), self).Error()
			if ok || errs.CodeOf(err) != errs.PortConflict || err.Error() != want {
				t.Errorf("ensure on a foreign listener = %v, %v\nwant %q", ok, err, want)
			}
			if k.port == testServerPort {
				res := c.Run(ctx, RunOptions{Timeout: 5 * time.Second}, "devices")
				if res.Code != -1 || res.Out != want {
					t.Errorf("Run on a foreign listener = %d %q", res.Code, res.Out)
				}
				if reason := c.PrepareForClose(ctx, 20080, 4*time.Second); reason != want {
					t.Errorf("PrepareForClose on a foreign listener = %q", reason)
				}
			}
			ln.Close()
		}
		if n := logs.count("adb server（端口"); n != 0 {
			t.Errorf("a server was started despite the conflict (%d log lines)", n)
		}
	})

	var serverPID uint32
	t.Run("ensure", func(t *testing.T) {
		start := time.Now()
		ok, err := c.EnsureServer(ctx, 20*time.Second)
		if err != nil || !ok {
			t.Fatalf("EnsureServer = %v, %v", ok, err)
		}
		t.Logf("EnsureServer took %v", time.Since(start))
		if n := logs.count(fmt.Sprintf("启动魔改 AVD 私有 adb server（端口 %d）", testServerPort)); n != 1 {
			t.Errorf("start log lines = %d", n)
		}
		pid, up, err := win.ListenerPID(testServerPort)
		if err != nil || !up {
			t.Fatalf("ListenerPID = %d %v %v", pid, up, err)
		}
		serverPID = pid
		if name := win.ProcessName(pid); !isAdbName(name) {
			t.Errorf("listener is %q", name)
		}
		if p, ok := win.FindProcess(pid); ok && p.PPID == uint32(os.Getpid()) {
			t.Errorf("server PID %d is our direct child", pid)
		}
		// already listening: nothing new is started
		ok, err = c.EnsureServer(ctx, time.Second)
		if err != nil || !ok {
			t.Errorf("second EnsureServer = %v, %v", ok, err)
		}
		if n := logs.count("启动魔改 AVD 私有 adb server"); n != 1 {
			t.Errorf("second EnsureServer started again (%d log lines)", n)
		}
		// PrepareForClose with the server up asks it first; it does not know emulator-20080, and no qemu of this
		// (fresh) root listens on 20081, so nothing is registered
		want := "adb 端口 20081 上不是这台实例的模拟器，不替它登记"
		if reason := c.PrepareForClose(ctx, 20080, 4*time.Second); reason != want {
			t.Errorf("PrepareForClose with the server up = %q, want %q", reason, want)
		}
	})
	if serverPID == 0 {
		t.FailNow()
	}

	t.Run("run", func(t *testing.T) {
		res := c.Run(ctx, RunOptions{Timeout: 10 * time.Second}, "devices")
		if res.Code != 0 || !strings.HasPrefix(res.Out, "List of devices attached") {
			t.Errorf("devices = %d %q", res.Code, res.Out)
		}
		res = c.Run(ctx, RunOptions{Timeout: 10 * time.Second, TotalTimeout: true}, "devices")
		if res.Code != 0 || !strings.HasPrefix(res.Out, "List of devices attached") {
			t.Errorf("devices (total timeout) = %d %q", res.Code, res.Out)
		}
		// a serial the server does not know: the real error text must match DeviceMissing; not ours, so not
		// re-registered (serial outside the port plan, nothing is read or sent for it)
		res = c.Run(ctx, RunOptions{Serial: "emulator-29990", Timeout: 10 * time.Second, Reregister: true}, "get-state")
		if res.Code == 0 || !DeviceMissing("emulator-29990", res.Out) {
			t.Errorf("unknown serial = %d %q", res.Code, res.Out)
		}
		if st := c.GetState(ctx, "emulator-29990", 10*time.Second); st != "" {
			t.Errorf("GetState unknown = %q", st)
		}
		res = c.Run(ctx, RunOptions{Timeout: 10 * time.Second, Raw: true}, "version")
		if res.Code != 0 || !bytes.Contains(res.Stdout, []byte("Android Debug Bridge version")) || res.Out != res.Stderr {
			t.Errorf("raw version = %d stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
		}
		res = c.Run(ctx, RunOptions{Timeout: 10 * time.Second, Raw: true, Serial: "emulator-29990"}, "exec-out", "true")
		if res.Code == 0 || !DeviceMissing("emulator-29990", res.Stderr) || len(res.Stdout) != 0 {
			t.Errorf("raw unknown serial = %d stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
		}
		if pid, _, _ := win.ListenerPID(testServerPort); pid != serverPID {
			t.Errorf("server changed from PID %d to %d", serverPID, pid)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		res := c.Run(ctx, RunOptions{Timeout: 2 * time.Second}, "wait-for-device") // no device ever comes
		elapsed := time.Since(start)
		if res.Code != -1 || res.Out != "adb wait-for-device 超时（2 秒）" {
			t.Errorf("timeout result = %d %q", res.Code, res.Out)
		}
		if elapsed < 2*time.Second || elapsed > 6*time.Second {
			t.Errorf("timeout took %v", elapsed)
		}
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		start = time.Now()
		res = c.Run(cctx, RunOptions{Timeout: 10 * time.Second}, "wait-for-device")
		if res.Code != -1 || res.Out != "adb wait-for-device 已取消" {
			t.Errorf("canceled result = %d %q", res.Code, res.Out)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("cancel took %v", elapsed)
		}
	})

	t.Run("script server", func(t *testing.T) {
		ok, err := c.EnsureScriptServer(ctx, 20*time.Second)
		if err != nil || !ok {
			t.Fatalf("EnsureScriptServer = %v, %v", ok, err)
		}
		if n := logs.count(fmt.Sprintf("启动魔改 AVD 脚本专用 adb server（端口 %d）", testScriptPort)); n != 1 {
			t.Errorf("script start log lines = %d", n)
		}
		pid, _, _ := win.ListenerPID(testScriptPort)
		if name := win.ProcessName(pid); !isAdbName(name) {
			t.Errorf("script listener is %q", name)
		}
	})

	t.Run("pipe held by server", func(t *testing.T) {
		// What EnsureServer avoids: an adb client that starts the server itself hands it our pipe. Run must still
		// return (WaitDelay) instead of waiting for the server to exit.
		killServer(t, adbExe, testScriptPort)
		start := time.Now()
		res := c.execAdb(ctx, false, 15*time.Second, 15*time.Second, []string{"start-server"},
			[]string{"-P", strconv.Itoa(testScriptPort), "start-server"})
		elapsed := time.Since(start)
		t.Logf("self-started server: %d %q in %v", res.Code, res.Out, elapsed)
		if res.Code != 0 {
			t.Errorf("start-server = %d %q", res.Code, res.Out)
		}
		if elapsed > 8*time.Second {
			t.Errorf("adb with a server holding its pipe took %v", elapsed)
		}
		if up, _ := listening(testScriptPort); !up {
			t.Errorf("script port not listening after start-server")
		}
	})

	t.Run("prepare for close starts the server", func(t *testing.T) {
		// Server down, no qemu of ours on the adb port: the server is started, nothing is registered.
		const console = 20080
		if up, _ := listening(console + 1); up {
			t.Skipf("端口 %d 有人在监听，跳过", console+1)
		}
		killServer(t, adbExe, testServerPort)
		reason := c.PrepareForClose(ctx, console, 10*time.Second)
		if want := fmt.Sprintf("adb 端口 %d 上不是这台实例的模拟器，不替它登记", console+1); reason != want {
			t.Errorf("PrepareForClose = %q, want %q", reason, want)
		}
		if up, _ := listening(testServerPort); !up {
			t.Errorf("PrepareForClose did not start the server")
		}
	})
}

// testWriter sends log lines to t.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
