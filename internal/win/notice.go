//go:build windows

package win

import (
	"fmt"
	"syscall"
	"unsafe"
)

// wtsapi32 by full System32 path, like crypt32 (dpapi.go): a wtsapi32.dll next to the exe must never be picked up.
var (
	modwtsapi32         = syscall.NewLazyDLL(systemDir() + `\wtsapi32.dll`)
	procWTSSendMessageW = modwtsapi32.NewProc("WTSSendMessageW")
)

// SessionNotice shows an information box with title and text in the caller's session and returns at once. The
// session's own system process shows it, not this one: nothing of ours stays mapped while it is up, and no child
// process is started. It closes by itself after seconds.
func SessionNotice(title, text string, seconds uint32) error {
	if err := procWTSSendMessageW.Find(); err != nil {
		return err
	}
	t, err := syscall.UTF16FromString(title)
	if err != nil {
		return err
	}
	m, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	const (
		currentSession    = 0xFFFFFFFF
		mbIconInformation = 0x40
	)
	var response uint32
	// lengths are in bytes, without the terminating NUL
	ok, _, callErr := procWTSSendMessageW.Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		currentSession,
		uintptr(unsafe.Pointer(&t[0])), uintptr((len(t)-1)*2),
		uintptr(unsafe.Pointer(&m[0])), uintptr((len(m)-1)*2),
		mbIconInformation, uintptr(seconds),
		uintptr(unsafe.Pointer(&response)),
		0, // do not wait for the answer
	)
	if ok == 0 {
		return fmt.Errorf("WTSSendMessage: %w", callErr)
	}
	return nil
}
