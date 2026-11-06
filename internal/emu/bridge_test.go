//go:build windows

package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

func TestBridgeScriptRejectsRelativeSystemRoot(t *testing.T) {
	m := New(managerTestRoot(t, 1), nil)
	if err := os.MkdirAll(filepath.Join(m.Root.Dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root.Dir, "tools", bridgeScriptName), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := bridgeCommandContext
	t.Cleanup(func() { bridgeCommandContext = saved })
	for _, systemRoot := range []string{"relative", "", `C:Windows`, `\Windows`} {
		t.Run(systemRoot, func(t *testing.T) {
			t.Setenv("SystemRoot", systemRoot)
			called := false
			bridgeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
				called = true
				return exec.Command("missing-powershell.exe")
			}
			run, err := m.runBridgeScript(context.Background(), 1, "verify", false)
			if called {
				t.Fatal("invalid SystemRoot reached PowerShell command creation")
			}
			if errs.CodeOf(err) != errs.Internal {
				t.Fatalf("SystemRoot=%q: error=%v, want internal", systemRoot, err)
			}
			if run.Code != -1 || run.LogFile != "" {
				t.Fatalf("SystemRoot=%q: run=%+v", systemRoot, run)
			}
			if _, err := os.Stat(m.Root.LogsDir()); !os.IsNotExist(err) {
				t.Fatalf("invalid SystemRoot created logs: %v", err)
			}
		})
	}
}

func TestBridgeScriptAbsoluteSystemRootRuns(t *testing.T) {
	m := New(managerTestRoot(t, 1), nil)
	if err := os.MkdirAll(filepath.Join(m.Root.Dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root.Dir, "tools", bridgeScriptName), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	systemRoot := testutil.TempDir(t)
	t.Setenv("SystemRoot", systemRoot)
	t.Setenv("MODAVD_TEST_BRIDGE_PROCESS", "1")
	saved := bridgeCommandContext
	t.Cleanup(func() { bridgeCommandContext = saved })
	called := 0
	bridgeCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		called++
		want := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if name != want {
			t.Fatalf("PowerShell=%q, want %q", name, want)
		}
		if len(args) != 6 || args[4] != "-Command" || !strings.Contains(args[5], bridgeScriptName) {
			t.Fatalf("PowerShell arguments=%q", args)
		}
		return exec.CommandContext(ctx, exe, "-test.run=^TestBridgeScriptProcess$")
	}
	run, err := m.runBridgeScript(context.Background(), 1, "verify", false)
	if err != nil || called != 1 || run.Code != 0 || run.Last != "bridge test output" {
		t.Fatalf("run=%+v, calls=%d, error=%v", run, called, err)
	}
	out, err := os.ReadFile(run.LogFile)
	if err != nil || strings.TrimSpace(string(out)) != "bridge test output" {
		t.Fatalf("script log=%q, error=%v", out, err)
	}
}

func TestBridgeScriptProcess(t *testing.T) {
	if os.Getenv("MODAVD_TEST_BRIDGE_PROCESS") != "1" {
		return
	}
	fmt.Println("bridge test output")
	os.Exit(0)
}

func TestBridgeScriptWindowsPowerShell(t *testing.T) {
	m := New(managerTestRoot(t, 1), nil)
	if err := os.MkdirAll(filepath.Join(m.Root.Dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root.Dir, "tools", bridgeScriptName),
		[]byte("Write-Output 'bridge PowerShell output'; exit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 12; k++ {
		mkLog(t, m.Root.LogsDir(), fmt.Sprintf("modavd-bridge-1-20261028-1200%02d.log", k), 1, k)
	}
	run, err := m.runBridgeScript(context.Background(), 1, "verify", false)
	if err != nil || run.Code != 0 || run.Last != "bridge PowerShell output" || run.LogFile == "" {
		t.Fatalf("run=%+v, error=%v", run, err)
	}
	if got := names(t, m.Root.LogsDir()); len(got) != 10 {
		t.Fatalf("script did not prune bridge logs: %q", got)
	}
	if _, err := os.Stat(run.LogFile); err != nil {
		t.Fatalf("new script output was removed: %v", err)
	}
}

// TestBridgeLockExcludes: while some process holds an instance's bridge lock (an install, with its stops and cold
// boots), status says busy "bridging", a second bridge action and a delete are refused as busy, and the delete that
// runs afterwards removes the lock file with the others.
func TestBridgeLockExcludes(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	i := 6
	_, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &i})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	l, ok, err := win.TryLock(m.lockPath(i, "bridge"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	info, err := m.Info(ctx, i)
	if err != nil || info.Busy != "bridging" {
		l.Unlock()
		t.Fatalf("busy = %q, %v", info.Busy, err)
	}
	for _, action := range []string{"install", "uninstall", "verify"} {
		if _, err := m.Bridge(ctx, i, action); errs.CodeOf(err) != errs.Busy {
			l.Unlock()
			t.Fatalf("bridge %s while bridging: %v", action, err)
		}
	}
	if _, err := m.Delete(ctx, i); errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在装卸执行桥") {
		l.Unlock()
		t.Fatalf("delete while bridging: %v", err)
	}
	l.Unlock()

	if _, err := m.Delete(ctx, i); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"start", "stop", "bridge"} {
		if fileOrDirExists(m.lockPath(i, kind)) {
			t.Errorf("%s lock file left after delete", kind)
		}
	}
}

// TestBridgeKeepsWindowMode: every cold boot of modavd bridge (the install's on an instance without the bridge, the one
// after an update, both of an uninstall) brings a running instance back the way it ran, with its window or headless;
// an instance that was off is started headless.
func TestBridgeKeepsWindowMode(t *testing.T) {
	m, i := opsTestInstance(t)
	windowed, _ := startSleeper(t) // a readable command line without -no-window
	headless := win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe"}
	findQemu, start, guest, script := bridgeFindQemu, bridgeStart, bridgeGuest, bridgeScript
	t.Cleanup(func() { bridgeFindQemu, bridgeStart, bridgeGuest, bridgeScript = findQemu, start, guest, script })
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		action    string
		installed bool  // the guest runs the bridge
		codes     []int // the script's exit codes, in order (0 after them)
		running   int   // the starts on a running instance (its cold boots)
		off       int   // on one that was off (the first start, then the cold boots)
	}{
		{"install without the bridge", "install", false, nil, 1, 1},
		{"install updating it", "install", true, []int{0}, 1, 2},
		{"uninstall", "uninstall", true, []int{3, 0}, 2, 3},
	} {
		for _, run := range []struct {
			mode   string
			qemu   *win.Process
			window bool
		}{{"windowed", &windowed, true}, {"headless", &headless, false}, {"off", nil, false}} {
			var starts []StartOptions
			bridgeFindQemu = func(*Manager, int) (win.Process, bool, error) {
				if run.qemu == nil {
					return win.Process{}, false, nil
				}
				return *run.qemu, true, nil
			}
			// the instance's qemu is not really there: the stops before the boots find it offline
			bridgeStart = func(_ *Manager, _ context.Context, _ int, opt StartOptions) (*StartResult, error) {
				starts = append(starts, opt)
				return &StartResult{}, nil
			}
			bridgeGuest = func(*Manager, context.Context, int) string {
				if c.installed {
					return bridgeProp
				}
				return ""
			}
			n := 0
			bridgeScript = func(*Manager, context.Context, int, string, bool) (bridgeRun, error) {
				code := 0
				if n < len(c.codes) {
					code = c.codes[n]
				}
				n++
				return bridgeRun{Code: code, Last: "done"}, nil
			}
			if _, err := m.Bridge(ctx, i, c.action); err != nil {
				t.Fatalf("%s, %s: %v", c.name, run.mode, err)
			}
			want := c.running
			if run.qemu == nil {
				want = c.off
			}
			if len(starts) != want {
				t.Errorf("%s, %s: %d starts, want %d", c.name, run.mode, len(starts), want)
			}
			for k, opt := range starts {
				if opt.Window != run.window || opt.Headless {
					t.Errorf("%s, %s: start %d has Window %v Headless %v, want Window %v", c.name, run.mode, k+1,
						opt.Window, opt.Headless, run.window)
				}
			}
		}
	}
}

// TestPayloadHashes: the payload's SHA-256 by guest target, as the guest's sha256sum prints it; a missing file means
// no answer (the script would not install from that payload either).
func TestPayloadHashes(t *testing.T) {
	dir := testutil.TempDir(t)
	want := map[string]string{}
	for k, f := range bridgePayload {
		data := []byte(strings.Repeat(f.name, k+1))
		if err := os.WriteFile(filepath.Join(dir, f.name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		want[f.target] = hex.EncodeToString(sum[:])
	}
	got, ok := payloadHashes(dir)
	if !ok || len(got) != len(bridgePayload) {
		t.Fatalf("payloadHashes = %v, %v", got, ok)
	}
	for target, sum := range want {
		if got[target] != sum {
			t.Errorf("%s: %s, want %s", target, got[target], sum)
		}
	}
	if err := os.Remove(filepath.Join(dir, bridgePayload[1].name)); err != nil {
		t.Fatal(err)
	}
	if got, ok := payloadHashes(dir); ok {
		t.Fatalf("a file missing: %v, %v", got, ok)
	}
}

// TestBridgeGuestHashScript: the guest script names every installed file.
func TestBridgeGuestHashScript(t *testing.T) {
	s := bridgeGuestHashScript(bridgePayloadTargets())
	for _, f := range bridgePayload {
		if !strings.Contains(s, " "+f.target) {
			t.Errorf("%s not in %q", f.target, s)
		}
	}
}

// TestBridgeUpdateRecovery: the advice after a failed cold boot leads to a lasting way back (uninstall records the
// opt-out, so no later start writes the marker again), names the marker, and does not end in an empty log path.
func TestBridgeUpdateRecovery(t *testing.T) {
	marker := `C:\ModAVD\avd\mas_3.avd\` + bridgeMarkerName
	s := bridgeUpdateRecovery(3, marker, "")
	for _, want := range []string{"modavd stop 3", marker, "modavd bridge 3 uninstall", "modavd bridge 3 install"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q not in %q", want, s)
		}
	}
	if strings.Contains(s, "日志") {
		t.Errorf("no log file, yet: %q", s)
	}
	if s := bridgeUpdateRecovery(3, marker, `C:\ModAVD\logs\b.log`); !strings.HasSuffix(s, `C:\ModAVD\logs\b.log`) {
		t.Errorf("log file not named: %q", s)
	}
}

func targetOf(k int) string { return bridgePayload[k].target }

// TestPayloadStale: the guest's answer to bridgeGuestHashScript against the package's payload. A file that differs
// or is not there makes the bridge outdated; an answer without a hash or "-" for every file decides nothing.
func TestPayloadStale(t *testing.T) {
	want := map[string]string{}
	var lines []string
	for k, f := range bridgePayload {
		sum := strings.Repeat(string(rune('a'+k)), 64)
		want[f.target] = sum
		lines = append(lines, sum+" "+f.target)
	}
	current := strings.Join(lines, "\r\n") + "\r\n"
	for _, c := range []struct {
		name         string
		out          string
		stale, known bool
	}{
		{"current", current, false, true},
		{"LF", strings.ReplaceAll(current, "\r\n", "\n"), false, true},
		{"upper case", strings.ToUpper(lines[0][:64]) + lines[0][64:] + "\n" + lines[1] + "\n" + lines[2] + "\n",
			false, true},
		{"one differs", strings.Replace(current, strings.Repeat("b", 64), strings.Repeat("f", 64), 1), true, true},
		{"one not installed", strings.Replace(current, strings.Repeat("c", 64), "-", 1), true, true},
		{"none installed", "- " + targetOf(0) + "\n- " + targetOf(1) + "\n- " + targetOf(2) + "\n", true, true},
		{"one not hashable", strings.Replace(current, strings.Repeat("c", 64), "?", 1), false, false},
		{"empty hash", strings.Replace(current, strings.Repeat("c", 64), "", 1), false, false},
		{"not a hash", strings.Replace(current, strings.Repeat("c", 64), "sha256sum:", 1), false, false},
		{"short hex", strings.Replace(current, strings.Repeat("c", 64), "abcdef", 1), false, false},
		{"cut short", lines[0] + "\n" + lines[1] + "\n", false, false},
		{"last line cut", current[:len(current)-20], false, false},
		{"empty", "", false, false},
		{"adb error", "error: device offline\n", false, false},
	} {
		if stale, known := payloadStale(want, c.out); stale != c.stale || known != c.known {
			t.Errorf("%s: stale=%v known=%v, want %v %v", c.name, stale, known, c.stale, c.known)
		}
	}
}
