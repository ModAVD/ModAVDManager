//go:build windows

package win

import (
	"fmt"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

// ExclusiveLockHeld probes with a shared byte-range lock. Concurrent probes do
// not conflict with each other; only a real exclusive owner reports held=true.
// Closing the handle releases the probe even on error. The file may be created.
func ExclusiveLockHeld(path string) (bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var ol syscall.Overlapped
	r, err := call(procLockFileEx, f.Fd(), lockfileFailImmediately, 0, 0xFFFFFFFF, 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return false, nil
	}
	if err == errorLockViolation {
		return true, nil
	}
	return false, errs.Wrap(errs.Internal, err, fmt.Sprintf("锁文件 %s 查询失败: %v", path, err))
}
