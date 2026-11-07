package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

const (
	glMark = "# modavd: report GLES 3.2 (ModAVD/ModAVD#131)"
	glLine = "ro.opengles.version=196610"
)

// glGuest is a fake guest for gles32.sh under a POSIX sh (Git's on Windows): MODAVD_GLES_ROOT is a temporary directory
// standing for /, remount is a stub, and getprop answers what <root>/prop holds (nothing when the file is missing).
type glGuest struct {
	t       *testing.T
	sh      string
	root    string
	env     []string
	prop    string
	build   string // the guest path of build.prop in the fake
	scriptP string
}

func newGLGuest(t *testing.T) *glGuest {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"grep", "tail", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	base := testutil.TempDir(t)
	if strings.ContainsAny(base, " \t") {
		t.Skip("the temporary directory has a space in its path (the guest's never do)")
	}
	g := &glGuest{t: t, sh: sh, root: filepath.ToSlash(filepath.Join(base, "root"))}
	g.build = g.root + "/vendor/build.prop"
	bin := filepath.Join(base, "bin")
	for _, d := range []string{bin, g.root + "/vendor"} {
		if err := os.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("remount", `echo remount >> "$MODAVD_GLES_ROOT/remount.log"; exit 0`)
	stub("getprop", `[ "$1" = ro.opengles.version ] && cat "$MODAVD_GLES_ROOT/prop" 2>/dev/null; exit 0`)
	g.env = append(os.Environ(), "MODAVD_GLES_ROOT="+g.root, "MODAVD_PS_STUBS="+filepath.ToSlash(bin))
	g.scriptP = filepath.Join(base, "gles32.sh")
	b, err := Script(GLES32)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.scriptP, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *glGuest) write(p, data string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.FromSlash(p)), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(p), []byte(data), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *glGuest) read(p string) string {
	b, err := os.ReadFile(filepath.FromSlash(p))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// setProp is what getprop ro.opengles.version answers ("" for an empty answer).
func (g *glGuest) setProp(v string) { g.write(g.root+"/prop", v) }

func (g *glGuest) run(action string) (string, string, int) {
	g.t.Helper()
	cmd := exec.Command(g.sh, "-c", stubbedSh, "sh", filepath.ToSlash(g.scriptP), action)
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	result := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if w, ok := strings.CutPrefix(line, "RESULT "); ok {
			result = w
		}
	}
	return result, string(out), code
}

const glOriginal = "ro.vendor.build.id=ABC\nro.product.vendor.model=sdk\n# a comment\nro.opengles.version.note=x\n"

func TestGLES32ScriptEmbedded(t *testing.T) {
	b, err := Script(GLES32)
	if err != nil || len(b) == 0 || strings.ContainsRune(string(b), '\r') {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
}

// TestGLES32ScriptOnOff: on remounts and appends the marker and the line, the file's other content is untouched; a
// second on finds it; off takes out both lines and nothing else; a second off finds nothing.
func TestGLES32ScriptOnOff(t *testing.T) {
	g := newGLGuest(t)
	g.write(g.build, glOriginal)

	r, out, code := g.run("on")
	if r != "reboot" || code != 0 {
		t.Fatalf("on: %s %d\n%s", r, code, out)
	}
	if want := glOriginal + glMark + "\n" + glLine + "\n"; g.read(g.build) != want {
		t.Errorf("build.prop after on:\n%q\nwant\n%q", g.read(g.build), want)
	}
	if !strings.Contains(g.read(g.root+"/remount.log"), "remount") {
		t.Error("remount was not run")
	}
	if g.exists("/vendor/.modavd-w") {
		t.Error("the write probe is left")
	}

	if r, out, _ := g.run("on"); r != "already" {
		t.Fatalf("second on: %s\n%s", r, out)
	}
	if want := glOriginal + glMark + "\n" + glLine + "\n"; g.read(g.build) != want {
		t.Errorf("build.prop after the second on: %q", g.read(g.build))
	}

	r, out, code = g.run("off")
	if r != "reboot" || code != 0 {
		t.Fatalf("off: %s %d\n%s", r, code, out)
	}
	if g.read(g.build) != glOriginal {
		t.Errorf("build.prop after off:\n%q\nwant\n%q", g.read(g.build), glOriginal)
	}
	if g.exists("/vendor/build.prop.modavd-tmp") {
		t.Error("the temporary copy is left")
	}
	if r, out, _ := g.run("off"); r != "already" {
		t.Fatalf("second off: %s\n%s", r, out)
	}
	if g.read(g.build) != glOriginal {
		t.Errorf("build.prop after the second off: %q", g.read(g.build))
	}
}

// TestGLES32ScriptOffKeepsOthers: off takes out the marker and the line after it only; a 196610 line without our
// marker, lines around the pair, a file without a final newline and a missing last newline are kept as they are.
func TestGLES32ScriptOffKeepsOthers(t *testing.T) {
	g := newGLGuest(t)
	g.write(g.build, "a=1\n"+glLine+"\n"+glMark+"\n"+glLine+"\nb=2\n  c = 3  \nlast=4")
	if r, out, _ := g.run("off"); r != "reboot" {
		t.Fatalf("off: %s\n%s", r, out)
	}
	if want := "a=1\n" + glLine + "\nb=2\n  c = 3  \nlast=4\n"; g.read(g.build) != want {
		t.Errorf("build.prop:\n%q\nwant\n%q", g.read(g.build), want)
	}

	// no final newline: on starts its pair on a line of its own
	g = newGLGuest(t)
	g.write(g.build, "a=1")
	if r, out, _ := g.run("on"); r != "reboot" {
		t.Fatalf("on: %s\n%s", r, out)
	}
	if want := "a=1\n" + glMark + "\n" + glLine + "\n"; g.read(g.build) != want {
		t.Errorf("build.prop:\n%q\nwant\n%q", g.read(g.build), want)
	}

	// only the marker decides: our line without it is not ours
	g = newGLGuest(t)
	g.write(g.build, glLine+"\n")
	if r, _, _ := g.run("off"); r != "already" {
		t.Errorf("off with a foreign line: %s", r)
	}
	if g.read(g.build) != glLine+"\n" {
		t.Errorf("a foreign line was changed: %q", g.read(g.build))
	}
}

func (g *glGuest) exists(p string) bool {
	_, err := os.Lstat(filepath.FromSlash(g.root + p))
	return err == nil
}

// TestGLES32ScriptKeepsFile: the file is written in place, not replaced: a hard link made to it before sees the change
// (an mv or sed -i would leave the link with the old content).
func TestGLES32ScriptKeepsFile(t *testing.T) {
	g := newGLGuest(t)
	g.write(g.build, glOriginal)
	link := g.root + "/vendor/build.prop.link"
	if err := os.Link(filepath.FromSlash(g.build), filepath.FromSlash(link)); err != nil {
		t.Skip("no hard links here: " + err.Error())
	}
	if r, out, _ := g.run("on"); r != "reboot" {
		t.Fatalf("on: %s\n%s", r, out)
	}
	if !strings.HasSuffix(g.read(link), glLine+"\n") {
		t.Errorf("the link does not see on: %q", g.read(link))
	}
	if r, out, _ := g.run("off"); r != "reboot" {
		t.Fatalf("off: %s\n%s", r, out)
	}
	if g.read(link) != glOriginal {
		t.Errorf("the link does not see off: %q", g.read(link))
	}
}

// TestGLES32ScriptStatus: the four results, the two lines before them.
func TestGLES32ScriptStatus(t *testing.T) {
	cases := []struct {
		marked bool
		prop   string
		want   string
	}{
		{true, "196610", "on"},
		{false, "196609", "off"},
		{false, "", "off"},
		{true, "196609", "pending-on"},
		{true, "", "pending-on"},
		{false, "196610", "pending-off"},
	}
	for _, c := range cases {
		g := newGLGuest(t)
		data := glOriginal
		if c.marked {
			data += glMark + "\n" + glLine + "\n"
		}
		g.write(g.build, data)
		g.setProp(c.prop)
		r, out, code := g.run("status")
		line := "line no"
		if c.marked {
			line = "line yes"
		}
		if r != c.want || code != 0 || !strings.Contains(out, "prop "+c.prop+"\n") || !strings.Contains(out, line+"\n") {
			t.Errorf("marked %v prop %q: %s (want %s)\n%s", c.marked, c.prop, r, c.want, out)
		}
		if g.read(g.build) != data {
			t.Errorf("status changed the file")
		}
	}
	// no build.prop at all
	g := newGLGuest(t)
	os.Remove(filepath.FromSlash(g.build))
	if r, out, _ := g.run("status"); r != "off" || !strings.Contains(out, "line no") {
		t.Errorf("without build.prop: %s\n%s", r, out)
	}
}

// TestGLES32ScriptNotWritable: /vendor that cannot be written to is not-writable (exit code 3) for on and off, and
// nothing is changed.
func TestGLES32ScriptNotWritable(t *testing.T) {
	g := newGLGuest(t)
	// a file where the partition's directory should be: touch inside it fails
	os.RemoveAll(filepath.FromSlash(g.root + "/vendor"))
	g.write(g.root+"/vendor", "not a directory")
	for _, action := range []string{"on", "off"} {
		r, out, code := g.run(action)
		if r != "not-writable" || code != 3 {
			t.Errorf("%s: %s %d\n%s", action, r, code, out)
		}
	}
}

func TestGLES32ScriptUsage(t *testing.T) {
	g := newGLGuest(t)
	if r, _, code := g.run("bogus"); r != "failed" || code != 2 {
		t.Errorf("usage: %s %d", r, code)
	}
}
