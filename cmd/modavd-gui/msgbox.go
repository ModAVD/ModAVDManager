//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// user32 is a KnownDLL: Windows always maps it from System32.
var procMessageBoxW = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

const (
	mbOK            = 0x0
	mbIconError     = 0x10
	mbSetForeground = 0x10000
	mbTopmost       = 0x40000
)

// fatal shows a startup error: a GUI-subsystem process has no console to print it to.
func fatal(msg string) {
	text, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	caption, _ := syscall.UTF16PtrFromString("ModAVD 管理器")
	if procMessageBoxW.Find() != nil {
		return
	}
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)),
		mbOK|mbIconError|mbSetForeground|mbTopmost)
}
