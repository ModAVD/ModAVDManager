//go:build windows

package update

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"modavd/internal/errs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	imageKernel32 = syscall.NewLazyDLL("kernel32.dll")
	imageInfoEx   = imageKernel32.NewProc("GetFileInformationByHandleEx")
	imageOpenByID = imageKernel32.NewProc("OpenFileById")
)

// FILE_ID_INFO and FILE_ID_DESCRIPTOR, Windows 8+. The full 128-bit ID also
// supports ReFS and deduplicates SDK paths that are hardlinks to the same image.
type imageID struct {
	Volume uint64
	ID     [16]byte
}
type imageDescriptor struct {
	Size uint32
	Kind uint32
	ID   [16]byte
}

func imageFileID(handle syscall.Handle) (imageID, error) {
	var id imageID
	ok, _, err := imageInfoEx.Call(uintptr(handle), 18, uintptr(unsafe.Pointer(&id)), unsafe.Sizeof(id)) // FileIdInfo
	if ok == 0 {
		return imageID{}, err
	}
	if id.ID == [16]byte{} {
		return imageID{}, fmt.Errorf("无法确认程序文件身份")
	}
	return id, nil
}

// Standard CreateProcess opens its image without sharing write access. A writable
// handle excludes raw starts and callers sharing the SDK, without changing files
// or ACLs. Opening by file ID is essential: a handle opened by an SDK pathname
// would itself prevent directory renames, even with FILE_SHARE_DELETE.
func (u *Updater) lockSDKImages() (release func(), err error) {
	var handles []syscall.Handle
	closeAll := func() {
		for _, h := range handles {
			syscall.CloseHandle(h)
		}
		handles = nil
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	seen := map[imageID]bool{}
	for _, dir := range []string{u.l.emulator(), u.l.stageEmulator(), u.l.previousEmulator()} {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, swapBlocked("无法检查模拟器目录 %s（%v），什么都没动", dir, err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, swapBlocked("无法确认模拟器目录 %s（%v），什么都没动", dir, err)
		}
		err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
			if !ok {
				return fmt.Errorf("无法确认模拟器文件属性: %s", path)
			}
			if attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("模拟器目录内含重解析点，无法安全锁定: %s", path)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			// Include DLLs and renamed PE files too. Close this pathname handle before
			// any directory rename, retaining only the handle opened by file identity.
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			var magic [2]byte
			if _, err := io.ReadFull(f, magic[:]); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return nil
				}
				return err
			}
			if magic != [2]byte{'M', 'Z'} {
				return nil
			}
			id, err := imageFileID(syscall.Handle(f.Fd()))
			if err != nil {
				return errs.Newf(errs.NotReady, "文件系统不支持安全更新所需的程序身份查询: %s（%v）", path, err)
			}
			if seen[id] {
				return nil
			}
			desc := imageDescriptor{Kind: 2, ID: id.ID} // ExtendedFileIdType
			desc.Size = uint32(unsafe.Sizeof(desc))
			h, _, openErr := imageOpenByID.Call(f.Fd(), uintptr(unsafe.Pointer(&desc)), syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE, 0, 0)
			if syscall.Handle(h) == syscall.InvalidHandle {
				if errors.Is(openErr, syscall.Errno(1)) || errors.Is(openErr, syscall.Errno(50)) || errors.Is(openErr, syscall.Errno(87)) {
					return errs.Newf(errs.NotReady, "文件系统不支持按身份锁定模拟器程序，无法安全更新: %s（%v）", path, openErr)
				}
				return fmt.Errorf("无法锁定程序 %s: %w", path, openErr)
			}
			handles = append(handles, syscall.Handle(h))
			lockedID, err := imageFileID(syscall.Handle(h))
			if err != nil || lockedID != id {
				return fmt.Errorf("程序文件身份不一致: %s (%v)", path, err)
			}
			seen[id] = true
			return nil
		})
		if err != nil {
			if errs.CodeOf(err) == errs.NotReady {
				return nil, err
			}
			return nil, swapBlocked("无法安全独占模拟器程序（%v），什么都没动，请关掉占用程序并检查文件权限", err)
		}
	}
	return closeAll, nil
}
