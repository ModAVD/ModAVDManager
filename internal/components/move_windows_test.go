package components

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"modavd/internal/testutil"
)

// moveFakes makes the rename fail with err for the first fails calls (all of them for fails < 0), and sleeping free.
func moveFakes(t *testing.T, fails int, err error) *int {
	t.Helper()
	savedRename, savedSleep, savedFor := moveRename, moveSleep, moveRetryFor
	t.Cleanup(func() { moveRename, moveSleep, moveRetryFor = savedRename, savedSleep, savedFor })
	calls := 0
	moveRename = func(src, dst string) error {
		calls++
		if fails < 0 || calls <= fails {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
		}
		return os.Rename(src, dst)
	}
	moveSleep = func(time.Duration) {}
	moveRetryFor = 300 * time.Millisecond
	return &calls
}

func moveTree(t *testing.T) (src, dst string) {
	t.Helper()
	base := testutil.TempDir(t)
	src = filepath.Join(base, ".staging", "playstore")
	for name, text := range map[string]string{"Phonesky.apk": "store", "sub/GmsCore.apk": "gms"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src, filepath.Join(base, "sdk", "playstore")
}

func checkTree(t *testing.T, dst string) {
	t.Helper()
	for name, want := range map[string]string{"Phonesky.apk": "store", "sub/GmsCore.apk": "gms"} {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q, %v", name, b, err)
		}
	}
}

// Access denied for a while (a scanner holds a file just written): retried until the rename goes through.
func TestMoveDirRetriesAccessDenied(t *testing.T) {
	src, dst := moveTree(t)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := moveFakes(t, 3, syscall.ERROR_ACCESS_DENIED)
	if err := moveDir(src, dst, false); err != nil {
		t.Fatal(err)
	}
	if *calls != 4 {
		t.Errorf("%d renames", *calls)
	}
	checkTree(t, dst)
}

// Still denied after the retries: an install copies the tree and removes the source; any other move gives up.
func TestMoveDirCopiesWhenRenameKeepsFailing(t *testing.T) {
	src, dst := moveTree(t)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	moveFakes(t, -1, errorSharingViolation)
	if err := moveDir(src, dst, false); !errors.Is(err, errorSharingViolation) {
		t.Fatalf("without the copy: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("dst made without the copy")
	}
	if err := installComponent(src, dst); err != nil {
		t.Fatal(err)
	}
	checkTree(t, dst)
	if _, err := os.Stat(src); err == nil {
		t.Error("source left behind")
	}
}

// Another error is not retried; a copy that cannot finish leaves no half dst.
func TestMoveDirOtherErrorAndFailedCopy(t *testing.T) {
	src, dst := moveTree(t)
	calls := moveFakes(t, -1, syscall.ERROR_FILE_NOT_FOUND)
	if err := moveDir(src, dst, true); !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || *calls != 1 {
		t.Fatalf("%v after %d renames", err, *calls)
	}

	moveFakes(t, -1, syscall.ERROR_ACCESS_DENIED)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// a directory where a file should go: the copy fails half way
	if err := os.MkdirAll(filepath.Join(src, "zz.apk", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "zz.apk", "x", "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	savedCopy := copyFileFn
	t.Cleanup(func() { copyFileFn = savedCopy })
	copyFileFn = func(from, to string) error {
		if filepath.Base(from) == "f" {
			return errors.New("disk full")
		}
		return savedCopy(from, to)
	}
	err := moveDir(src, dst, true)
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("failed copy: %v", err)
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("half copy left in dst")
	}
	if _, serr := os.Stat(filepath.Join(src, "Phonesky.apk")); serr != nil {
		t.Error("source removed after a failed copy")
	}
}
