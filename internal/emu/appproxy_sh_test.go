package emu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplaceDaemonScriptSh runs the script that swaps in a new daemon in a POSIX sh (the macOS / Linux CI; on Windows
// when Git's sh is on PATH): the old daemon, a sleep whose pid a stand-in status prints the way modavd-proxyd does, is
// killed and the new file put in place; with no daemon running nothing is killed.
func TestReplaceDaemonScriptSh(t *testing.T) {
	if strings.Contains(replaceDaemonScript(proxyExe), "'") {
		t.Fatal("the script has a single quote (Manager.su puts it inside single quotes)")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	if strings.ContainsAny(dir, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	d := filepath.ToSlash(dir)
	exe := d + "/modavd-proxyd"
	status := `#!/bin/sh
[ "$1" = status ] || exit 2
if [ -s ` + d + `/pid ]; then
	printf '{"version":"old","running":true,"pid":%s,"started_at":"x"}\n' "$(cat ` + d + `/pid)"
else
	printf '{"version":"old","running":false,"pid":0}\n'
fi
`
	run := func(daemon bool) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "modavd-proxyd"), []byte(status), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "modavd-proxyd.new"), []byte("#!/bin/sh\necho new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, "pid"))
		start := ""
		if daemon {
			start = "sleep 30 & echo $! > " + d + "/pid\n"
		}
		script := start + replaceDaemonScript(exe) + "\nrc=$?\nsleep 1\n" +
			`if [ -s ` + d + `/pid ] && kill -0 "$(cat ` + d + `/pid)" 2>/dev/null; then echo alive; else echo gone; fi` + "\n" +
			`echo "rc=$rc"; ` + exe + "\n"
		out, err := exec.Command(sh, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("sh: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(true); !strings.Contains(out, "gone") || !strings.Contains(out, "rc=0") || !strings.Contains(out, "new") {
		t.Errorf("with the old daemon running:\n%s", out)
	}
	if out := run(false); !strings.Contains(out, "rc=0") || !strings.Contains(out, "new") {
		t.Errorf("with none running:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "modavd-proxyd.new")); err == nil {
		t.Error("the .new file is still there")
	}
}
