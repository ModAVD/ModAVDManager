package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// lfGuest is a fake guest for libfix.sh under a POSIX sh (Git's on Windows): MODAVD_LIBFIX_ROOT is a temporary
// directory standing for /, with a fake /proc/self/mountinfo. mount, umount, chcon, ls and setprop are stubs on PATH:
//   - mount --bind <src> <dst> keeps the file under it as <dst>.under, copies src over dst and adds dst's guest path to
//     mountinfo; umount [-l] <dst> puts the file under it back and takes the line out (umount.log records the calls);
//   - a file's label is in <file>.label (none: shell_data_file); chcon writes it, ls -Z prints it;
//   - setprop appends its arguments to setprop.log;
//   - chcon and mount fail (doing nothing) on a path containing what <root>/chcon.fail or mount.fail holds.
type lfGuest struct {
	t        *testing.T
	sh, root string
	env      []string
}

// lfLib is one library of the fake image: its guest path, cache and upload paths, and the original and patched
// contents.
type lfLib struct {
	target, cache, upload string
	orig, fix             string
}

var (
	lfNDK = lfLib{"/system/lib64/libndk_translation.so", "/data/local/tmp/libndk_translation.hintnop1.so",
		"/data/local/tmp/ndkfix-upload.so", "ndk original", "ndk patched"}
	lfVK = lfLib{"/vendor/lib64/libvulkan_enc.so", "/data/local/tmp/libvulkan_enc.khrfix-bindlock2.so",
		"/data/local/tmp/vulkanfix-upload.so", "vulkan original", "vulkan patched"}
)

func lfSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (l lfLib) args() []string {
	return []string{l.target, l.cache, l.upload, lfSum(l.orig), lfSum(l.fix)}
}

func newLFGuest(t *testing.T) *lfGuest {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"sha256sum", "cut", "grep", "cp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	base := testutil.TempDir(t)
	if strings.ContainsAny(base, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	g := &lfGuest{t: t, sh: sh, root: filepath.ToSlash(filepath.Join(base, "root"))}
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	g.write("/proc/self/mountinfo", "22 1 253:0 / / ro,relatime - ext4 /dev/root ro\n")
	g.write("/data/local/tmp/.keep", "")
	for _, l := range []lfLib{lfNDK, lfVK} {
		g.write(l.target, l.orig)
	}
	g.write(lfNDK.target+".label", "u:object_r:system_lib_file:s0")
	g.write(lfVK.target+".label", "u:object_r:vendor_file:s0")

	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nR=$MODAVD_LIBFIX_ROOT\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fails := func(what, path string) string {
		return `f=$(cat "$R/` + what + `.fail" 2>/dev/null)
if [ -n "$f" ]; then case "` + path + `" in *"$f"*) echo "` + what + `: failed" >&2; exit 1 ;; esac; fi
`
	}
	stub("mount", `[ "$1" = --bind ] || { echo "mount: unexpected $*" >&2; exit 1; }
`+fails("mount", "$3")+`[ -e "$3.under" ] || cp "$3" "$3.under"
cp "$2" "$3"
echo "40 22 253:5 /local/tmp/x ${3#$R} rw,relatime - ext4 /dev/block/vdc rw" >> "$R/proc/self/mountinfo"`)
	stub("umount", `[ "$1" = -l ] && shift
echo "$1" >> "$R/umount.log"
cp "$1.under" "$1" && rm -f "$1.under"
grep -vF " ${1#$R} " "$R/proc/self/mountinfo" > "$R/mountinfo.new"; cp "$R/mountinfo.new" "$R/proc/self/mountinfo"`)
	stub("chcon", fails("chcon", "$2")+`printf %s "$1" > "$2.label"`)
	stub("ls", `[ "$1" = -Z ] || { echo "ls: unexpected $*" >&2; exit 1; }
[ -e "$2" ] || exit 1
echo "$(cat "$2.label" 2>/dev/null || echo u:object_r:shell_data_file:s0) $2"`)
	stub("setprop", `echo "$*" >> "$R/setprop.log"`)
	g.env = append(os.Environ(), "MODAVD_LIBFIX_ROOT="+g.root, "MODAVD_PS_STUBS="+filepath.ToSlash(bin))
	return g
}

func (g *lfGuest) path(p string) string { return filepath.FromSlash(g.root + p) }

func (g *lfGuest) write(p, data string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(g.path(p)), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(g.path(p), []byte(data), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *lfGuest) read(p string) (string, bool) {
	b, err := os.ReadFile(g.path(p))
	return string(b), err == nil
}

// run runs `sh libfix.sh <args>`: its status per target (what follows "<target> "), its last line and its output.
func (g *lfGuest) run(args ...string) (map[string]string, string, string) {
	g.t.Helper()
	b, err := Script(LibFix)
	if err != nil {
		g.t.Fatal(err)
	}
	script := g.path("/data/local/tmp/libfix.sh")
	if err := os.WriteFile(script, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command(g.sh, append([]string{"-c", stubbedSh, "sh", filepath.ToSlash(script)}, args...)...)
	cmd.Env = g.env
	out, _ := cmd.CombinedOutput()
	status := map[string]string{}
	last := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		last = line
		for _, l := range []lfLib{lfNDK, lfVK} {
			if rest, ok := strings.CutPrefix(line, l.target+" "); ok {
				status[l.target] = rest
			}
		}
	}
	return status, last, string(out)
}

func (g *lfGuest) mount(libs ...lfLib) (map[string]string, string, string) {
	args := []string{"mount"}
	for _, l := range libs {
		args = append(args, l.args()...)
	}
	return g.run(args...)
}

func (g *lfGuest) restarts() int {
	log, _ := g.read("/setprop.log")
	if n := strings.Count(log, "ctl.restart zygote"); n != strings.Count(log, "\n") {
		g.t.Errorf("setprop calls: %q", log)
	}
	return strings.Count(log, "ctl.restart zygote")
}

func (g *lfGuest) mountedOver(target string) bool {
	mi, _ := g.read("/proc/self/mountinfo")
	return strings.Contains(mi, " "+target+" ")
}

// cache puts the guest's cached patched copy of l in place (as an earlier boot left it).
func (g *lfGuest) cache(l lfLib) { g.write(l.cache, l.fix) }

func TestLibFixScriptEmbedded(t *testing.T) {
	b, err := Script(LibFix)
	if err != nil || len(b) == 0 || strings.ContainsRune(string(b), '\r') {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
}

// TestLibFixMount: with both caches in place both are mounted (relabelled like their targets), zygote restarts once;
// the next run finds them mounted and changes nothing.
func TestLibFixMount(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfNDK)
	g.cache(lfVK)
	st, last, out := g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "MOUNTED "+lfSum(lfNDK.fix) || st[lfVK.target] != "MOUNTED "+lfSum(lfVK.fix) || last != "RESTARTED" {
		t.Fatalf("mount: %v %s\n%s", st, last, out)
	}
	for _, l := range []lfLib{lfNDK, lfVK} {
		if got, _ := g.read(l.target); got != l.fix || !g.mountedOver(l.target) {
			t.Errorf("%s: %q mounted %v", l.target, got, g.mountedOver(l.target))
		}
		want, _ := g.read(l.target + ".label")
		if got, _ := g.read(l.cache + ".label"); got != want {
			t.Errorf("%s: cache label %q, want %q", l.target, got, want)
		}
	}
	if n := g.restarts(); n != 1 {
		t.Errorf("zygote restarted %d times, want once", n)
	}

	st, last, out = g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "ALREADY "+lfSum(lfNDK.fix) || st[lfVK.target] != "ALREADY "+lfSum(lfVK.fix) || last != "UNCHANGED" {
		t.Errorf("again: %v %s\n%s", st, last, out)
	}
	if n := g.restarts(); n != 1 {
		t.Errorf("zygote restarted %d times after the second run, want once", n)
	}
}

// TestLibFixUpload: while one library waits for its upload nothing is mounted (the other says READY); the run after
// the upload takes it into the cache and mounts both.
func TestLibFixUpload(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfVK)
	g.write(lfNDK.upload, "half an upload")
	st, last, out := g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "NEED_UPLOAD" || st[lfVK.target] != "READY" || last != "UNCHANGED" {
		t.Fatalf("first run: %v %s\n%s", st, last, out)
	}
	if g.mountedOver(lfVK.target) || g.mountedOver(lfNDK.target) || g.restarts() != 0 {
		t.Fatal("mounted or restarted while an upload was missing")
	}
	if _, ok := g.read(lfNDK.upload); ok {
		t.Error("the bad upload was left")
	}

	g.write(lfNDK.upload, lfNDK.fix)
	st, last, out = g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "MOUNTED "+lfSum(lfNDK.fix) || st[lfVK.target] != "MOUNTED "+lfSum(lfVK.fix) || last != "RESTARTED" {
		t.Fatalf("after the upload: %v %s\n%s", st, last, out)
	}
	if got, _ := g.read(lfNDK.cache); got != lfNDK.fix {
		t.Errorf("cache %q", got)
	}
	if _, ok := g.read(lfNDK.upload); ok {
		t.Error("the upload was left after it was taken into the cache")
	}
	if n := g.restarts(); n != 1 {
		t.Errorf("zygote restarted %d times, want once", n)
	}
}

// TestLibFixImageMismatch: an image library with another hash is left alone (and its cache is not used); the other
// library is still mounted.
func TestLibFixImageMismatch(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfNDK)
	g.cache(lfVK)
	g.write(lfNDK.target, "another release")
	st, last, out := g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "SKIPPED image "+lfSum("another release") || st[lfVK.target] != "MOUNTED "+lfSum(lfVK.fix) ||
		last != "RESTARTED" {
		t.Fatalf("%v %s\n%s", st, last, out)
	}
	if got, _ := g.read(lfNDK.target); got != "another release" || g.mountedOver(lfNDK.target) {
		t.Errorf("the unexpected library was touched: %q", got)
	}
}

// TestLibFixNothingToDo: no such file, or the image's own already patched: no mount, no restart.
func TestLibFixNothingToDo(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfNDK)
	g.cache(lfVK)
	os.Remove(g.path(lfNDK.target))
	g.write(lfVK.target, lfVK.fix)
	st, last, out := g.mount(lfVK, lfNDK)
	if st[lfNDK.target] != "ABSENT" || st[lfVK.target] != "IMAGE_PATCHED" || last != "UNCHANGED" || g.restarts() != 0 {
		t.Fatalf("%v %s\n%s", st, last, out)
	}
}

// TestLibFixFailures: a chcon or mount that does not take leaves that library unmounted; the other is mounted.
func TestLibFixFailures(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfNDK)
	g.cache(lfVK)
	g.write("/chcon.fail", "libndk")
	st, last, out := g.mount(lfVK, lfNDK)
	if !strings.HasPrefix(st[lfNDK.target], "FAILED chcon u:object_r:system_lib_file:s0 -> ") ||
		st[lfVK.target] != "MOUNTED "+lfSum(lfVK.fix) || last != "RESTARTED" {
		t.Fatalf("chcon: %v %s\n%s", st, last, out)
	}
	if g.mountedOver(lfNDK.target) {
		t.Error("mounted although chcon failed")
	}

	g = newLFGuest(t)
	g.cache(lfNDK)
	g.write("/mount.fail", "libndk")
	st, last, out = g.mount(lfNDK)
	if st[lfNDK.target] != "FAILED mount "+lfSum(lfNDK.orig) || last != "UNCHANGED" || g.restarts() != 0 {
		t.Fatalf("mount: %v %s\n%s", st, last, out)
	}
	if got, _ := g.read(lfNDK.target); got != lfNDK.orig || g.mountedOver(lfNDK.target) {
		t.Errorf("after a failed mount: %q", got)
	}
}

// TestLibFixRevert: revert takes the patched copy away (the image's file is back, zygote restarts), leaves a mount
// that is not ours alone, and a second revert finds nothing to do.
func TestLibFixRevert(t *testing.T) {
	g := newLFGuest(t)
	g.cache(lfNDK)
	g.cache(lfVK)
	if _, last, out := g.mount(lfVK, lfNDK); last != "RESTARTED" {
		t.Fatalf("mount: %s\n%s", last, out)
	}
	st, last, out := g.run("revert", lfNDK.target, lfSum(lfNDK.fix))
	if st[lfNDK.target] != "REVERTED "+lfSum(lfNDK.orig) || last != "RESTARTED" {
		t.Fatalf("revert: %v %s\n%s", st, last, out)
	}
	if got, _ := g.read(lfNDK.target); got != lfNDK.orig || g.mountedOver(lfNDK.target) {
		t.Errorf("after revert: %q, mounted %v", got, g.mountedOver(lfNDK.target))
	}
	if log, _ := g.read("/umount.log"); !strings.Contains(log, lfNDK.target) {
		t.Errorf("umount calls: %q", log)
	}
	if got, _ := g.read(lfVK.target); got != lfVK.fix || !g.mountedOver(lfVK.target) {
		t.Error("revert of one library touched the other")
	}
	if n := g.restarts(); n != 2 {
		t.Errorf("zygote restarted %d times, want twice (mount, revert)", n)
	}

	st, last, out = g.run("revert", lfNDK.target, lfSum(lfNDK.fix), lfVK.target, lfSum("someone else's"))
	if st[lfNDK.target] != "NOT_MOUNTED" || st[lfVK.target] != "OTHER "+lfSum(lfVK.fix) || last != "UNCHANGED" {
		t.Errorf("second revert: %v %s\n%s", st, last, out)
	}
	if !g.mountedOver(lfVK.target) || g.restarts() != 2 {
		t.Error("a mount that is not ours was taken away")
	}
}

func TestLibFixUsage(t *testing.T) {
	g := newLFGuest(t)
	for _, args := range [][]string{{}, {"mount"}, {"mount", "a", "b"}, {"revert", "a"}, {"frobnicate"}} {
		_, last, out := g.run(args...)
		if !strings.HasPrefix(last, "usage: ") {
			t.Errorf("%q: %s", args, out)
		}
	}
	if g.restarts() != 0 {
		t.Error("a usage error restarted zygote")
	}
}
