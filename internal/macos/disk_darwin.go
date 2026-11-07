//go:build darwin && arm64 && cgo

package macos

/*
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <errno.h>
static int daily_openat(int dirfd, const char *name, int flags) {
    return openat(dirfd, name, flags | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
}
static int daily_renameat(int dirfd, const char *src, const char *dst) {
    return renameat(dirfd, src, dirfd, dst) == 0 ? 0 : errno;
}
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func dailyOpenAt(dir *os.File, name string, flags int) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return nil, syscall.EINVAL
	}
	path := C.CString(name)
	defer C.free(unsafe.Pointer(path))
	fd, err := C.daily_openat(C.int(dir.Fd()), path, C.int(flags))
	if fd < 0 {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

func dailyOpenDirectory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// AllocatedFileBytes counts host filesystem blocks, excluding sparse-file holes.
// APFS shared extents can remain allocated after replacement; this is a snapshot
// of the file's allocation, not a promise of a volume-wide free-space increase.
func AllocatedFileBytes(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks < 0 || uint64(stat.Blocks) > math.MaxUint64/512 {
		return 0, fmt.Errorf("文件分配块数无效: %s", path)
	}
	return uint64(stat.Blocks) * 512, nil
}

func VolumeFileSystem(path string) (string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", err
	}
	name := make([]byte, 0, len(stat.Fstypename))
	for _, b := range stat.Fstypename {
		if b == 0 {
			break
		}
		name = append(name, byte(b))
	}
	return string(name), nil
}

func plainSingleFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}

// CheckCompactFile refuses symlinks and hardlinks before any rewrite occurs.
func CheckCompactFile(path string, expected os.FileInfo) error {
	dir, err := dailyOpenDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := dailyOpenAt(dir, filepath.Base(path), syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !plainSingleFile(info) || !os.SameFile(expected, info) {
		return fmt.Errorf("数据盘必须是独立普通文件，不能是符号链接、硬链接或已替换的文件: %s", path)
	}
	return nil
}

// ReplaceFileDurable flushes the verified copy, then replaces through one held
// directory descriptor. APFS rename is atomic; no remove-then-rename gap exists.
func ReplaceFileDurable(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) || filepath.Clean(filepath.Dir(src)) != filepath.Clean(filepath.Dir(dst)) {
		return fmt.Errorf("磁盘替换必须在同一目录内")
	}
	dir, err := dailyOpenDirectory(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := dailyOpenAt(dir, filepath.Base(src), syscall.O_RDWR)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !plainSingleFile(info) {
		return fmt.Errorf("整理副本不是独立普通文件: %s", src)
	}
	old, err := dailyOpenAt(dir, filepath.Base(dst), syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer old.Close()
	oldInfo, err := old.Stat()
	if err != nil || !plainSingleFile(oldInfo) {
		return fmt.Errorf("原数据盘不是独立普通文件: %s", dst)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0); errno != 0 {
		return fmt.Errorf("刷新整理副本失败: %w", errno)
	}
	a, b := C.CString(filepath.Base(src)), C.CString(filepath.Base(dst))
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(b))
	if code := C.daily_renameat(C.int(dir.Fd()), a, b); code != 0 {
		return syscall.Errno(code)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("数据盘已原子替换，但目录刷新失败: %w", err)
	}
	return nil
}

// RunBound owns a new process group so cancellation also stops tool children.
func RunBound(ctx context.Context, exe string, args ...string) ([]byte, []byte, *os.ProcessState, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), cmd.ProcessState, err
}
