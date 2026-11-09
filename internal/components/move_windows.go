//go:build windows

package components

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"modavd/internal/root"
)

// moveDir renames a component directory, made to survive what a scanner does on Windows. On a user's computer with
// security software, renaming the Play Store directory failed with ERROR_ACCESS_DENIED a second after its five files
// were written, three fetches in a row: something kept them open, and a directory with an open file cannot be renamed.
// So the rename is retried for moveRetryFor while it fails that way. After that, with copyFallback (the source is a
// staging directory that is ours to throw away), the tree is copied into dst instead (new files, nothing to rename)
// and src removed as far as possible (what is left in the staging directory goes with the next fetch).
func moveDir(src, dst string, copyFallback bool) error {
	err := moveRename(src, dst)
	wait := moveRetryFirst
	deadline := time.Now().Add(moveRetryFor)
	for err != nil && transientMoveError(err) && time.Now().Before(deadline) {
		moveSleep(wait)
		wait = min(wait*2, moveRetryMaxWait)
		err = moveRename(src, dst)
	}
	if err == nil || !copyFallback || !transientMoveError(err) {
		return err
	}
	if cerr := copyTree(src, dst); cerr != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("%w（改用复制也没成：%v）", err, cerr)
	}
	_ = os.RemoveAll(src)
	return nil
}

var (
	moveRetryFor     = 30 * time.Second
	moveRetryFirst   = 250 * time.Millisecond
	moveRetryMaxWait = 4 * time.Second
	moveRename       = os.Rename
	moveSleep        = time.Sleep
)

const (
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// transientMoveError: what a file somebody else has open makes a rename fail with.
func transientMoveError(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) &&
		(errno == syscall.ERROR_ACCESS_DENIED || errno == errorSharingViolation || errno == errorLockViolation)
}

// copyTree copies the directory src to dst, which must not exist; files are read with full sharing.
func copyTree(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s 已经存在", dst)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type().IsRegular():
			return copyFileFn(path, to)
		default:
			return fmt.Errorf("%s 不是普通文件，不复制", path)
		}
	})
}

// copyFileFn is copyFile (a variable for the tests).
var copyFileFn = copyFile

func copyFile(src, dst string) error {
	in, err := root.OpenShared(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
