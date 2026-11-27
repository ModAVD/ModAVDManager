package report

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func setHidden(t *testing.T, path string) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(p, attrs|syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
}

// shortName is the 8.3 short name of the last element of path (an existing directory), "" when the volume gives it
// none (8.3 name creation turned off).
func shortName(t *testing.T, path string) string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) >= len(buf) {
		t.Fatalf("GetShortPathName %s: %d, %v", path, n, err)
	}
	s := filepath.Base(syscall.UTF16ToString(buf[:n]))
	if strings.EqualFold(s, filepath.Base(path)) {
		return ""
	}
	return s
}
