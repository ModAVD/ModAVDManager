//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"modavd/internal/logx"
)

var setRetainedFileDisposition = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// Windows Lstat FileInfo loads its file ID lazily by path. Obtain the candidate's identity and attributes from one
// non-following handle now; a later SameFile comparison must not resolve the candidate path again.
func snapshotLog(path string) (fs.FileInfo, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const readAttributes = 0x80
	const openReparsePoint = 0x00200000
	handle, err := syscall.CreateFile(name, readAttributes, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, openReparsePoint|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return file.Stat()
}

// Holding a directory without delete sharing prevents a junction/rename replacement while publishing a child.
// Read/write sharing remains enabled so normal creation and rotation inside it can continue.
func holdDiagnosticDirectory(path string, expected fs.FileInfo) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const readAttributes = 0x80
	const openReparsePoint = 0x00200000
	handle, err := syscall.CreateFile(name, readAttributes|1, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
		syscall.OPEN_EXISTING, openReparsePoint|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, info) {
		file.Close()
		return nil, fmt.Errorf("诊断目录身份变化，未发布")
	}
	return file, nil
}

// openRetained binds deletion to the selected object. No delete sharing keeps a directory's path fixed while its
// flat contents are visited; OPEN_REPARSE_POINT never follows a replacement junction or symlink.
func openRetained(candidate logFile) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(candidate.path)
	if err != nil {
		return nil, err
	}
	const deleteAccess = 0x10000
	const readAttributes = 0x80
	const openReparsePoint = 0x00200000
	flags := uint32(openReparsePoint)
	access := uint32(deleteAccess | readAttributes)
	if candidate.dir {
		flags |= syscall.FILE_FLAG_BACKUP_SEMANTICS
		access |= 1 // FILE_LIST_DIRECTORY, read entries through this same handle
	}
	handle, err := syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), candidate.path)
	info, err := file.Stat()
	if err != nil || !os.SameFile(candidate.info, info) || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != candidate.dir {
		file.Close()
		return nil, fmt.Errorf("日志身份已变化，未删除 %s", candidate.name)
	}
	if !candidate.dir && (!info.Mode().IsRegular() || info.Size() != candidate.size || !info.ModTime().Equal(candidate.mtime)) {
		file.Close()
		return nil, fmt.Errorf("日志内容已变化，未删除 %s", candidate.name)
	}
	return file, nil
}

func disposeRetained(file *os.File) error {
	info := struct{ DeleteFile byte }{DeleteFile: 1}
	result, _, err := setRetainedFileDisposition.Call(file.Fd(), 4, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if result == 0 {
		if err == syscall.Errno(0) {
			return syscall.EINVAL
		}
		return err
	}
	return nil
}

func unlinkRetained(candidate logFile, log *logx.Logger) unlinkResult {
	file, err := openRetained(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return unlinkGone
	}
	if err != nil {
		log.Debugf("旧日志 %s 不能安全删除: %v", candidate.name, err)
		return unlinkFailed
	}
	defer file.Close()
	if candidate.dir {
		// Failed-boot snapshots are flat and immutable once published. Unexpected subdirectories or links are
		// preserved and reported rather than being traversed by a recursive path-based delete.
		entries, err := file.ReadDir(-1)
		if err != nil {
			log.Debugf("旧现场目录 %s 读不出，未删除: %v", candidate.name, err)
			return unlinkFailed
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				return unlinkFailed
			}
			info, err := snapshotLog(filepath.Join(candidate.path, entry.Name()))
			if err != nil {
				return unlinkFailed
			}
			child := logFile{path: filepath.Join(candidate.path, entry.Name()), name: entry.Name(), size: info.Size(),
				mtime: info.ModTime(), info: info}
			if result := unlinkRetained(child, log); result == unlinkFailed {
				return result
			}
		}
	}
	if err := disposeRetained(file); err != nil {
		log.Debugf("旧日志 %s 删除失败: %v", candidate.name, err)
		return unlinkFailed
	}
	return unlinkRemoved
}
