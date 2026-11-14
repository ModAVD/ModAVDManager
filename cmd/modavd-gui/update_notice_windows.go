// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"modavd/internal/update"
)

const (
	wtsCurrentSession = 0xFFFFFFFF
	noticeSeconds     = 15
	mbIconInformation = 0x40
)

// A message box in this process would keep ModAVD.exe mapped until the user
// dismisses it and prevent replacement. WTSSendMessage has the session's own
// system process show the fixed notice (no child process, nothing of ours left
// mapped) and returns at once; this manager then exits before the CLI's final
// scan and image gate. The box closes by itself after noticeSeconds.
var updatingNotice = func() error {
	title, err := syscall.UTF16FromString("ModAVD")
	if err != nil {
		return err
	}
	text, err := syscall.UTF16FromString(update.UpdatingMessage)
	if err != nil {
		return err
	}
	var response uint32
	// lengths are in bytes, without the terminating NUL
	ok, _, callErr := syscall.NewLazyDLL("wtsapi32.dll").NewProc("WTSSendMessageW").Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		wtsCurrentSession,
		uintptr(unsafe.Pointer(&title[0])), uintptr((len(title)-1)*2),
		uintptr(unsafe.Pointer(&text[0])), uintptr((len(text)-1)*2),
		mbIconInformation, noticeSeconds,
		uintptr(unsafe.Pointer(&response)),
		0, // do not wait for the answer
	)
	if ok == 0 {
		return fmt.Errorf("WTSSendMessage: %w", callErr)
	}
	return nil
}
