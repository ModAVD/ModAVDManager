package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// textRoute is where writeText puts a text.
type textRoute uint8

const (
	routeBox     textRoute = iota // no stream and no console: the caller shows a box
	routeStream                   // stdout / stderr is redirected: a file, a pipe or a device such as NUL
	routeConsole                  // not redirected, but started from a terminal: the parent's console
)

// kernel32 is a KnownDLL: Windows always maps it from System32.
var procAttachConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("AttachConsole")

const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS, (DWORD)-1

// routeText picks the route from the file type of the stream (syscall.FILE_TYPE_*, 0 when there is no stream),
// whether that stream is a console, and whether the parent's console could be attached.
func routeText(fileType uint32, streamIsConsole, parentConsole bool) textRoute {
	switch fileType {
	case syscall.FILE_TYPE_DISK, syscall.FILE_TYPE_PIPE:
		return routeStream
	case syscall.FILE_TYPE_CHAR:
		if !streamIsConsole {
			return routeStream
		}
	}
	if parentConsole {
		return routeConsole
	}
	return routeBox
}

// writeText writes msg to the redirected stderr (isErr) or stdout, or else to the console of the parent process.
// False when neither is there (a double click, a shortcut) or the write failed: the caller shows a box then. A
// GUI-subsystem process starts without a console, and with no standard handles unless they were redirected.
func writeText(msg string, isErr bool) bool {
	text := strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n"
	stream := os.Stdout
	if isErr {
		stream = os.Stderr
	}
	fileType, isConsole := streamType(stream)
	if routeText(fileType, isConsole, false) == routeStream {
		_, err := stream.WriteString(text)
		return err == nil
	}
	if routeText(fileType, isConsole, attachParentConsole()) != routeConsole {
		return false
	}
	// CONOUT$ rather than the standard handles, which stay empty after AttachConsole; os.File writes a console in
	// UTF-16, whatever its code page.
	con, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer con.Close()
	// the shell has printed its prompt already (it does not wait for a GUI program): start on a new line
	_, err = con.WriteString("\r\n" + text)
	return err == nil
}

// streamType is the file type of f's handle (0 when there is none) and whether it is a console.
func streamType(f *os.File) (fileType uint32, isConsole bool) {
	if f == nil {
		return 0, false
	}
	h := syscall.Handle(f.Fd())
	if h == 0 || h == syscall.InvalidHandle {
		return 0, false
	}
	t, err := syscall.GetFileType(h)
	if err != nil {
		return 0, false
	}
	var mode uint32
	return t, syscall.GetConsoleMode(h, &mode) == nil
}

// attachParentConsole attaches this process to its parent's console; true when attached, including by an earlier
// call.
func attachParentConsole() bool {
	if err := procAttachConsole.Find(); err != nil {
		return false
	}
	ok, _, err := procAttachConsole.Call(attachParentProcess)
	return ok != 0 || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
