package emu

import (
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/logx"
	"modavd/internal/testutil"
)

// No test creates ModAVD in the real Pictures folder: boots in tests have none, unless a test gives one.
func init() {
	picturesDir = func() (string, string) { return "", "tests have no Pictures folder" }
}

// usePictures points picturesDir at dir for the test.
func usePictures(t *testing.T, dir string) {
	t.Helper()
	old := picturesDir
	picturesDir = func() (string, string) { return dir, "" }
	t.Cleanup(func() { picturesDir = old })
}

// TestScreenshotSavePath: <Pictures>\ModAVD, made when missing and kept when there; "" without a Pictures folder or
// when the folder cannot be made (a file in its place).
func TestScreenshotSavePath(t *testing.T) {
	m := &Manager{Log: logx.Nop()}
	if got := m.screenshotSavePath(); got != "" {
		t.Errorf("no Pictures folder: %q", got)
	}
	pictures := testutil.TempDir(t)
	usePictures(t, pictures)
	want := filepath.Join(pictures, "ModAVD")
	for range 2 {
		if got := m.screenshotSavePath(); got != want {
			t.Errorf("screenshotSavePath = %q, want %q", got, want)
		}
		if st, err := os.Stat(want); err != nil || !st.IsDir() {
			t.Errorf("%s not made: %v", want, err)
		}
	}
	blocked := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(blocked, "ModAVD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	usePictures(t, blocked)
	if got := m.screenshotSavePath(); got != "" {
		t.Errorf("ModAVD is a file: %q", got)
	}
}

// TestHostPicturesDir: the real lookup answers with an existing folder (or says why not); it is only read.
func TestHostPicturesDir(t *testing.T) {
	dir, why := hostPicturesDir()
	if dir == "" {
		t.Skipf("no Pictures folder here: %s", why)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || !filepath.IsAbs(dir) {
		t.Errorf("hostPicturesDir = %q: %v", dir, err)
	}
}
