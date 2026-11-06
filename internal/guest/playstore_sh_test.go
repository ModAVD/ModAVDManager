package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// psGuest is a fake guest for playstore.sh under a POSIX sh (Git's on Windows): MODAVD_PS_ROOT is a temporary
// directory standing for /, and pm, am, remount, chcon and chown are stubs on PATH (pm logs its arguments and answers
// `pm path` by whether the placeholder is there). The overlay's upper layer is a separate tree, as in the guest.
type psGuest struct {
	t        *testing.T
	sh, root string
	dir      string // the push directory
	env      []string
}

func newPSGuest(t *testing.T) *psGuest {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"sha256sum", "cut", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	base := testutil.TempDir(t)
	if strings.ContainsAny(base, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	g := &psGuest{t: t, sh: sh, root: filepath.ToSlash(filepath.Join(base, "root")), dir: filepath.ToSlash(filepath.Join(base, "push"))}
	bin := filepath.Join(base, "bin")
	for _, d := range []string{g.dir, bin, g.root + "/product/app/LicenseChecker", g.root + "/system_ext",
		g.root + "/mnt/scratch/overlay/product/upper", g.root + "/mnt/scratch/overlay/system_ext/upper"} {
		if err := os.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.write(g.root+"/product/app/LicenseChecker/LicenseChecker.apk", "placeholder")
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("pm", `echo "$*" >> "$MODAVD_PS_ROOT/pm.log"
case "$1" in
path) if [ -e "$MODAVD_PS_ROOT/product/app/LicenseChecker" ]; then echo package:/product/app/LicenseChecker/LicenseChecker.apk
      else echo package:/product/priv-app/Phonesky/Phonesky.apk; fi ;;
uninstall-system-updates) echo Success ;;
esac`)
	for _, name := range []string{"am", "remount", "chcon", "chown"} {
		stub(name, "exit 0")
	}
	// the stubs go first on PATH inside the shell (run): Git for Windows' bin\sh.exe puts its usr\bin, which has a real
	// chcon and chown, before the PATH it is given
	g.env = append(os.Environ(), "MODAVD_PS_ROOT="+g.root, "MODAVD_PS_STUBS="+filepath.ToSlash(bin))
	return g
}

// stubbedSh runs `sh <args>` with the stubs first on PATH: set by the shell itself, in its own spelling of the
// directory (pwd: /c/... under Git's sh), as a PATH entry cannot hold a drive letter's colon.
const stubbedSh = `S=$(cd "$MODAVD_PS_STUBS" && pwd) || exit 97; PATH="$S:$PATH"; export PATH; exec sh "$@"`

func (g *psGuest) write(p, data string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.FromSlash(p)), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(p), []byte(data), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *psGuest) read(p string) (string, bool) {
	b, err := os.ReadFile(filepath.FromSlash(p))
	return string(b), err == nil
}

// files puts the store's (test) files and their list into the push directory; tamper changes one after listing it.
func (g *psGuest) files(tamper bool) {
	list := ""
	for _, f := range [][3]string{{"Phonesky.apk", "/product/priv-app/Phonesky/Phonesky.apk", "store"},
		{"PrebuiltGmsCore.apk", "/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", "play services"},
		{"GoogleServicesFramework.apk", "/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", "gsf"}} {
		s := sha256.Sum256([]byte(f[2]))
		list += f[0] + " " + f[1] + " " + hex.EncodeToString(s[:]) + "\n"
		g.write(g.dir+"/"+f[0], f[2])
	}
	if tamper {
		g.write(g.dir+"/PrebuiltGmsCore.apk", "something else")
	}
	g.write(g.dir+"/files.txt", list)
}

// TestPlayStoreScriptEmbedded: the script is embedded without CR (Android's sh chokes on CRLF).
func TestPlayStoreScriptEmbedded(t *testing.T) {
	b, err := Script(PlayStore)
	if err != nil || len(b) == 0 || strings.ContainsRune(string(b), '\r') {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
}

// run is the script's action: its RESULT word and its whole output.
func (g *psGuest) run(action string) (string, string) {
	g.t.Helper()
	b, err := Script(PlayStore)
	if err != nil {
		g.t.Fatal(err)
	}
	script := filepath.Join(filepath.FromSlash(g.dir), "playstore.sh")
	if err := os.WriteFile(script, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command(g.sh, "-c", stubbedSh, "sh", filepath.ToSlash(script), action, g.dir)
	cmd.Env = g.env
	out, _ := cmd.CombinedOutput()
	result := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if w, ok := strings.CutPrefix(line, "RESULT "); ok {
			result = w
		}
	}
	return result, string(out)
}

// TestPlayStoreScript goes through the guest script: a tampered file installs nothing; the files go in and the
// placeholder is hidden; status reads installed; uninstall removes the overlay's copies and the placeholder's
// whiteout, asks for a reboot, and the run after it finds the placeholder back and enables it.
func TestPlayStoreScript(t *testing.T) {
	g := newPSGuest(t)

	g.files(true)
	if r, out := g.run("install"); r != "bad-file" {
		t.Fatalf("tampered: %s\n%s", r, out)
	}
	if _, ok := g.read(g.root + "/product/priv-app/Phonesky/Phonesky.apk"); ok {
		t.Fatal("a file was copied although another one did not match")
	}
	if _, ok := g.read(g.root + "/product/app/LicenseChecker/LicenseChecker.apk"); !ok {
		t.Fatal("the placeholder went although nothing was installed")
	}

	g.files(false)
	if r, out := g.run("install"); r != "installed" {
		t.Fatalf("install: %s\n%s", r, out)
	}
	if got, _ := g.read(g.root + "/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk"); got != "gsf" {
		t.Errorf("GSF in place: %q", got)
	}
	if _, ok := g.read(g.root + "/product/app/LicenseChecker/LicenseChecker.apk"); ok {
		t.Error("the placeholder is still there")
	}
	if log, _ := g.read(g.root + "/pm.log"); !strings.Contains(log, "enable com.android.vending") {
		t.Errorf("pm calls: %q", log)
	}
	if r, out := g.run("status"); r != "installed" || !strings.Contains(out, "play /product/priv-app/Phonesky/Phonesky.apk") ||
		!strings.Contains(out, "stub hidden") {
		t.Errorf("status: %s\n%s", r, out)
	}

	// in the guest the copies are in the overlay's upper layer and the hidden placeholder is a whiteout there
	up := g.root + "/mnt/scratch/overlay"
	// an uninstall before any boot since the install: the package manager still has the placeholder, yet the files
	// just removed are only gone after a boot, so it asks for one
	g.write(up+"/product/upper/priv-app/Phonesky/Phonesky.apk", "store")
	g.write(g.root+"/product/app/LicenseChecker/LicenseChecker.apk", "placeholder")
	if r, out := g.run("uninstall"); r != "reboot" {
		t.Fatalf("uninstall before a boot: %s\n%s", r, out)
	}
	os.RemoveAll(filepath.FromSlash(g.root + "/product/app/LicenseChecker"))
	g.write(up+"/product/upper/priv-app/Phonesky/Phonesky.apk", "store")
	g.write(up+"/system_ext/upper/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", "gsf")
	g.write(up+"/product/upper/app/LicenseChecker", "")
	if r, out := g.run("uninstall"); r != "reboot" || !strings.Contains(out, "removed updates of com.android.vending") {
		t.Fatalf("uninstall: %s\n%s", r, out)
	}
	for _, p := range []string{"/product/upper/priv-app/Phonesky/Phonesky.apk", "/product/upper/app/LicenseChecker",
		"/system_ext/upper/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk"} {
		if _, ok := g.read(up + p); ok {
			t.Errorf("%s left in the upper layer", p)
		}
	}
	if log, _ := g.read(g.root + "/pm.log"); !strings.Contains(log, "disable com.android.vending") {
		t.Errorf("pm calls: %q", log)
	}
	// after the cold boot: the placeholder is back (the overlay without the whiteout)
	g.write(g.root+"/product/app/LicenseChecker/LicenseChecker.apk", "placeholder")
	os.Remove(filepath.FromSlash(g.root + "/pm.log"))
	if r, out := g.run("uninstall"); r != "gone" {
		t.Fatalf("second uninstall: %s\n%s", r, out)
	}
	if log, _ := g.read(g.root + "/pm.log"); !strings.HasSuffix(strings.TrimSpace(log), "enable com.android.vending") {
		t.Errorf("pm calls of the second run: %q", log)
	}
}

// TestPlayStoreScriptNotWritable: without a writable product partition nothing is checked or copied.
func TestPlayStoreScriptNotWritable(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	g := newPSGuest(t)
	g.files(false)
	// a file where the partition's directory should be: touch inside it fails
	os.RemoveAll(filepath.FromSlash(g.root + "/product"))
	g.write(g.root+"/product", "not a directory")
	if r, out := g.run("install"); r != "not-writable" {
		t.Fatalf("install: %s\n%s", r, out)
	}
	if _, ok := g.read(g.root + "/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk"); ok {
		t.Error("copied into a partition that is not writable")
	}
}
