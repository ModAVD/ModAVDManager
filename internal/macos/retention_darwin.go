//go:build darwin && arm64 && cgo

package macos

/*
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <unistd.h>
static int daily_mkdir(int parent, const char *name) {
    return mkdirat(parent, name, 0700) == 0 ? 0 : errno;
}
static int daily_move(int from, const char *src, int to, const char *dst) {
    return renameatx_np(from, src, to, dst, RENAME_EXCL) == 0 ? 0 : errno;
}
static int daily_unlink(int parent, const char *name, int directory) {
    return unlinkat(parent, name, directory ? AT_REMOVEDIR : 0) == 0 ? 0 : errno;
}
*/
import "C"

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func retentionName(fn func(*C.char) C.int, name string) error {
	p := C.CString(name)
	defer C.free(unsafe.Pointer(p))
	if code := fn(p); code != 0 {
		return syscall.Errno(code)
	}
	return nil
}

func retentionMove(from *os.File, src string, to *os.File, dst string) error {
	a, b := C.CString(src), C.CString(dst)
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(b))
	if code := C.daily_move(C.int(from.Fd()), a, C.int(to.Fd()), b); code != 0 {
		return syscall.Errno(code)
	}
	return nil
}

func retentionUnlink(dir *os.File, name string, directory bool) error {
	flag := C.int(0)
	if directory {
		flag = 1
	}
	return retentionName(func(p *C.char) C.int { return C.daily_unlink(C.int(dir.Fd()), p, flag) }, name)
}

func unchangedRetained(expected, actual os.FileInfo, directory bool) bool {
	if !os.SameFile(expected, actual) || expected.IsDir() != directory || actual.IsDir() != directory ||
		actual.Mode()&os.ModeSymlink != 0 || !expected.ModTime().Equal(actual.ModTime()) {
		return false
	}
	return directory || (plainSingleFile(actual) && expected.Size() == actual.Size())
}

// UnlinkRetained moves a selected object into a fresh private sibling directory,
// checks its identity again, then removes it relative to held directory FDs.
// Replaced entries and unexpected directory contents are preserved, never followed.
func UnlinkRetained(path string, expected os.FileInfo, directory bool) (result error) {
	if busy, gone := FileInUse(path); gone {
		return os.ErrNotExist
	} else if busy {
		return fmt.Errorf("日志仍被打开，未删除: %s", path)
	}
	parent, err := dailyOpenDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	flags := syscall.O_RDONLY
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	f, err := dailyOpenAt(parent, filepath.Base(path), flags)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !unchangedRetained(expected, info, directory) {
		return fmt.Errorf("日志身份或内容已变化，未删除: %s", path)
	}
	if directory {
		entries, err := f.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				return fmt.Errorf("日志目录含子目录或链接，未删除: %s", path)
			}
			if busy, gone := FileInUse(filepath.Join(path, entry.Name())); busy || gone {
				return fmt.Errorf("日志目录仍在使用或内容变化，未删除: %s", path)
			}
		}
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	trashName := ".modavd-retention-" + hex.EncodeToString(random[:])
	if err := retentionName(func(p *C.char) C.int { return C.daily_mkdir(C.int(parent.Fd()), p) }, trashName); err != nil {
		return err
	}
	trash, err := dailyOpenAt(parent, trashName, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		_ = retentionUnlink(parent, trashName, true)
		return err
	}
	defer trash.Close()
	defer retentionUnlink(parent, trashName, true) // only removes an empty directory
	if err := retentionMove(parent, filepath.Base(path), trash, "item"); err != nil {
		return err
	}
	restore := true
	defer func() {
		if restore {
			// Never overwrite an entry another writer created meanwhile. If it
			// exists, the moved object remains intact in the private directory.
			if err := retentionMove(trash, "item", parent, filepath.Base(path)); err != nil {
				result = errors.Join(result, fmt.Errorf("日志保留在 %s: %w", filepath.Join(trash.Name(), "item"), err))
			}
		}
	}()
	moved, err := dailyOpenAt(trash, "item", flags)
	if err != nil {
		return err
	}
	defer moved.Close()
	actual, err := moved.Stat()
	if err != nil || !unchangedRetained(expected, actual, directory) {
		return fmt.Errorf("日志移动时身份或内容变化，已保留: %s", path)
	}
	if !directory {
		// The original name is no longer available to new writers. Release our
		// own handles before checking for writers that opened it before the move.
		f.Close()
		moved.Close()
		if busy, gone := fileInUse(filepath.Join(trash.Name(), "item"), path); busy || gone {
			return fmt.Errorf("日志移动后仍被打开或已变化，已保留: %s", path)
		}
	}
	if directory {
		entries, err := moved.ReadDir(-1)
		if err != nil {
			return err
		}
		// Inspect the whole flat snapshot before deleting any child.
		selected := make(map[string]os.FileInfo, len(entries))
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				return fmt.Errorf("日志目录含子目录或链接，已保留: %s", path)
			}
			child, err := dailyOpenAt(moved, entry.Name(), syscall.O_RDONLY)
			if err != nil {
				return err
			}
			childInfo, statErr := child.Stat()
			child.Close()
			if statErr != nil || !plainSingleFile(childInfo) {
				return fmt.Errorf("日志目录含非独立普通文件，已保留: %s", path)
			}
			selected[entry.Name()] = childInfo
		}
		for _, entry := range entries {
			if err := unlinkRetainedChild(moved, entry.Name(), selected[entry.Name()], trash, filepath.Join(path, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := retentionUnlink(trash, "item", directory); err != nil {
		return err
	}
	restore = false
	return nil
}

func unlinkRetainedChild(parent *os.File, name string, selected os.FileInfo, trash *os.File, writerPath string) (result error) {
	if err := retentionMove(parent, name, trash, "child"); err != nil {
		return err
	}
	restore := true
	defer func() {
		if restore {
			if err := retentionMove(trash, "child", parent, name); err != nil {
				result = errors.Join(result, fmt.Errorf("日志子文件保留在 %s: %w", filepath.Join(trash.Name(), "child"), err))
			}
		}
	}()
	child, err := dailyOpenAt(trash, "child", syscall.O_RDONLY)
	if err != nil {
		return err
	}
	info, err := child.Stat()
	child.Close()
	if err != nil || !unchangedRetained(selected, info, false) {
		return fmt.Errorf("日志子文件身份或内容变化，未删除: %s", name)
	}
	if busy, gone := fileInUse(filepath.Join(trash.Name(), "child"), writerPath); busy || gone {
		return fmt.Errorf("日志子文件仍被打开或已变化，未删除: %s", name)
	}
	if err := retentionUnlink(trash, "child", false); err != nil {
		return err
	}
	restore = false
	return nil
}
