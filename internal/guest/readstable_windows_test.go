package guest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadStableWaitsForSharingViolation: a file another handle holds without read sharing (a writer that has just
// moved it into place) is read once that handle is closed, not reported as ERROR_SHARING_VIOLATION.
func TestReadStableWaitsForSharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.sh")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); !sharingViolation(err) {
		syscall.CloseHandle(h)
		t.Fatalf("held file: want a sharing violation, got %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		syscall.CloseHandle(h)
	}()
	t0 := time.Now()
	b, err := readStable(path)
	if err != nil || string(b) != "payload" {
		t.Fatalf("readStable = %q, %v", b, err)
	}
	if d := time.Since(t0); d < 150*time.Millisecond {
		t.Errorf("returned after %v, before the handle was closed", d)
	}

	// Other errors are not retried.
	t0 = time.Now()
	if _, err := readStable(filepath.Join(t.TempDir(), "missing.sh")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if d := time.Since(t0); d > 40*time.Millisecond {
		t.Errorf("missing file retried for %v", d)
	}
}
