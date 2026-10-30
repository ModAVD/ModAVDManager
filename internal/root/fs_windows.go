//go:build windows

package root

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Win32 bits the standard syscall package lacks. Kept here instead of internal/win so the low-level root package
// does not depend on the process / window code.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2

	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// OpenShared opens an existing file for reading with FILE_SHARE_DELETE as well as read / write sharing. Errors are
// *fs.PathError, so errors.Is(err, fs.ErrNotExist) works as with os.Open.
//
// Go's os.Open shares read / write only. While such a handle is open, deleting the file fails with a sharing
// violation, and so does an rmtree of a directory holding it: AUTO-MAS deleting an instance (delete_instance_files)
// while a modavd status poll reads its config.ini would stop half-way. With delete sharing the delete goes through
// (Windows 10+ deletes with POSIX semantics, measured 10-28 on NTFS). It does not help a rename over the file
// (MoveFileEx, i.e. Python's os.replace and Go's os.Rename: write_ini, the mas-avd.json writers) or a rename of a
// directory above it (the updater's sdk\emulator swap): those fail with ERROR_ACCESS_DENIED while any handle to the
// file is open, whatever its sharing. Only a short read helps there, so callers read the whole file and close it.
func OpenShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// ReadFileShared is os.ReadFile through OpenShared.
func ReadFileShared(path string) ([]byte, error) {
	f, err := OpenShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// retryableReplaceError: the target (or the temp file) is open in another process without FILE_SHARE_DELETE, or
// being scanned; Python reports these as PermissionError, which is what AUTO-MAS retries.
func retryableReplaceError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errorAccessDenied || errno == errorSharingViolation || errno == errorLockViolation
}

// fileLock is an exclusive LockFileEx lock on the first byte of a lock file. The system drops it when the handle is
// closed or the process dies, so a killed modavd never leaves the file locked (no stale-lock detection needed). A
// second handle in the same process conflicts too, so it also serializes goroutines.
type fileLock struct {
	f *os.File
}

// lockFile waits up to wait for the lock, polling every 50 ms (a blocking LockFileEx could not be given a timeout).
// ok=false when it is still held by someone else then.
func lockFile(path string, wait time.Duration) (*fileLock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(wait)
	for {
		var ol syscall.Overlapped
		r1, _, e1 := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
			uintptr(unsafe.Pointer(&ol)))
		if r1 != 0 {
			return &fileLock{f: f}, true, nil
		}
		if errno, _ := e1.(syscall.Errno); errno != errorLockViolation && errno != syscall.ERROR_IO_PENDING {
			f.Close()
			return nil, false, e1
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (l *fileLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	var ol syscall.Overlapped
	procUnlockFileEx.Call(l.f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	l.f.Close()
	l.f = nil
}
