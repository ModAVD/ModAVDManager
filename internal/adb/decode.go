//go:build windows

package adb

import (
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Code pages tried after UTF-8, in AUTO-MAS process_runner.ENCODINGS order: utf-8, utf-8-sig (the same result for
// strict input), locale.getpreferredencoding() (the ANSI code page on Windows), gbk, gb18030.
const (
	cpUTF8    = 65001
	cpGBK     = 936
	cpGB18030 = 54936
	// MB_ERR_INVALID_CHARS makes MultiByteToWideChar fail on invalid input instead of substituting, which is
	// Python's errors="strict".
	mbErrInvalidChars = 0x00000008
)

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

// decodeOutput is AUTO-MAS decode_bytes: the first encoding that decodes b strictly, else Latin-1. adb prints UTF-8
// itself; the fallbacks are for text Windows writes in the system language (error messages of the shell, a guest
// program printing GBK).
// DecodeOutput is decodeOutput for other packages' child processes (emu decodes `emulator -accel-check` with it).
func DecodeOutput(b []byte) string { return decodeOutput(b) }

func decodeOutput(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	tried := map[uint32]bool{0: true, cpUTF8: true}
	for _, cp := range []uint32{ansiCodePage(), cpGBK, cpGB18030} {
		if tried[cp] {
			continue
		}
		tried[cp] = true
		if s, ok := decodeCodePage(cp, b); ok {
			return s
		}
	}
	return latin1(b)
}

// decodeCodePage converts b from code page cp, failing on any invalid sequence.
func decodeCodePage(cp uint32, b []byte) (string, bool) {
	if len(b) == 0 {
		return "", true
	}
	if procMultiByteToWideChar.Find() != nil || len(b) > 1<<30 {
		return "", false
	}
	// the pointer conversions stay inside the Call expressions, which keeps b and buf alive and in place
	n, _, _ := procMultiByteToWideChar.Call(uintptr(cp), mbErrInvalidChars,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return "", false
	}
	buf := make([]uint16, n)
	n, _, _ = procMultiByteToWideChar.Call(uintptr(cp), mbErrInvalidChars,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&buf[0])), n)
	if n == 0 {
		return "", false
	}
	return string(utf16.Decode(buf[:n])), true
}

// latin1 maps every byte to the code point of the same value (Python's last resort, which never fails).
func latin1(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) * 2)
	for _, c := range b {
		s.WriteRune(rune(c))
	}
	return s.String()
}

// utf8Replace decodes UTF-8 with every invalid byte replaced by U+FFFD (Python errors="replace").
func utf8Replace(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string([]rune(s))
}
