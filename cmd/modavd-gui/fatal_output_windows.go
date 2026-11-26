//go:build windows

package main

import (
	"os"
	"syscall"
)

const (
	fileTypeDisk = 0x0001
	fileTypeChar = 0x0002
	fileTypePipe = 0x0003
	stdOutput    = uintptr(^uint32(10)) // (DWORD)-11, STD_OUTPUT_HANDLE
	stdError     = uintptr(^uint32(11)) // (DWORD)-12, STD_ERROR_HANDLE
)

type fatalRoute uint8

const (
	routeDialog fatalRoute = iota
	routeRedirect
	routeConsole
)

var (
	procGetFileType   = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileType")
	procAttachConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("AttachConsole")
	procGetStdHandle  = syscall.NewLazyDLL("kernel32.dll").NewProc("GetStdHandle")
)

func routeFatal(fileType uint32, parentConsole bool) fatalRoute {
	if fileType == fileTypeDisk || fileType == fileTypePipe {
		return routeRedirect
	}
	if parentConsole {
		return routeConsole
	}
	return routeDialog
}

func redirectedFileType(file *os.File) uint32 {
	if file == nil {
		return 0
	}
	r, _, _ := procGetFileType.Call(file.Fd())
	return uint32(r)
}

func writeFatal(msg string, stderr bool) bool {
	stream := os.Stdout
	if stderr {
		stream = os.Stderr
	}
	if routeFatal(redirectedFileType(stream), false) == routeRedirect {
		_, err := stream.WriteString(msg + "\r\n")
		return err == nil
	}

	// A GUI-subsystem process starts without a console. AttachConsole only succeeds when the parent has one.
	attached, _, _ := procAttachConsole.Call(^uintptr(0))
	if attached == 0 {
		return false
	}
	route := routeFatal(0, true)
	if route != routeConsole {
		return false
	}
	which := stdOutput
	if stderr {
		which = stdError
	}
	h, _, _ := procGetStdHandle.Call(which)
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	data := append([]byte(msg), '\r', '\n')
	var written uint32
	err := syscall.WriteFile(syscall.Handle(h), data, &written, nil)
	return err == nil && written == uint32(len(data))
}
