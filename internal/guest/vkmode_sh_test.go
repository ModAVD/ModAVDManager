package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// vkGuest is a fake guest for modavd-vkmode.sh under a POSIX sh (Git's on Windows): MODAVD_VKMODE_ROOT is a temporary
// directory standing for /. Stubs on PATH:
//   - stat -c %u, chown, chmod, chcon, ls -Zd keep owner, mode and label in <path>.owner / .mode / .label next to the
//     path; mv moves those with the file and logs the call;
//   - pgrep -u <uid> prints a pid when <root>/running lists the uid; pgrep -f finds nothing (no watcher);
//   - chown and mv fail (doing nothing) on a path containing what <root>/<name>.fail holds.
type vkGuest struct {
	t        *testing.T
	sh, root string
	env      []string
}

const (
	vkPkg   = "com.miHoYo.hkrpg"
	vkUID   = "10197"
	vkBili  = "com.miHoYo.hkrpg.bilibili"
	vkBUID  = "10198"
	vkMedia = "/data/media/0/Android/data"
	vkLabel = "u:object_r:media_rw_data_file:s0"
)

func vkFiles(pkg string) string { return vkMedia + "/" + pkg + "/files" }

func newVKGuest(t *testing.T) *vkGuest {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"cut", "head", "cat", "rm", "mkdir", "rmdir", "mv", "find", "tr"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	base := testutil.TempDir(t)
	if strings.ContainsAny(base, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	g := &vkGuest{t: t, sh: sh, root: filepath.ToSlash(filepath.Join(base, "root"))}
	bin := filepath.Join(base, "bin")
	for _, d := range []string{bin, g.path("/data/local/tmp"), g.path("/data/data"), g.path(vkMedia)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.write(vkMedia+".label", vkLabel)

	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nR=$MODAVD_VKMODE_ROOT\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fails := func(what, path string) string {
		return `f=$(cat "$R/` + what + `.fail" 2>/dev/null)
if [ -n "$f" ]; then case "` + path + `" in *"$f"*) echo "` + what + `: failed" >&2; exit 1 ;; esac; fi
`
	}
	stub("stat", `[ "$1" = -c ] && [ "$2" = %u ] && [ -e "$3" ] || exit 1
cut -d: -f1 "$3.owner" 2>/dev/null || echo 0`)
	stub("chown", fails("chown", "$2")+`printf '%s\n' "$1" > "$2.owner"`)
	stub("chmod", `printf '%s\n' "$1" > "$2.mode"`)
	stub("chcon", `printf '%s\n' "$1" > "$2.label"`)
	stub("ls", `[ "$1" = -Zd ] || { echo "ls: unexpected $*" >&2; exit 1; }
[ -e "$2" ] || exit 1
echo "$(cat "$2.label" 2>/dev/null || echo u:object_r:unlabeled:s0) $2"`)
	stub("pgrep", `[ "$1" = -u ] || exit 1
grep -qx "$2" "$R/running" 2>/dev/null && echo 4242
exit 0`)
	// the stub directory is the first PATH entry (stubbedSh): without it, mv is the real one
	stub("mv", `[ "$1" = -f ] || { echo "mv: unexpected $*" >&2; exit 1; }
`+fails("mv", "$3")+`PATH=${PATH#*:}
mv -f "$2" "$3" || exit 1
for x in owner mode label; do
    if [ -e "$2.$x" ]; then mv -f "$2.$x" "$3.$x"; else rm -f "$3.$x"; fi
done
echo "${3#$R}" >> "$R/mv.log"`)
	stub("nohup", `exit 0`)
	g.env = append(os.Environ(), "MODAVD_VKMODE_ROOT="+g.root, "MODAVD_PS_STUBS="+filepath.ToSlash(bin))
	return g
}

func (g *vkGuest) path(p string) string { return filepath.FromSlash(g.root + p) }

func (g *vkGuest) write(p, data string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(g.path(p)), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(g.path(p), []byte(data), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// raw is the file as it is ("" when missing); read is it trimmed, for the stubs' text files.
func (g *vkGuest) raw(p string) string {
	b, _ := os.ReadFile(g.path(p))
	return string(b)
}

func (g *vkGuest) read(p string) string {
	return strings.TrimSpace(strings.ReplaceAll(g.raw(p), "\r", ""))
}

func (g *vkGuest) exists(p string) bool {
	_, err := os.Stat(g.path(p))
	return err == nil
}

func (g *vkGuest) remove(p string) {
	g.t.Helper()
	if err := os.RemoveAll(g.path(p)); err != nil {
		g.t.Fatal(err)
	}
}

func (g *vkGuest) run(action string) string {
	g.t.Helper()
	b, err := Script(VulkanMode)
	if err != nil {
		g.t.Fatal(err)
	}
	script := g.path("/data/local/tmp/" + VulkanMode)
	if err := os.WriteFile(script, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command(g.sh, "-c", stubbedSh, "sh", filepath.ToSlash(script), action)
	cmd.Env = g.env
	b, _ = cmd.CombinedOutput()
	out := strings.ReplaceAll(string(b), "\r", "")
	g.t.Logf("%s: %s", action, out)
	return out
}

// install makes the package's private data directory, which is how the script tells an installed app and its uid.
func (g *vkGuest) install(pkg, uid string) {
	g.t.Helper()
	if err := os.MkdirAll(g.path("/data/data/"+pkg), 0o755); err != nil {
		g.t.Fatal(err)
	}
	g.write("/data/data/"+pkg+".owner", uid+":"+uid)
}

// launched makes the folders the app gets from the system at its first start.
func (g *vkGuest) launched(pkg, uid string) {
	g.t.Helper()
	for _, p := range []string{vkMedia + "/" + pkg, vkFiles(pkg)} {
		if err := os.MkdirAll(g.path(p), 0o755); err != nil {
			g.t.Fatal(err)
		}
		g.write(p+".owner", uid+":1078")
		g.write(p+".mode", "2770")
		g.write(p+".label", vkLabel)
	}
}

func (g *vkGuest) mvCalls() int {
	if s := g.read("/mv.log"); s != "" {
		return strings.Count(s, "\n") + 1
	}
	return 0
}

// wantVulkan: both files hold the byte 01 and belong to the app; the folders are the app's; no temporary file is left.
func (g *vkGuest) wantVulkan(pkg, uid, what string) {
	g.t.Helper()
	dir := vkFiles(pkg)
	for _, p := range []string{vkMedia + "/" + pkg, dir} {
		if g.read(p+".owner") != uid+":1078" || g.read(p+".mode") != "2770" || g.read(p+".label") != vkLabel {
			g.t.Errorf("%s: %s: owner %q mode %q label %q", what, p, g.read(p+".owner"), g.read(p+".mode"), g.read(p+".label"))
		}
	}
	for _, name := range []string{"vulkancaps.bin", "customvulkancaps.bin"} {
		p := dir + "/" + name
		if got := g.raw(p); got != "\x01" {
			g.t.Errorf("%s: %s holds %q", what, name, got)
		}
		if g.read(p+".owner") != uid+":1078" || g.read(p+".mode") != "660" {
			g.t.Errorf("%s: %s: owner %q mode %q", what, name, g.read(p+".owner"), g.read(p+".mode"))
		}
	}
	g.wantNoTemp(pkg, what)
}

func (g *vkGuest) wantNoTemp(pkg, what string) {
	g.t.Helper()
	entries, _ := os.ReadDir(g.path(vkFiles(pkg)))
	for _, e := range entries {
		// (the stubs' .owner / .mode files of a removed one stay: the real guest has none)
		if strings.HasSuffix(e.Name(), ".modavd-vkmode") {
			g.t.Errorf("%s: temporary file %s left", what, e.Name())
		}
	}
}

func TestVulkanModeNotInstalled(t *testing.T) {
	g := newVKGuest(t)
	if out := g.run("once"); strings.TrimSpace(out) != "" {
		t.Errorf("output %q", out)
	}
	for _, pkg := range []string{vkPkg, vkBili} {
		if g.exists(vkMedia + "/" + pkg) {
			t.Errorf("made folders for %s, which is not installed", pkg)
		}
	}
	out := g.run("status")
	if !strings.Contains(out, "watchers: \n") || !strings.Contains(out, vkPkg+": not installed") ||
		!strings.Contains(out, vkBili+": not installed") {
		t.Errorf("status %q", out)
	}
}

// Installed and never started: the folders are made as the system makes them, both files are 01; the next passes have
// nothing to do.
func TestVulkanModeBeforeFirstStart(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkPkg, vkUID)
	out := g.run("once")
	if !strings.Contains(out, vkPkg+": set to Vulkan") {
		t.Errorf("output %q", out)
	}
	g.wantVulkan(vkPkg, vkUID, "first pass")
	if g.exists(vkMedia + "/" + vkBili) {
		t.Error("made folders for the bilibili package, which is not installed")
	}
	if out := g.run("status"); !strings.Contains(out, vkPkg+": set to Vulkan") || !strings.Contains(out, vkBili+": not installed") {
		t.Errorf("status %q", out)
	}
	n := g.mvCalls()
	if n != 2 {
		t.Errorf("%d moves, want 2", n)
	}
	for k := 0; k < 2; k++ {
		if out := g.run("once"); strings.TrimSpace(out) != "" {
			t.Errorf("pass %d: output %q", k+2, out)
		}
	}
	if g.mvCalls() != n {
		t.Errorf("later passes wrote again (%d moves)", g.mvCalls())
	}
	g.wantVulkan(vkPkg, vkUID, "later passes")
	if out := g.run("status"); !strings.Contains(out, vkPkg+": set to Vulkan") {
		t.Errorf("status after the later passes %q", out)
	}
}

// Started once without it: the game chose GLES on its own (vulkancaps.bin 03, no customvulkancaps.bin) and closed.
func TestVulkanModeAfterTheGameChoseGLES(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkPkg, vkUID)
	g.launched(vkPkg, vkUID)
	g.write(vkFiles(vkPkg)+"/vulkancaps.bin", "\x03")
	g.write(vkFiles(vkPkg)+"/vulkancaps.bin.owner", vkUID+":1078")
	g.write(vkFiles(vkPkg)+"/vulkancaps.bin.mode", "660")
	g.write(vkFiles(vkPkg)+"/other.dat", "game data")
	g.run("once")
	g.wantVulkan(vkPkg, vkUID, "after GLES")
	if g.raw(vkFiles(vkPkg)+"/other.dat") != "game data" {
		t.Error("another file of the game was touched")
	}
}

// customvulkancaps.bin there: a mode was chosen (in the game, or by an earlier pass), whatever the files hold.
func TestVulkanModeChosenAlready(t *testing.T) {
	for _, c := range []struct{ custom, caps string }{{"\x01", "\x01"}, {"\x03", "\x03"}, {"\x02", ""}, {"", "\x03"}} {
		g := newVKGuest(t)
		g.install(vkPkg, vkUID)
		g.launched(vkPkg, vkUID)
		g.write(vkFiles(vkPkg)+"/customvulkancaps.bin", c.custom)
		if c.caps != "" {
			g.write(vkFiles(vkPkg)+"/vulkancaps.bin", c.caps)
		}
		out := g.run("once")
		if g.raw(vkFiles(vkPkg)+"/customvulkancaps.bin") != c.custom || g.raw(vkFiles(vkPkg)+"/vulkancaps.bin") != c.caps ||
			g.mvCalls() != 0 {
			t.Errorf("custom %q caps %q: changed to %q %q", c.custom, c.caps,
				g.raw(vkFiles(vkPkg)+"/customvulkancaps.bin"), g.raw(vkFiles(vkPkg)+"/vulkancaps.bin"))
		}
		if !strings.Contains(out, vkPkg+": display mode chosen already") {
			t.Errorf("custom %q caps %q: output %q", c.custom, c.caps, out)
		}
		if out := g.run("status"); !strings.Contains(out, vkPkg+": display mode chosen already") {
			t.Errorf("status %q", out)
		}
	}
}

// Nothing is done while the game runs; once it has exited, the next pass sets it.
func TestVulkanModeNeverUnderARunningGame(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkPkg, vkUID)
	g.write("/running", vkUID+"\n")
	g.run("once")
	if g.exists(vkMedia + "/" + vkPkg) {
		t.Error("made folders under a running game")
	}
	if out := g.run("status"); !strings.Contains(out, vkPkg+": running, set once it exits") {
		t.Errorf("status %q", out)
	}
	// the game made its folders and chose GLES meanwhile
	g.launched(vkPkg, vkUID)
	g.write(vkFiles(vkPkg)+"/vulkancaps.bin", "\x03")
	g.run("once")
	if g.raw(vkFiles(vkPkg)+"/vulkancaps.bin") != "\x03" || g.exists(vkFiles(vkPkg)+"/customvulkancaps.bin") {
		t.Error("wrote under a running game")
	}
	g.remove("/running")
	g.run("once")
	g.wantVulkan(vkPkg, vkUID, "game exited")
}

// The two listed packages are independent of each other.
func TestVulkanModeBothListedGames(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkBili, vkBUID)
	g.run("once")
	g.wantVulkan(vkBili, vkBUID, "bilibili alone")
	if g.exists(vkMedia + "/" + vkPkg) {
		t.Error("made folders for the official package, which is not installed")
	}

	// the official one installed and running, bilibili's mode chosen in the game since
	g.install(vkPkg, vkUID)
	g.write("/running", vkUID+"\n")
	g.write(vkFiles(vkBili)+"/customvulkancaps.bin", "\x03")
	g.write(vkFiles(vkBili)+"/vulkancaps.bin", "\x03")
	g.run("once")
	if g.exists(vkMedia+"/"+vkPkg) || g.raw(vkFiles(vkBili)+"/customvulkancaps.bin") != "\x03" ||
		g.raw(vkFiles(vkBili)+"/vulkancaps.bin") != "\x03" {
		t.Error("one package's state changed what was done to the other")
	}
	out := g.run("status")
	if !strings.Contains(out, vkPkg+": running, set once it exits") || !strings.Contains(out, vkBili+": set to Vulkan") {
		t.Errorf("status %q", out)
	}
	g.remove("/running")
	g.run("once")
	g.wantVulkan(vkPkg, vkUID, "official after it exited")
	if g.raw(vkFiles(vkBili)+"/vulkancaps.bin") != "\x03" {
		t.Error("bilibili's choice was overwritten")
	}
}

// A pass that fails half way is finished by the next one; a folder that cannot be given its owner is not left.
func TestVulkanModeFailures(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkPkg, vkUID)

	// the package's folder cannot be given its owner
	g.write("/chown.fail", "/"+vkPkg)
	out := g.run("once")
	if g.exists(vkMedia + "/" + vkPkg) {
		t.Error("a folder without its owner was left")
	}
	if !strings.Contains(out, vkPkg+": cannot make /data/media/0/Android/data/"+vkPkg+"/files") {
		t.Errorf("output %q", out)
	}
	if out := g.run("status"); !strings.Contains(out, vkPkg+": cannot make ") {
		t.Errorf("status %q", out)
	}

	// files/ cannot be given its owner: the package's folder stays (it is right), files/ goes
	g.write("/chown.fail", "/files")
	g.run("once")
	if !g.exists(vkMedia+"/"+vkPkg) || g.exists(vkFiles(vkPkg)) {
		t.Errorf("package folder %v, files folder %v", g.exists(vkMedia+"/"+vkPkg), g.exists(vkFiles(vkPkg)))
	}
	g.remove("/chown.fail")

	// customvulkancaps.bin does not get into place: the next pass writes it
	g.write("/mv.fail", "customvulkancaps.bin")
	out = g.run("once")
	if g.exists(vkFiles(vkPkg)+"/customvulkancaps.bin") || !strings.Contains(out, "cannot write the display mode files") {
		t.Errorf("after a failed move: output %q", out)
	}
	g.wantNoTemp(vkPkg, "failed move")
	g.remove("/mv.fail")
	out = g.run("once")
	if !strings.Contains(out, vkPkg+": set to Vulkan") {
		t.Errorf("output %q", out)
	}
	g.wantVulkan(vkPkg, vkUID, "after the failures")
}

// The package's folder already there (made by the system) is kept as it is; only files/ is made.
func TestVulkanModeKeepsExistingFolder(t *testing.T) {
	g := newVKGuest(t)
	g.install(vkPkg, vkUID)
	p := vkMedia + "/" + vkPkg
	if err := os.MkdirAll(g.path(p), 0o755); err != nil {
		t.Fatal(err)
	}
	g.write(p+".owner", vkUID+":1078")
	g.write(p+".mode", "2770")
	g.write(p+".label", vkLabel)
	g.write(p+"/cache/x", "kept")
	g.run("once")
	g.wantVulkan(vkPkg, vkUID, "existing folder")
	if g.raw(p+"/cache/x") != "kept" {
		t.Error("the existing folder's content changed")
	}
}
