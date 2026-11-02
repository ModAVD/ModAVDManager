//go:build windows

package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
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
	for _, systemRoot := range []string{"relative", ""} {
		t.Run(systemRoot, func(t *testing.T) {
			t.Setenv("SystemRoot", systemRoot)
			run, err := m.runBridgeScript(context.Background(), 1, "verify", false)
			if errs.CodeOf(err) != errs.Internal {
				t.Fatalf("SystemRoot=%q: error=%v, want internal", systemRoot, err)
			}
			if run.Code != -1 {
				t.Fatalf("SystemRoot=%q: run=%+v, relative PowerShell may have run", systemRoot, run)
			}
		})
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

// TestPayloadHashes: the payload's SHA-256 by guest target, as the guest's sha256sum prints it; a missing file means
// no answer (the script would not install from that payload either).
func TestPayloadHashes(t *testing.T) {
	dir := t.TempDir()
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
