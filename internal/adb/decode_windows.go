//go:build windows

package adb

import (
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const mbErrInvalidChars = 0x00000008

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetACP              = kernel32.NewProc("GetACP")
	procMultiByteToWideChar = kernel32.NewProc("MultiByteToWideChar")
)

func ansiCodePage() uint32 {
	if procGetACP.Find() != nil {
		return 0
	}
	cp, _, _ := procGetACP.Call()
	return uint32(cp)
}

// decodeCodePage converts b from code page cp, failing on any invalid sequence.
func decodeCodePage(cp uint32, b []byte) (string, bool) {
	if len(b) == 0 {
		return "", true
	}
	if procMultiByteToWideChar.Find() != nil || len(b) > 1<<30 {
		return "", false
	}
	n, _, _ := procMultiByteToWideChar.Call(uintptr(cp), mbErrInvalidChars, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return "", false
	}
	buf := make([]uint16, n)
	n, _, _ = procMultiByteToWideChar.Call(uintptr(cp), mbErrInvalidChars, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&buf[0])), n)
	if n == 0 {
		return "", false
	}
	return string(utf16.Decode(buf[:n])), true
}
