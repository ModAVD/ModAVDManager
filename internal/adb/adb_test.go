//go:build windows

package adb

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

// tempDir is a fresh directory for one test, removed afterwards: under $MODAVD_TEST_TMP\adb when that is set (to keep
// test data on a chosen drive), otherwise t.TempDir().
func tempDir(t *testing.T) string {
	t.Helper()
	env := os.Getenv("MODAVD_TEST_TMP")
	if env == "" {
		return t.TempDir()
	}
	base := filepath.Join(env, "adb")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, strings.ReplaceAll(t.Name(), "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestEnvKey(t *testing.T) {
	for kv, want := range map[string]string{
		`=C:=C:\work`: "=C:",
		"A=B=C":       "A",
		"Path=":       "Path",
		"NOEQ":        "NOEQ",
		"=":           "=",
	} {
		if got := envKey(kv); got != want {
			t.Errorf("envKey(%q) = %q, want %q", kv, got, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{
		`=C:=C:\work`,
		`Path=C:\Windows`,
		"android_home=old",
		"ANDROID_SDK_ROOT=old1",
		"Android_Sdk_Root=old2", // a case-insensitive duplicate: dropped
		"FOO=bar=baz",
	}
	set := map[string]string{
		"ANDROID_HOME":                 "H",
		"ANDROID_SDK_ROOT":             "S",
		"ANDROID_AVD_HOME":             "A",
		"ADB_LOCAL_TRANSPORT_MAX_PORT": "5554",
	}
	got := mergeEnv(base, set)
	want := []string{
		`=C:=C:\work`,
		`Path=C:\Windows`,
		"ANDROID_HOME=H",
		"ANDROID_SDK_ROOT=S",
		"FOO=bar=baz",
		"ADB_LOCAL_TRANSPORT_MAX_PORT=5554",
		"ANDROID_AVD_HOME=A",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mergeEnv:\n got %q\nwant %q", got, want)
	}
}

// envValues collects the values of key (case-insensitive) in env.
func envValues(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if strings.EqualFold(envKey(kv), key) {
			vals = append(vals, kv[len(envKey(kv))+1:])
		}
	}
	return vals
}

func TestEnv(t *testing.T) {
	r := &root.Root{Dir: tempDir(t)}
	c := New(r, nil)
	t.Setenv("ANDROID_HOME", `C:\elsewhere`)
	t.Setenv("ANDROID_ADB_SERVER_PORT", "5037")
	t.Setenv("ADB_LOCAL_TRANSPORT_MAX_PORT", "5585")
	t.Setenv("MODAVD_TEST_KEEP", "kept")
	env := c.Env()
	want := map[string]string{
		"ANDROID_SDK_ROOT":             r.RuntimeSDK(),
		"ANDROID_HOME":                 r.RuntimeSDK(),
		"ANDROID_AVD_HOME":             r.AvdHome(),
		"ANDROID_ADB_SERVER_PORT":      strconv.Itoa(r.AdbServerPort()),
		"ADB_LOCAL_TRANSPORT_MAX_PORT": "5554",
		"MODAVD_TEST_KEEP":             "kept",
	}
	for k, v := range want {
		got := envValues(env, k)
		if len(got) != 1 || got[0] != v {
			t.Errorf("Env %s = %q, want exactly [%q]", k, got, v)
		}
	}
	if len(envValues(env, "PATH")) != 1 {
		t.Errorf("Env lost PATH")
	}
}

func TestDeviceMissing(t *testing.T) {
	cases := []struct {
		serial, out string
		want        bool
	}{
		{"emulator-20000", "error: device 'emulator-20000' not found", true},
		{"emulator-20000", "adb: device 'emulator-20000' not found", true},
		{"emulator-20000", "Error: Device 'EMULATOR-20000' NOT FOUND", true},
		{"emulator-20000", "error: device offline", false},
		{"emulator-20000", "error: device 'emulator-20010' not found", false},
		{"emulator-20000", "error: no devices/emulators found", false},
		{"emulator-20000", "", false},
	}
	for _, tc := range cases {
		if got := DeviceMissing(tc.serial, tc.out); got != tc.want {
			t.Errorf("DeviceMissing(%q, %q) = %v, want %v", tc.serial, tc.out, got, tc.want)
		}
	}
}

func TestConsolePortOf(t *testing.T) {
	ok := map[string]int{"emulator-20000": 20000, "emulator-20040": 20040, "emulator-20090": 20090}
	for serial, want := range ok {
		if got, found := consolePortOf(serial); !found || got != want {
			t.Errorf("consolePortOf(%q) = %d, %v; want %d", serial, got, found, want)
		}
	}
	for _, serial := range []string{"emulator-", "emulator-5554", "emulator-20001", "emulator-20100", "emulator-2000x",
		"emulator-+20000", "127.0.0.1:20001", "EMULATOR-20000", "emulator-0000000020000"} {
		if got, found := consolePortOf(serial); found {
			t.Errorf("consolePortOf(%q) = %d, want not ours", serial, got)
		}
	}
}

func TestQemuArgsMatch(t *testing.T) {
	cases := []struct {
		cmdline []string
		want    bool
	}{
		{[]string{"qemu.exe", "-avd", "mas_0", "-ports", "20000,20001"}, true},
		{[]string{"qemu.exe", "-ports", "20000,20001", "-x", "-avd", "mas_0"}, true},
		{[]string{"qemu.exe", "-avd", "mas_0", "-ports", "20000,20002"}, false},
		{[]string{"qemu.exe", "-avd", "mas_0", "-ports", "20000"}, false},
		{[]string{"qemu.exe", "-avd", "mas_1", "-ports", "20000,20001"}, false},
		{[]string{"qemu.exe", "-ports", "20000,20001"}, false},
		{[]string{"qemu.exe", "-ports", "20000,20001", "-avd"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := qemuArgsMatch(tc.cmdline, 20000, "mas_0"); got != tc.want {
			t.Errorf("qemuArgsMatch(%q) = %v, want %v", tc.cmdline, got, tc.want)
		}
	}
}

// Ports outside the plan are rejected before the listener table is read; so is any port without a root.
func TestQemuOnAdbPortNotOurs(t *testing.T) {
	r := &root.Root{Dir: tempDir(t)}
	for _, console := range []int{5554, 20005, 20100, 19990} {
		if _, ok := QemuOnAdbPort(r, console); ok {
			t.Errorf("QemuOnAdbPort(%d) = ok, want not ours", console)
		}
	}
	if _, ok := QemuOnAdbPort(nil, 20000); ok {
		t.Errorf("QemuOnAdbPort(nil root) = ok")
	}
	// A fresh root has no instance running: whatever listens on an instance adb port on this machine (a real
	// emulator of another root, say) is not its own. Only the listener table and that process are read.
	for i := 0; i <= root.MaxIndex; i++ {
		if !root.ValidIndex(i) {
			continue
		}
		if p, ok := QemuOnAdbPort(r, root.ConsolePort(i)); ok {
			t.Errorf("QemuOnAdbPort(%d) on a fresh root = pid %d", root.ConsolePort(i), p.PID)
		}
	}
}

func TestReregisterGuards(t *testing.T) {
	c := New(&root.Root{Dir: tempDir(t)}, nil)
	ctx := context.Background()
	for _, serial := range []string{"127.0.0.1:20001", "emulator-5554", ""} {
		if c.Reregister(ctx, serial) {
			t.Errorf("Reregister(%q) = true", serial)
		}
	}
	// a booting instance is never registered on its behalf; checked before anything is read from the system
	var asked string
	c.Booting = func(serial string) bool { asked = serial; return true }
	hooked := false
	c.OnReregistered = func(string) { hooked = true }
	if c.Reregister(ctx, "emulator-20080") {
		t.Errorf("Reregister while booting = true")
	}
	if asked != "emulator-20080" {
		t.Errorf("Booting asked for %q", asked)
	}
	if hooked {
		t.Errorf("OnReregistered ran")
	}
}

func TestRegisterPayload(t *testing.T) {
	cases := map[int]string{
		20001: "0013host:emulator:20001",
		5:     "000fhost:emulator:5", // lower-case hex
	}
	for adbPort, want := range cases {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan string, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				got <- "accept: " + err.Error()
				return
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, err := io.ReadAll(conn) // until Register closes: it must close by itself
			if err != nil {
				got <- "read: " + err.Error()
				return
			}
			got <- string(b)
		}()
		start := time.Now()
		err = Register(context.Background(), ln.Addr().(*net.TCPAddr).Port, adbPort, 2*time.Second)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if elapsed < registerLinger {
			t.Errorf("Register returned after %v, want >= %v (server needs time to read)", elapsed, registerLinger)
		}
		if s := <-got; s != want {
			t.Errorf("payload %q, want %q", s, want)
		}
		ln.Close()
	}
}

// freePort is a loopback port nobody listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestRegisterFails(t *testing.T) {
	port := freePort(t)
	err := Register(context.Background(), port, 20001, 5*time.Second)
	if err == nil {
		t.Fatal("Register to a closed port succeeded")
	}
	if errs.CodeOf(err) != errs.Internal || !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("Register error = %v (%s)", err, errs.CodeOf(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Register(ctx, port, 20001, time.Second); errs.CodeOf(err) != errs.Canceled {
		t.Errorf("Register with canceled ctx = %v, want canceled", err)
	}
}

func TestPortConflictMessage(t *testing.T) {
	dir := tempDir(t)
	c := New(&root.Root{Dir: dir}, nil)
	e := c.portConflict(privateServer, 20050, 1234, "foo.exe")
	want := "魔改 AVD 的私有 adb 端口 20050 被 foo.exe（PID 1234）占用，它不是 adb。请在 " + c.Root.MetadataPath() +
		" 里把 adbServerPort 改成一个没被占用的端口（不能用 5037 和各实例的控制台 / adb / gRPC 端口，也不能和 " +
		"scriptAdbServerPort 相同），或者先关掉占用它的程序"
	if e.Code != errs.PortConflict || e.Error() != want {
		t.Errorf("private conflict:\n got %s %q\nwant %q", e.Code, e.Error(), want)
	}
	e = c.portConflict(scriptServer, 20049, 1, "x.exe")
	if !strings.HasPrefix(e.Message, "魔改 AVD 的脚本专用 adb 端口 20049 被 x.exe（PID 1）占用") ||
		!strings.Contains(e.Message, "里把 scriptAdbServerPort 改成") || !strings.Contains(e.Message, "也不能和 adbServerPort 相同") {
		t.Errorf("script conflict: %q", e.Message)
	}
}

func TestIsAdbName(t *testing.T) {
	for name, want := range map[string]bool{"adb.exe": true, "ADB.EXE": true, "adb": true, "adb.test.exe": false,
		"qemu-system-x86_64.exe": false, "": false} {
		if got := isAdbName(name); got != want {
			t.Errorf("isAdbName(%q) = %v", name, got)
		}
	}
}
