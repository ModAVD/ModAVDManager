//go:build windows

package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"modavd/internal/errs"
)

// loadDLL puts a copy of a small system DLL at path and loads it into this process, the way MAA loads
// mumu-shim\shell\sdk\external_renderer_ipc.dll; freed at the end of the test (or by the returned func).
func loadDLL(t *testing.T, path string) (free func()) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "msimg32.dll"))
	if err != nil {
		t.Skipf("no system DLL to load: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := syscall.LoadLibrary(path)
	if err != nil {
		t.Fatalf("LoadLibrary %s: %v", path, err)
	}
	freed := false
	free = func() {
		if !freed {
			freed = true
			syscall.FreeLibrary(h)
		}
	}
	t.Cleanup(free)
	return free
}

func TestFileInUseSeesLoadedDLL(t *testing.T) {
	dir := t.TempDir()
	dll := filepath.Join(dir, "external_renderer_ipc.dll")
	free := loadDLL(t, dll)
	if !fileInUse(dll) {
		t.Error("a loaded DLL is not in use")
	}
	free()
	if fileInUse(dll) {
		t.Error("a freed DLL is still in use")
	}
	plain := filepath.Join(dir, "plain.txt")
	os.WriteFile(plain, []byte("x"), 0o644)
	readOnly := filepath.Join(dir, "readonly.txt")
	os.WriteFile(readOnly, []byte("x"), 0o444)
	for _, p := range []string{plain, readOnly, filepath.Join(dir, "missing.dll")} {
		if fileInUse(p) {
			t.Errorf("%s counts as in use", p)
		}
	}
	if b, _ := os.ReadFile(plain); string(b) != "x" {
		t.Errorf("the check changed the file: %q", b)
	}
}

// TestInstallOverThirdPartyShim: AUTO-MAS copies its own external_renderer_ipc.dll to the same path, so the root's
// file differs from the root manifest. A full package install takes the package's copy (the other one is kept in the
// backup until the install is done) and does not fail.
func TestInstallOverThirdPartyShim(t *testing.T) {
	const shim = "mumu-shim/shell/sdk/external_renderer_ipc.dll"
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, map[string]string{shim: "shim 36"})
	if err := os.WriteFile(filepath.Join(r.Dir, filepath.FromSlash(shim)), []byte("AUTO-MAS copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, map[string]string{shim: "shim 36"}), false))
	u := f.updater(r)
	u.scan = func(bool) ([]processUse, error) { return nil, nil }
	if st, err := u.Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("install: %+v %v", st, err)
	}
	if got := read(t, r, shim); got != "shim 36" {
		t.Errorf("shim = %q", got)
	}
}

// TestInstallBlockedByLoadedDLL: MAA holding the MuMu shell (or extras\modavd_extras.dll) loaded stops the install
// before anything is swapped, with a message that says which file and to close MAA; the stage stays installable and
// the next install, once MAA is closed, goes through.
func TestInstallBlockedByLoadedDLL(t *testing.T) {
	const shim = "mumu-shim/shell/sdk/external_renderer_ipc.dll"
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, map[string]string{shim: "shim 36"})
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, map[string]string{shim: "shim 37"}), false))
	u := f.updater(r)
	u.scan = func(bool) ([]processUse, error) { return nil, nil }
	free := loadDLL(t, filepath.Join(r.Dir, filepath.FromSlash(shim)))
	_, err := u.Install(context.Background())
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), `mumu-shim\shell\sdk\external_renderer_ipc.dll`) ||
		!strings.Contains(err.Error(), "MAA") || !strings.Contains(err.Error(), "什么都没动") {
		t.Fatalf("blocked: %v", err)
	}
	if st := u.Status(); st.Phase != phaseStaged || st.PreparedVersion != "v0.37.0" {
		t.Fatalf("after: %+v", st)
	}
	for rel, want := range map[string]string{"sdk/emulator/emulator.exe": "emulator mas-36", "说明.txt": "ModAVD 0.36.0\n",
		"tools/modavd-bridge.ps1": "# bridge 36\n"} {
		if got := read(t, r, rel); got != want {
			t.Errorf("%s = %q after the blocked install", rel, got)
		}
	}
	if lexists(u.l.previous()) {
		t.Error("previous\\ left behind")
	}
	free()
	if st, err := u.Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("install after MAA closed: %+v %v", st, err)
	}
	if got := read(t, r, shim); got != "shim 37" {
		t.Errorf("shim = %q", got)
	}
}
