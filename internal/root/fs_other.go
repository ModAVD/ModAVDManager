//go:build !windows

package root

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func OpenShared(path string) (*os.File, error)   { return os.Open(path) }
func ReadFileShared(path string) ([]byte, error) { return os.ReadFile(path) }
func retryableReplaceError(err error) bool {
	var e *os.PathError
	return errors.As(err, &e) && (errors.Is(e.Err, syscall.EACCES) || errors.Is(e.Err, syscall.EAGAIN))
}

type fileLock struct{ f *os.File }

func lockFile(path string, wait time.Duration) (*fileLock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{f: f}, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, false, err
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
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
