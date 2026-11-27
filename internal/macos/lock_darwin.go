//go:build darwin && cgo

package macos

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"modavd/internal/errs"
)

const lockPollInterval = 100 * time.Millisecond

// TryLock takes the lock now or returns ok=false when someone else holds it.
func TryLock(path string) (*FileLock, bool, error) { return Lock(path, 0) }

// Lock creates the lock file and its directory, then waits up to timeout for an
// exclusive flock. A busy lock is not an error; other failures are reported.
func Lock(path string, timeout time.Duration) (*FileLock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了锁文件目录 %s: %v", filepath.Dir(path), err))
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, err, fmt.Sprintf("打不开锁文件 %s: %v", path, err))
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &FileLock{f: f}, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, false, errs.Wrap(errs.Internal, err, fmt.Sprintf("锁文件 %s 加锁失败: %v", path, err))
		}
		left := time.Until(deadline)
		if left <= 0 {
			f.Close()
			return nil, false, nil
		}
		time.Sleep(min(lockPollInterval, left))
	}
}

// Unlock releases and closes; nil-safe. Keep the file so later contenders lock
// the same inode instead of creating a second lock under an existing holder.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close() // closing also releases the lock when explicit unlocking fails
	l.f = nil
}

// ExclusiveLockHeld is a read-only shared-lock probe. Missing files are free;
// never create a file or directory just to inspect lifecycle state.
func ExclusiveLockHeld(path string) (bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("锁文件不是普通文件")
	}
	err = syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		return false, syscall.Flock(fd, syscall.LOCK_UN)
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}
