package emu

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procSHGetKnownFolderPath = syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree        = syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")
)

// folderIDPictures is FOLDERID_Pictures {33E28130-4E1E-4676-835A-98395C3BC3BB}.
var folderIDPictures = syscall.GUID{Data1: 0x33E28130, Data2: 0x4E1E, Data3: 0x4676,
	Data4: [8]byte{0x83, 0x5A, 0x98, 0x39, 0x5C, 0x3B, 0xC3, 0xBB}}

// hostPicturesDir is the known folder FOLDERID_Pictures of the current user: where Windows keeps it, also when the
// user moved it (to another drive, into OneDrive), which %USERPROFILE%\Pictures would miss. KF_FLAG_DEFAULT: an
// existing folder only, never created here.
func hostPicturesDir() (string, string) {
	if err := procSHGetKnownFolderPath.Find(); err != nil {
		return "", err.Error()
	}
	var p *uint16
	hr, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&folderIDPictures)), 0, 0,
		uintptr(unsafe.Pointer(&p)))
	if p != nil {
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	}
	if int32(hr) < 0 || p == nil {
		return "", fmt.Sprintf("SHGetKnownFolderPath(FOLDERID_Pictures) 返回 0x%08X", uint32(hr))
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(unsafe.Add(ptr, 2*n)) != 0; n++ {
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n)), ""
}
