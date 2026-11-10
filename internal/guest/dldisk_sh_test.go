package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// ddGuest is a fake guest for modavd-dldisk.sh under a POSIX sh (Git's on Windows): MODAVD_DLDISK_ROOT is a temporary
// directory standing for /, with a fake /proc/self/mountinfo. Stubs on PATH:
//   - stat -c %u / %u:%g / %a and chown, chmod, chcon, ls -Zd keep owner, mode and label in <path>.owner / .mode /
//     .label next to the path;
//   - pgrep -u <uid> prints a pid when <root>/running lists the uid; pgrep -f finds nothing (no watcher);
//   - truncate makes the file, mke2fs writes "ext4" into it and logs the call, tune2fs -l prints the state in
//     <root>/fsstate (clean without it);
//   - losetup keeps its table in <root>/loops; mount adds a line to mountinfo and logs its options, umount takes the
//     line out and frees the loop device of an image, as the kernel does;
//   - df reports <root>/free_kb (30 GB without it); nohup starts nothing;
//   - mke2fs, mount and umount fail (doing nothing) on a path containing what <root>/<name>.fail holds.
//
// What the "image" holds is simply the inner directory: it stays when the image is unmounted and goes when the script
// deletes the disk, like the real thing.
type ddGuest struct {
	t        *testing.T
	sh, root string
	env      []string
}

const (
	ddPkg   = "com.hypergryph.arknights"
	ddUID   = "10194"
	ddRel   = "/files/HGGameUpdate/HGDownload"
	ddMedia = "/data/media/0/Android/data"
	ddView  = "/mnt/user/0/emulated/0/Android/data"
	ddBase  = "/data/local/tmp/modavd-dldisk"
	ddLabel = "u:object_r:media_rw_data_file:s0"
)

func newDDGuest(t *testing.T) *ddGuest {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"grep", "find", "cut", "tr", "head", "awk", "cat", "cp", "rm", "mkdir", "rmdir"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	base := testutil.TempDir(t)
	if strings.ContainsAny(base, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	g := &ddGuest{t: t, sh: sh, root: filepath.ToSlash(filepath.Join(base, "root"))}
	bin := filepath.Join(base, "bin")
	for _, d := range []string{bin, g.path("/data/local/tmp"), g.path("/data/data"), g.path(ddMedia), g.path(ddView)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write(ddMedia+".label", ddLabel)

	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nR=$MODAVD_DLDISK_ROOT\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fails := func(what, path string) string {
		return `f=$(cat "$R/` + what + `.fail" 2>/dev/null)
if [ -n "$f" ]; then case "` + path + `" in *"$f"*) echo "` + what + `: failed" >&2; exit 1 ;; esac; fi
`
	}
	stub("stat", `[ "$1" = -c ] && [ -e "$3" ] || exit 1
case "$2" in
%u) cut -d: -f1 "$3.owner" 2>/dev/null || echo 0 ;;
%u:%g) cat "$3.owner" 2>/dev/null || echo 0:0 ;;
%a) cat "$3.mode" 2>/dev/null || echo 755 ;;
*) echo "stat: unexpected $*" >&2; exit 1 ;;
esac`)
	stub("chown", `printf '%s\n' "$1" > "$2.owner"`)
	stub("chmod", `printf '%s\n' "$1" > "$2.mode"`)
	stub("chcon", `printf '%s\n' "$1" > "$2.label"`)
	stub("ls", `[ "$1" = -Zd ] || { echo "ls: unexpected $*" >&2; exit 1; }
[ -e "$2" ] || exit 1
echo "$(cat "$2.label" 2>/dev/null || echo u:object_r:unlabeled:s0) $2"`)
	stub("pgrep", `[ "$1" = -u ] || exit 1
grep -qx "$2" "$R/running" 2>/dev/null && echo 4242
exit 0`)
	stub("truncate", `[ "$1" = -s ] || exit 1
: > "$3"`)
	stub("mke2fs", `for a in "$@"; do img=$a; done
`+fails("mke2fs", "$img")+`echo ext4 > "$img"
echo "$*" >> "$R/mke2fs.log"`)
	stub("tune2fs", `echo "Filesystem state:         $(cat "$R/fsstate" 2>/dev/null || echo clean)"`)
	stub("losetup", `case "$1" in
-a) cat "$R/loops" 2>/dev/null ;;
-f) n=$(( $(grep -c . "$R/loops" 2>/dev/null) + 7 ))
    echo "/dev/block/loop$n: [fd00]:12 (${3#$R})" >> "$R/loops"; echo "/dev/block/loop$n" ;;
-d) grep -v "^$2:" "$R/loops" > "$R/loops.new"; cp "$R/loops.new" "$R/loops" ;;
esac`)
	// mount -t ext4 -o <options> <device> <dir>, or mount -t overlay overlay -o <options> <dir>
	stub("mount", `[ "$1" = -t ] && [ $# = 6 ] || { echo "mount: unexpected $*" >&2; exit 1; }
if [ "$3" = -o ]; then opts=$4; dev=$5; else opts=$5; dev=$3; fi
dir=$6
`+fails("mount", "$2 $dir")+`echo "$2 $opts ${dir#$R}" >> "$R/mount.log"
echo "50 22 7:56 / ${dir#$R} rw,noatime - $2 $dev rw,$opts" >> "$R/proc/self/mountinfo"`)
	stub("umount", fails("umount", "$1")+`echo "${1#$R}" >> "$R/umount.log"
dev=$(grep " ${1#$R} " "$R/proc/self/mountinfo" | awk '{print $(NF-1)}')
grep -vF " ${1#$R} " "$R/proc/self/mountinfo" > "$R/mountinfo.new"; cp "$R/mountinfo.new" "$R/proc/self/mountinfo"
case "$dev" in /dev/block/loop*) grep -v "^$dev:" "$R/loops" > "$R/loops.new"; cp "$R/loops.new" "$R/loops" ;; esac
exit 0`)
	// "daemon" starts the watcher through nohup: here that starts nothing, so no loop is left running on the host
	stub("nohup", `exit 0`)
	stub("df", `echo "Filesystem 1K-blocks Used Available Use% Mounted on"
echo "/dev/block/dm-40 33109872 1 $(cat "$R/free_kb" 2>/dev/null || echo 30000000) 1% /data"`)
	g.env = append(os.Environ(), "MODAVD_DLDISK_ROOT="+g.root, "MODAVD_PS_STUBS="+filepath.ToSlash(bin))
	return g
}

func (g *ddGuest) path(p string) string { return filepath.FromSlash(g.root + p) }

func (g *ddGuest) write(p, data string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(g.path(p)), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(g.path(p), []byte(data), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *ddGuest) read(p string) string {
	b, _ := os.ReadFile(g.path(p))
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\r", ""))
}

func (g *ddGuest) exists(p string) bool {
	_, err := os.Stat(g.path(p))
	return err == nil
}

func (g *ddGuest) mkdir(p string) {
	g.t.Helper()
	if err := os.MkdirAll(g.path(p), 0o755); err != nil {
		g.t.Fatal(err)
	}
}

func (g *ddGuest) remove(p string) {
	g.t.Helper()
	if err := os.RemoveAll(g.path(p)); err != nil {
		g.t.Fatal(err)
	}
}

// run runs `sh modavd-dldisk.sh <action>` and returns its output.
func (g *ddGuest) run(action string) string {
	g.t.Helper()
	b, err := Script(DownloadDisk)
	if err != nil {
		g.t.Fatal(err)
	}
	script := g.path("/data/local/tmp/modavd-dldisk.sh")
	if err := os.WriteFile(script, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command(g.sh, "-c", stubbedSh, "sh", filepath.ToSlash(script), action)
	cmd.Env = g.env
	b, _ = cmd.CombinedOutput()
	out := strings.ReplaceAll(string(b), "\r", "")
	g.t.Logf("%s: %s", action, out) // shown when the test fails
	return out
}

func (g *ddGuest) mountedOn(p string) bool {
	return strings.Contains(g.read("/proc/self/mountinfo")+"\n", " "+p+" ")
}

// install makes the package's private data directory, which is how the script tells an installed app and its uid.
func (g *ddGuest) install() {
	g.mkdir("/data/data/" + ddPkg)
	g.write("/data/data/"+ddPkg+".owner", ddUID+":"+ddUID)
}

// launched makes the folders the app gets from the system at its first start, and the index the game writes there.
func (g *ddGuest) launched() {
	p := ddMedia
	for _, seg := range strings.Split(ddPkg+ddRel, "/") {
		p += "/" + seg
		g.mkdir(p)
		g.write(p+".owner", ddUID+":1078")
		g.write(p+".mode", "2770")
		g.write(p+".label", ddLabel)
	}
	g.write(ddMedia+"/"+ddPkg+ddRel+"/index.json", "{}")
}

func (g *ddGuest) calls(log string) int {
	if s := g.read("/" + log); s != "" {
		return strings.Count(s, "\n") + 1
	}
	return 0
}

const (
	ddDisk  = ddBase + "/" + ddPkg
	ddInner = ddDisk + "/inner"
	ddLower = ddMedia + "/" + ddPkg + ddRel
	ddOver  = ddView + "/" + ddPkg + ddRel
)

func (g *ddGuest) wantOnDisk(what string) {
	g.t.Helper()
	if !g.mountedOn(ddInner) || !g.mountedOn(ddOver) {
		g.t.Fatalf("%s: not on its disk; mountinfo:\n%s", what, g.read("/proc/self/mountinfo"))
	}
}

func (g *ddGuest) wantNoDisk(what string) {
	g.t.Helper()
	if g.mountedOn(ddInner) || g.mountedOn(ddOver) || g.exists(ddDisk) {
		g.t.Fatalf("%s: a disk is there; mountinfo:\n%s", what, g.read("/proc/self/mountinfo"))
	}
}

func TestDownloadDiskNotInstalled(t *testing.T) {
	g := newDDGuest(t)
	if out := g.run("once"); strings.TrimSpace(out) != "" {
		t.Errorf("output %q", out)
	}
	g.wantNoDisk("nothing installed")
	if g.exists(ddMedia + "/" + ddPkg) {
		t.Error("made folders for a game that is not installed")
	}
	if out := g.run("status"); !strings.Contains(out, ddPkg+": not installed") || !strings.Contains(out, "watchers: \n") {
		t.Errorf("status %q", out)
	}
}

// Installed and never started: the folders are made as the system makes them, so the disk is there before the game's
// first download.
func TestDownloadDiskBeforeFirstStart(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	out := g.run("once")
	g.wantOnDisk("installed, never started")
	if !strings.Contains(out, ddPkg+": disk mounted (new image)") {
		t.Errorf("output %q", out)
	}
	p := ddMedia
	for _, seg := range strings.Split(ddPkg+ddRel, "/") {
		p += "/" + seg
		if g.read(p+".owner") != ddUID+":1078" || g.read(p+".mode") != "2770" || g.read(p+".label") != ddLabel {
			t.Errorf("%s: owner %q mode %q label %q", p, g.read(p+".owner"), g.read(p+".mode"), g.read(p+".label"))
		}
	}
	for _, layer := range []string{"/lower", "/upper"} {
		q := ddInner + layer
		if g.read(q+".owner") != ddUID+":1078" || g.read(q+".mode") != "2770" || g.read(q+".label") != ddLabel {
			t.Errorf("%s: owner %q mode %q label %q", q, g.read(q+".owner"), g.read(q+".mode"), g.read(q+".label"))
		}
	}
	log := g.read("/mount.log")
	if !strings.Contains(log, "ext4 noatime,data=journal,commit=3,discard "+ddInner) {
		t.Errorf("image mount: %q", log)
	}
	if !strings.Contains(log, "overlay lowerdir="+g.root+ddInner+"/lower,upperdir="+g.root+ddInner+"/upper,workdir="+
		g.root+ddInner+"/work,volatile "+ddOver) {
		t.Errorf("overlay mount: %q", log)
	}
	if g.read(ddDisk+"/dl.img.mode") != "600" {
		t.Errorf("image mode %q", g.read(ddDisk+"/dl.img.mode"))
	}
	if out := g.run("status"); !strings.Contains(out, ddPkg+": on its disk") {
		t.Errorf("status %q", out)
	}
	// the next pass has nothing to do
	if out := g.run("once"); strings.TrimSpace(out) != "" || g.calls("mke2fs.log") != 1 || g.calls("mount.log") != 2 {
		t.Errorf("second pass: output %q, %d mke2fs, %d mounts", out, g.calls("mke2fs.log"), g.calls("mount.log"))
	}
}

func TestDownloadDiskNeverUnderARunningGame(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.write("/running", ddUID+"\n")
	g.run("once")
	g.wantNoDisk("game running")
	if out := g.run("status"); !strings.Contains(out, ddPkg+": nothing done yet") {
		t.Errorf("status %q", out)
	}
	// it has exited: the folder holds only the game's index, the disk goes on
	g.remove("/running")
	g.run("once")
	g.wantOnDisk("game exited")
}

// A download that began in the plain folder stays there; a disk left from before is stale by then and goes.
func TestDownloadDiskLeavesAPlainDownloadAlone(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.wantOnDisk("first pass")
	// next boot (nothing mounted), and the plain folder has a package in flight
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	g.write(ddLower+"/file_1/download/a.downloading", "partial")
	out := g.run("once")
	g.wantNoDisk("plain download under way")
	if !strings.Contains(out, "a download is under way in the plain folder") {
		t.Errorf("output %q", out)
	}
	if g.read(ddLower+"/file_1/download/a.downloading") != "partial" {
		t.Error("the plain download was touched")
	}
	// done and cleaned up by the game: only the index is left
	g.remove(ddLower + "/file_1")
	g.run("once")
	g.wantOnDisk("plain download finished")
	if g.calls("mke2fs.log") != 2 {
		t.Errorf("%d images made, want a new one after the stale one went", g.calls("mke2fs.log"))
	}
}

// After a stop of any kind the image is attached again as it is (the kernel replays its journal at mount), and the
// marker of the last volatile mount is taken away first.
func TestDownloadDiskComesBackAfterReboot(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.write(ddInner+"/upper/file_1/download/a.downloading", "in flight")
	g.write(ddInner+"/work/work/incompat/volatile/dirty", "")
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	out := g.run("daemon")
	g.wantOnDisk("after reboot")
	if !strings.Contains(out, "disk mounted (kept image)") || g.calls("mke2fs.log") != 1 {
		t.Errorf("output %q, %d mke2fs", out, g.calls("mke2fs.log"))
	}
	if g.read(ddInner+"/upper/file_1/download/a.downloading") != "in flight" {
		t.Error("the download in flight is gone")
	}
	if g.exists(ddInner + "/work/work/incompat") {
		t.Error("the volatile marker is still there")
	}
}

func TestDownloadDiskUninstalled(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.wantOnDisk("installed")
	g.remove("/data/data/" + ddPkg)
	g.remove(ddMedia + "/" + ddPkg)
	out := g.run("once")
	g.wantNoDisk("uninstalled")
	if !strings.Contains(out, "not installed any more, disk removed") {
		t.Errorf("output %q", out)
	}
}

// App data cleared: the folder under the mount is gone. Whatever the image holds belongs to the app's earlier life.
func TestDownloadDiskAppDataCleared(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.write(ddInner+"/upper/file_1/x.json", "old task")
	g.remove(ddMedia + "/" + ddPkg)
	// not while the game runs
	g.write("/running", ddUID+"\n")
	g.run("once")
	if !g.mountedOn(ddOver) {
		t.Fatal("unmounted under a running game")
	}
	g.remove("/running")
	g.run("once")
	g.wantOnDisk("after the data was cleared")
	if g.exists(ddInner+"/upper/file_1") || g.calls("mke2fs.log") != 2 {
		t.Errorf("the old image was kept (%d mke2fs)", g.calls("mke2fs.log"))
	}
	if !g.exists(ddLower) {
		t.Error("the folder was not made again")
	}

	// the same found at boot: the data was cleared, then the instance was stopped before a pass saw it
	g.write(ddInner+"/upper/file_2/y.json", "task of the earlier life")
	g.remove(ddMedia + "/" + ddPkg)
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	g.run("once")
	g.wantOnDisk("at the boot after the data was cleared")
	if g.exists(ddInner+"/upper/file_2") || g.calls("mke2fs.log") != 3 {
		t.Errorf("the old image was kept at boot (%d mke2fs)", g.calls("mke2fs.log"))
	}
}

func TestDownloadDiskBadImage(t *testing.T) {
	// does not mount: removed in this pass, a new one in the next
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	g.write("/mount.fail", "ext4")
	out := g.run("once")
	g.wantNoDisk("image does not mount")
	if !strings.Contains(out, "the image does not mount, starting a new one") {
		t.Errorf("output %q", out)
	}
	g.remove("/mount.fail")
	g.run("once")
	g.wantOnDisk("new image")

	// marked with errors (its journal was aborted, e.g. /data ran full): not mounted at all
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	g.write("/fsstate", "not clean with errors")
	n := g.calls("mount.log")
	out = g.run("once")
	if !strings.Contains(out, "the image has file system errors, starting a new one") {
		t.Errorf("output %q", out)
	}
	g.remove("/fsstate")
	g.run("once")
	g.wantOnDisk("after the image with errors")
	if g.calls("mount.log") != n+2 || g.calls("mke2fs.log") != 3 {
		t.Errorf("%d mounts since, %d mke2fs", g.calls("mount.log")-n, g.calls("mke2fs.log"))
	}

	// went read-only while mounted: replaced once the game is not running
	mi := strings.Replace(g.read("/proc/self/mountinfo"), ddInner+" rw,noatime", ddInner+" ro,noatime", 1)
	g.write("/proc/self/mountinfo", mi+"\n")
	g.write("/running", ddUID+"\n")
	g.run("once")
	if !g.mountedOn(ddOver) {
		t.Fatal("unmounted under a running game")
	}
	g.remove("/running")
	g.run("once")
	g.wantOnDisk("after the read-only image")
	if g.calls("mke2fs.log") != 4 {
		t.Errorf("%d mke2fs, want a new image", g.calls("mke2fs.log"))
	}
}

func TestDownloadDiskCannotBeMade(t *testing.T) {
	// no shared storage yet
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.remove("/mnt/user")
	g.run("once")
	g.wantNoDisk("no shared storage")
	g.mkdir(ddView)

	// /data nearly full
	g.write("/free_kb", "2097151")
	g.run("once")
	g.wantNoDisk("less than 2 GB free")
	if out := g.run("status"); !strings.Contains(out, "less than 2 GB free on /data, no disk") {
		t.Errorf("status %q", out)
	}
	g.write("/free_kb", "2097152")

	// mke2fs fails: nothing is left behind
	g.write("/mke2fs.fail", "dl.img")
	g.run("once")
	g.wantNoDisk("mke2fs failed")
	g.remove("/mke2fs.fail")

	// the overlay does not mount: the image is not left mounted either
	g.write("/mount.fail", "overlay")
	g.run("once")
	if g.mountedOn(ddInner) || g.mountedOn(ddOver) {
		t.Errorf("after a failed overlay mount:\n%s", g.read("/proc/self/mountinfo"))
	}
	g.remove("/mount.fail")
	g.run("once")
	g.wantOnDisk("in the end")
}

func TestDownloadDiskOff(t *testing.T) {
	g := newDDGuest(t)
	g.install()
	g.launched()
	g.run("once")
	g.write(ddInner+"/upper/index.json", `{"task":1}`)
	g.write(ddInner+"/upper/file_1/download/a.downloading", "in flight")

	// a running game keeps its disk
	g.write("/running", ddUID+"\n")
	out := g.run("off")
	g.wantOnDisk("off, game running")
	if !strings.Contains(out, "running, its disk stays until the next boot") {
		t.Errorf("output %q", out)
	}

	// otherwise the downloads in flight go back into the plain folder and the disk goes
	g.remove("/running")
	out = g.run("off")
	g.wantNoDisk("off")
	if !strings.Contains(out, ddPkg+": disk removed") {
		t.Errorf("output %q", out)
	}
	if g.read(ddLower+"/index.json") != `{"task":1}` || g.read(ddLower+"/file_1/download/a.downloading") != "in flight" {
		t.Errorf("plain folder: index %q, package %q", g.read(ddLower+"/index.json"), g.read(ddLower+"/file_1/download/a.downloading"))
	}

	// the same from an image that is not mounted (off at the boot after it was in use)
	g.run("once")
	g.wantNoDisk("a download now sits in the plain folder")
	g.remove(ddLower + "/file_1")
	g.run("once")
	g.wantOnDisk("on again")
	g.write(ddInner+"/upper/file_2/download/b.downloading", "second")
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/loops", "")
	g.run("off")
	g.wantNoDisk("off after a reboot")
	if g.read(ddLower+"/file_2/download/b.downloading") != "second" {
		t.Error("the download in the unmounted image was not copied back")
	}

	// the copy fails (unmount refused): the disk and what it holds stay
	g.remove(ddLower + "/file_2")
	g.run("once")
	g.write(ddInner+"/upper/file_3/c.downloading", "third")
	g.write("/umount.fail", ddOver)
	out = g.run("off")
	if !g.exists(ddInner+"/upper/file_3/c.downloading") || !strings.Contains(out, "disk in use, kept") {
		t.Errorf("after a refused unmount: output %q", out)
	}
}
