//go:build windows

package update

import (
	"errors"
	"os"
	"syscall"
)

const (
	fileAttributeReparsePoint = 0x400
	errorSharingViolation     = syscall.Errno(32)
	errorLockViolation        = syscall.Errno(33)
)

// fileInUse: another process holds path so that it can be neither replaced nor deleted, e.g. a DLL MAA has loaded
// (a mapped image refuses write access) or a file opened without sharing. Opened for writing without truncating and
// closed at once; any other error (read-only, missing) is not "in use": the swap reports those itself.
func fileInUse(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		return false
	}
	return errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation)
}

func isReparsePoint(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && sys.FileAttributes&fileAttributeReparsePoint != 0
}
func removeLink(p string) bool {
	if os.Remove(p) == nil {
		return true
	}
	ptr, _ := syscall.UTF16PtrFromString(p)
	return syscall.RemoveDirectory(ptr) == nil
}
