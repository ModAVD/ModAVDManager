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

// reportFatal writes to a redirected stream or the parent's console when possible. It falls back to the dialog
// used by the desktop launch path.
func reportFatal(msg string, stderr bool) {
	if writeFatal(msg, stderr) {
		return
	}
	showFatalDialog(msg)
}

func fatal(msg string) { reportFatal(msg, true) }

// showFatalDialog shows a startup error when there is no stream and no parent console.
func showFatalDialog(msg string) {
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
