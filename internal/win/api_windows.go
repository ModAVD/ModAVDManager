//go:build windows

package win

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// HideConsoleCmd marks a child command with CREATE_NO_WINDOW.
func HideConsoleCmd(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: HideConsole}
	}
}

var procGetConsoleWindow = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")

func GetConsoleWindow() uintptr {
	if procGetConsoleWindow.Find() != nil {
		return 0
	}
	h, _, _ := procGetConsoleWindow.Call()
	return h
}

var procMessageBoxW = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

const (
	mbOK              = 0x00000000
	mbIconError       = 0x00000010
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000
	mbTopmost         = 0x00040000
)

// MessageBox shows msg in an error box (red cross).
func MessageBox(msg string) error { return messageBox(msg, mbIconError) }

// InfoBox shows msg in an information box (the "i" icon): for what was asked for, such as the usage.
func InfoBox(msg string) error { return messageBox(msg, mbIconInformation) }

func messageBox(msg string, icon uintptr) error {
	text, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return err
	}
	caption, err := syscall.UTF16PtrFromString("ModAVD 管理器")
	if err != nil {
		return err
	}
	if err := procMessageBoxW.Find(); err != nil {
		return err
	}
	if r, _, e := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), mbOK|icon|mbSetForeground|mbTopmost); r == 0 {
		return e
	}
	return nil
}
