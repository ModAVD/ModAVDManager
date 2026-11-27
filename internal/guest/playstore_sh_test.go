package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// psGuest is a fake guest for playstore.sh under a POSIX sh (Git's on Windows): MODAVD_PS_ROOT is a temporary
// directory standing for /, and pm, am, remount, chcon, chown, mv and sqlite3 are stubs on PATH (pm logs its arguments
// and answers `pm path` by whether the placeholder is there). The overlay's upper layer is a separate tree, as in the
// guest. Failures to order: chcon and mv fail on a path containing what <root>/chcon.fail or mv.fail holds, and
// `pm enable` appends to the guest path <root>/pm.tamper names (a file that is not the store's after the copy).
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
enable) if [ -f "$MODAVD_PS_ROOT/pm.tamper" ]; then echo changed >> "$MODAVD_PS_ROOT$(cat "$MODAVD_PS_ROOT/pm.tamper")"; fi ;;
esac`)
	// fails stands first in the stubs that can be told to fail: $1 is the path to look at
	fails := func(what string) string {
		return `f=$(cat "$MODAVD_PS_ROOT/` + what + `.fail" 2>/dev/null)
if [ -n "$f" ]; then case "$1" in *"$f"*) echo "` + what + `: $1: No space left on device" >&2; exit 1 ;; esac; fi
`
	}
	stub("chcon", `shift
`+fails("chcon")+`exit 0`)
	// the real mv after the check: the stubs' directory is first on PATH, so by its full path
	stub("mv", fails("mv")+`for d in /usr/bin /bin; do [ -x "$d/mv" ] && exec "$d/mv" "$@"; done
echo "no mv" >&2; exit 127`)
	// GSF's database is a text file holding the ID in the fake
	stub("sqlite3", `case "$2" in *"from main where name='android_id'"*) cat "$1" ;; *) echo "unexpected query: $2" >&2; exit 1 ;; esac`)
	for _, name := range []string{"am", "remount", "chown"} {
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

// The test list's guest paths (files) and the placeholder's apk.
const (
	psStore = "/product/priv-app/Phonesky/Phonesky.apk"
	psGms   = "/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk"
	psGSF   = "/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk"
	psStub  = "/product/app/LicenseChecker/LicenseChecker.apk"
)

func (g *psGuest) exists(p string) bool {
	_, err := os.Lstat(filepath.FromSlash(p))
	return err == nil
}

// outLines are the script's output lines; why is the one before RESULT (why an install failed).
func outLines(out string) []string {
	return strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r", "")), "\n")
}

func why(out string) string {
	lines := outLines(out)
	for k := len(lines) - 1; k > 0; k-- {
		if strings.HasPrefix(lines[k], "RESULT ") {
			return lines[k-1]
		}
	}
	return ""
}

// TestPlayStoreScriptUndoStaged: an install that cannot copy a file (the overlay is out of space) takes back what it
// put in: the staged copies and the directories it made go, a file that was the store's copy already stays, the
// image's file and the placeholder are untouched, and RESULT failed says nothing of it is left. Nothing of it needs a
// boot.
func TestPlayStoreScriptUndoStaged(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	g.write(g.root+psStore, "store") // an earlier install's copy: kept
	g.write(g.root+"/chcon.fail", "PrebuiltGmsCore.apk.new")
	r, out := g.run("install")
	if r != "failed" || why(out) != "install failed: cannot copy "+psGms || !slices.Contains(outLines(out), "kept "+psStore) ||
		slices.Contains(outLines(out), "undone from the next boot") {
		t.Fatalf("install: %s (%q)\n%s", r, why(out), out)
	}
	if got, _ := g.read(g.root + psStore); got != "store" {
		t.Errorf("the copy that was there: %q", got)
	}
	for _, p := range []string{psGms + ".new", "/product/priv-app/PrebuiltGmsCore", psGSF, psGSF + ".new"} {
		if g.exists(g.root + p) {
			t.Errorf("%s left", p)
		}
	}
	if !g.exists(g.root + psStub) {
		t.Error("the placeholder went")
	}
}

// TestPlayStoreScriptUndoMove: a file that cannot take its place undoes the ones that did (one the image did not
// have: through the merged view) and the staged copies; the image's own file stays.
func TestPlayStoreScriptUndoMove(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	g.write(g.root+psGms, "the image's play services")
	g.write(g.root+"/mv.fail", "PrebuiltGmsCore.apk.new")
	r, out := g.run("install")
	if r != "failed" || why(out) != "install failed: cannot move "+psGms+" into place" ||
		!slices.Contains(outLines(out), "took back "+psStore) {
		t.Fatalf("install: %s (%q)\n%s", r, why(out), out)
	}
	if got, _ := g.read(g.root + psGms); got != "the image's play services" {
		t.Errorf("the image's file: %q", got)
	}
	for _, p := range []string{psStore, "/product/priv-app/Phonesky", psGms + ".new", psGSF + ".new", "/system_ext/priv-app"} {
		if g.exists(g.root + p) {
			t.Errorf("%s left", p)
		}
	}
	if r, out := g.run("status"); r != "absent" {
		t.Errorf("status after the undo: %s\n%s", r, out)
	}
}

// TestPlayStoreScriptUndoMoved: an install whose files are all in place yet not the store's afterwards takes them all
// back: the ones the image did not have through the merged view, the one that replaced the image's and the
// placeholder's whiteout from the overlay's upper layer, which the guest sees from its next boot (it says so).
func TestPlayStoreScriptUndoMoved(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	up := g.root + "/mnt/scratch/overlay"
	g.write(g.root+psGms, "the image's play services")
	// what the overlay's upper layer holds after the move and the hide (the fake has no overlay to make them)
	g.write(up+"/product/upper/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", "play services")
	g.write(up+"/product/upper/app/LicenseChecker", "")
	g.write(g.root+"/pm.tamper", psGSF)
	r, out := g.run("install")
	lines := outLines(out)
	if r != "failed" || why(out) != "install failed: the store is partial after copying" ||
		!slices.Contains(lines, "undone from the next boot") || !slices.Contains(lines, "restored /product/app/LicenseChecker") ||
		!slices.Contains(lines, "hid /product/app/LicenseChecker") {
		t.Fatalf("install: %s (%q)\n%s", r, why(out), out)
	}
	for _, p := range []string{g.root + psStore, g.root + "/product/priv-app/Phonesky", g.root + psGSF, g.root + "/system_ext/priv-app",
		up + "/product/upper/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", up + "/product/upper/app/LicenseChecker"} {
		if g.exists(p) {
			t.Errorf("%s left", p)
		}
	}
}

// TestPlayStoreScriptUndoPartial: what cannot be taken back makes the install partial (and status says partial too);
// so does a staged copy left behind.
func TestPlayStoreScriptUndoPartial(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	up := g.root + "/mnt/scratch/overlay"
	g.write(g.root+psGms, "the image's play services")
	// a directory where the upper copy is: rm -f cannot remove it
	g.write(up+"/product/upper/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk/x", "")
	g.write(g.root+"/pm.tamper", psGSF)
	r, out := g.run("install")
	if r != "partial" || why(out) != "install failed: the store is partial after copying" || !slices.Contains(outLines(out), "left "+psGms) {
		t.Fatalf("install: %s (%q)\n%s", r, why(out), out)
	}
	if r, out := g.run("status"); r != "partial" {
		t.Errorf("status: %s\n%s", r, out)
	}

	g = newPSGuest(t)
	g.files(false)
	g.write(g.root+psStore+".new", "store")
	if r, out := g.run("status"); r != "partial" || !slices.Contains(outLines(out), "staged "+psStore+".new") {
		t.Errorf("status with a staged copy: %s\n%s", r, out)
	}
}

// TestPlayStoreScriptGSFID: gsfid reads GSF's android_id with the documentation's query and gives it in decimal and
// hex; without the database, or with nothing, 0 or something else in it, it says no-id.
func TestPlayStoreScriptGSFID(t *testing.T) {
	g := newPSGuest(t)
	g.files(false)
	if r, out := g.run("gsfid"); r != "no-id" || !strings.Contains(out, "wait a minute") {
		t.Errorf("without the database: %s\n%s", r, out)
	}
	db := g.root + "/data/data/com.google.android.gsf/databases/gservices.db"
	for _, v := range []string{"", "0\n", "abc\n", "12-3\n"} {
		g.write(db, v)
		if r, out := g.run("gsfid"); r != "no-id" {
			t.Errorf("%q: %s\n%s", v, r, out)
		}
	}
	const id = 3881793548573364239
	g.write(db, strconv.FormatInt(id, 10)+"\n")
	r, out := g.run("gsfid")
	if lines := outLines(out); r != "ok" || !slices.Contains(lines, "decimal "+strconv.FormatInt(id, 10)) ||
		!slices.Contains(lines, "hex "+strconv.FormatInt(id, 16)) {
		t.Errorf("an ID: %s\n%s", r, out)
	}
}
