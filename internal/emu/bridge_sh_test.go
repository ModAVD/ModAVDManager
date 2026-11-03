package emu

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBridgeGuestHashScriptSh runs the guest script in a POSIX sh with sha256sum (the macOS / Linux CI, macOS through
// a shasum wrapper when it has no sha256sum; on Windows only when Git's sh and sha256sum are on PATH) on temporary
// files: one current, one different, one missing and, where permissions can deny reading (not as root, not on
// Windows), one unreadable. Its output goes through payloadStale.
func TestBridgeGuestHashScriptSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var env []string
	if _, err := exec.LookPath("sha256sum"); err != nil {
		shasum, err := exec.LookPath("shasum")
		if err != nil || runtime.GOOS == "windows" {
			t.Skip("no sha256sum")
		}
		bin := t.TempDir()
		wrapper := "#!/bin/sh\nexec '" + shasum + "' -a 256 \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte(wrapper), 0o755); err != nil {
			t.Fatal(err)
		}
		env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	dir := t.TempDir()
	if strings.ContainsAny(dir, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	path := func(name string) string { return filepath.ToSlash(filepath.Join(dir, name)) }
	sumOf := func(data string) string {
		s := sha256.Sum256([]byte(data))
		return hex.EncodeToString(s[:])
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("new payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old"), []byte("old payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(want map[string]string) (bool, bool, string) {
		t.Helper()
		targets := make([]string, 0, len(want))
		for target := range want {
			targets = append(targets, target)
		}
		cmd := exec.Command(sh, "-c", bridgeGuestHashScript(targets))
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sh: %v\n%s", err, out)
		}
		stale, known := payloadStale(want, string(out))
		return stale, known, string(out)
	}

	if stale, known, out := run(map[string]string{path("current"): sumOf("new payload")}); stale || !known {
		t.Errorf("current: stale=%v known=%v\n%s", stale, known, out)
	}
	if stale, known, out := run(map[string]string{path("current"): sumOf("new payload"),
		path("old"): sumOf("new payload")}); !stale || !known {
		t.Errorf("one different: stale=%v known=%v\n%s", stale, known, out)
	}
	if stale, known, out := run(map[string]string{path("current"): sumOf("new payload"),
		path("missing"): sumOf("new payload")}); !stale || !known {
		t.Errorf("one missing: stale=%v known=%v\n%s", stale, known, out)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "unreadable"), []byte("new payload"), 0o000); err != nil {
		t.Fatal(err)
	}
	if stale, known, out := run(map[string]string{path("old"): sumOf("new payload"),
		path("unreadable"): sumOf("new payload")}); known {
		t.Errorf("one unreadable: stale=%v known=%v, want unknown\n%s", stale, known, out)
	}
}
