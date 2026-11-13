//go:build windows

package update

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	fileAttributeReparsePoint = 0x400
	errorSharingViolation     = syscall.Errno(32)
	errorLockViolation        = syscall.Errno(33)
	deleteAccess              = 0x00010000
)

// Probe writes (loaded images) and deletes (handles without FILE_SHARE_DELETE).
// Other errors remain the swap's to report; no attributes or contents are changed.
func fileInUse(path string) bool {
	f, writeErr := os.OpenFile(path, os.O_WRONLY, 0)
	if writeErr == nil {
		f.Close()
	}
	if errors.Is(writeErr, errorSharingViolation) || errors.Is(writeErr, errorLockViolation) {
		return true
	}
	ptr, err := syscall.UTF16PtrFromString(path)
	if err == nil {
		h, err := syscall.CreateFile(ptr, deleteAccess, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil, syscall.OPEN_EXISTING, 0, 0)
		if err == nil {
			syscall.CloseHandle(h)
		} else if errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) {
			return true
		}
	}
	// Read-only masks a loaded image's write sharing violation, while DELETE
	// alone still succeeds. Confirm an actual loaded image: an ordinary reader
	// with delete sharing must not prevent removal of an obsolete read-only file.
	if errors.Is(writeErr, syscall.ERROR_ACCESS_DENIED) {
		st, err := os.Stat(path)
		return err == nil && !st.IsDir() && st.Mode().Perm()&0o200 == 0 && loadedImageInUse(path)
	}
	return false
}

var (
	occupancyKernel32    = syscall.NewLazyDLL("kernel32.dll")
	enumProcessModulesEx = occupancyKernel32.NewProc("K32EnumProcessModulesEx")
	getMappedFileNameW   = occupancyKernel32.NewProc("K32GetMappedFileNameW")
	getModuleFileNameExW = occupancyKernel32.NewProc("K32GetModuleFileNameExW")
	getFinalPathNameW    = occupancyKernel32.NewProc("GetFinalPathNameByHandleW")
)

// loadedImageInUse is best effort, like the write/delete probes. A protected or
// exiting process that cannot be inspected leaves the existing swap/recovery path.
func loadedImageInUse(path string) bool {
	target := finalImagePath(path)
	if target == "" {
		return false
	}
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(snapshot)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err := syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		if processHasImage(entry.ProcessID, target) {
			return true
		}
	}
	return false
}

func finalImagePath(path string) string {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	h, err := syscall.CreateFile(ptr, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	var name [32768]uint16
	const volumeNameNT = 2
	n, _, _ := getFinalPathNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), volumeNameNT)
	syscall.CloseHandle(h)
	if n == 0 || n >= uintptr(len(name)) {
		return ""
	}
	// Both APIs return the resolved NT device path. This also covers directory
	// links and avoids resolving every module's DOS path through the filesystem.
	return syscall.UTF16ToString(name[:n])
}

func processHasImage(pid uint32, target string) bool {
	const processQueryInformation = 0x0400
	const processVMRead = 0x0010
	process, err := syscall.OpenProcess(processQueryInformation|processVMRead, false, pid)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(process)
	modules := make([]syscall.Handle, 128)
	var needed uint32
	const listModulesAll = 3
	const handleSize = uint32(unsafe.Sizeof(syscall.Handle(0)))
	// The module list can grow between calls. Retry with the reported size,
	// then leave an unstable process to the swap's normal error handling.
	for attempt := 0; attempt < 3; attempt++ {
		size := uint32(len(modules)) * handleSize
		ok, _, _ := enumProcessModulesEx.Call(uintptr(process), uintptr(unsafe.Pointer(&modules[0])), uintptr(size),
			uintptr(unsafe.Pointer(&needed)), listModulesAll)
		if ok == 0 {
			return false
		}
		if needed > size {
			modules = make([]syscall.Handle, (needed+handleSize-1)/handleSize)
			continue
		}
		// Toolhelp's MODULEENTRY32 path is limited to MAX_PATH. The mapped name
		// accepts full paths and resolves links to the actual image on disk.
		var name [32768]uint16
		for _, module := range modules[:needed/handleSize] {
			n, _, err := getMappedFileNameW.Call(uintptr(process), uintptr(module), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
			// GetMappedFileName can report MORE_DATA for deep paths even with
			// this full-sized buffer. Read the loader's path and resolve it then.
			if n == 0 && errors.Is(err, syscall.ERROR_MORE_DATA) {
				n, _, _ = getModuleFileNameExW.Call(uintptr(process), uintptr(module), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
				if n > 0 && n < uintptr(len(name)) && strings.EqualFold(finalImagePath(syscall.UTF16ToString(name[:n])), target) {
					return true
				}
				continue
			}
			if n == 0 || n >= uintptr(len(name)) {
				continue
			}
			image := syscall.UTF16ToString(name[:n])
			if strings.EqualFold(image, target) {
				return true
			}
		}
		return false
	}
	return false
}

func isReparsePoint(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && sys.FileAttributes&fileAttributeReparsePoint != 0
}
func removeLink(p string) bool {
	if os.Remove(p) == nil {
		return true
	}
	ptr, _ := syscall.UTF16PtrFromString(p)
	return syscall.RemoveDirectory(ptr) == nil
}
