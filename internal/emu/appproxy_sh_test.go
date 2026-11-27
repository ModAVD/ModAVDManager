package emu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/appproxy"
	"modavd/internal/testutil"
)

// TestReplaceDaemonScriptSh runs the script that swaps in a new daemon in a POSIX sh with a /proc that shows a
// program's path (the Linux CI; skipped where it does not, as on macOS). The old daemon is a copy of sleep at the
// daemon's path whose pid is in the lock file: it is killed, without running the old file, and the new file is put in
// place. A pid in the lock file that runs another program is left alone; with no lock file nothing is killed.
func TestReplaceDaemonScriptSh(t *testing.T) {
	for _, s := range []string{replaceDaemonScript(proxyExe), rootDaemonScript(proxyExe, strings.Repeat("a", 64), "start")} {
		if strings.Contains(s, "'") {
			t.Fatalf("a single quote (Manager.su puts the script inside single quotes): %s", s)
		}
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	dir := testutil.TempDir(t)
	if strings.ContainsAny(dir, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	d := filepath.ToSlash(dir)
	exe := d + "/" + appproxy.DaemonName
	lock := d + "/" + appproxy.LockName
	run := func(start string) string {
		t.Helper()
		b, err := os.ReadFile(sleep)
		if err != nil {
			t.Skip(err)
		}
		if err := os.WriteFile(filepath.Join(dir, appproxy.DaemonName), b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, appproxy.DaemonName+".new"), []byte("#!/bin/sh\necho new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, appproxy.LockName))
		script := start + replaceDaemonScript(exe) + "\nrc=$?\nsleep 1\n" +
			`if [ -s ` + lock + ` ] && kill -0 "$(cat ` + lock + `)" 2>/dev/null; then echo alive; else echo gone; fi` + "\n" +
			`echo "rc=$rc"; ` + exe + "\n" +
			`[ -s ` + lock + ` ] && kill "$(cat ` + lock + `)" 2>/dev/null; true` + "\n"
		out, err := exec.Command(sh, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("sh: %v\n%s", err, out)
		}
		return string(out)
	}
	// the old daemon: this path running, its pid in the lock file, as modavd-proxyd run writes it
	probe := exe + " 30 & p=$!; echo $p > " + lock + "; " +
		`case "$(readlink /proc/$p/exe)" in "` + exe + `"|"` + exe + ` (deleted)") ;; *) kill $p; echo noproc; exit 0;; esac` + "\n"
	out := run(probe)
	if strings.Contains(out, "noproc") {
		t.Skip("this sh's /proc does not show a program's path")
	}
	if !strings.Contains(out, "gone") || !strings.Contains(out, "rc=0") || !strings.Contains(out, "new") {
		t.Errorf("with the old daemon running:\n%s", out)
	}
	// the lock file names a process of another program: not killed, the file still swapped
	if out := run(sleep + " 30 & echo $! > " + lock + "\n"); !strings.Contains(out, "alive") || !strings.Contains(out, "rc=0") ||
		!strings.Contains(out, "new") {
		t.Errorf("with another program's pid in the lock file:\n%s", out)
	}
	// no lock file
	if out := run(""); !strings.Contains(out, "rc=0") || !strings.Contains(out, "new") {
		t.Errorf("with no daemon:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, appproxy.DaemonName+".new")); err == nil {
		t.Error("the .new file is still there")
	}
}
