//go:build windows

package win

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/errs"
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
	lockPollInterval        = 100 * time.Millisecond
)

// openLockFile opens (creating it and its directory) the lock file. os.OpenFile shares read / write but not delete,
// so a lock file cannot be deleted and re-created under a holder (which would let a second process lock a new file
// of the same name).
func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了锁文件目录 %s: %v", filepath.Dir(path), err))
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("打不开锁文件 %s: %v", path, err))
	}
	return f, nil
}

// lockFile tries LockFileEx(EXCLUSIVE | FAIL_IMMEDIATELY) over the whole file once. Byte-range locks belong to the
// handle, so a second handle in the same process conflicts like another process does (emu probes its own locks
// that way).
func lockFile(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	r, err := call(procLockFileEx, f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 0xFFFFFFFF, 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return true, nil
	}
	if err == errorLockViolation {
		return false, nil
	}
	return false, errs.Wrap(errs.Internal, err, fmt.Sprintf("锁文件 %s 加锁失败: %v", f.Name(), err))
}

// TryLock takes the lock now or returns ok=false when someone else holds it (creating the file and its directory).
func TryLock(path string) (*FileLock, bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	ok, err := lockFile(f)
	if !ok {
		f.Close()
		return nil, false, err
	}
	return &FileLock{f: f}, true, nil
}

// Lock waits up to timeout for the lock (polling every 100 ms). ok=false on timeout.
func Lock(path string, timeout time.Duration) (*FileLock, bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := lockFile(f)
		if ok {
			return &FileLock{f: f}, true, nil
		}
		if err != nil {
			f.Close()
			return nil, false, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			f.Close()
			return nil, false, nil
		}
		time.Sleep(min(lockPollInterval, left))
	}
}

// Unlock releases and closes; nil-safe. The file itself is left in place.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	var ol syscall.Overlapped
	call(procUnlockFileEx, l.f.Fd(), 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	l.f.Close() // closing the handle would release the lock anyway
	l.f = nil
}
