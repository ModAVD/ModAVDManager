// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package mumu

import (
	"syscall"
	"unsafe"
)

func installDirs() []string {
	var dirs []string
	for _, view := range []uint32{syscall.KEY_WOW64_64KEY, syscall.KEY_WOW64_32KEY} {
		key, _ := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\MuMuPlayer`)
		var h syscall.Handle
		if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, key, 0, syscall.KEY_QUERY_VALUE|view, &h) != nil {
			continue
		}
		name, _ := syscall.UTF16PtrFromString("InstallLocation")
		buf := make([]uint16, 32768)
		n := uint32(2 * len(buf))
		var typ uint32
		err := syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n)
		syscall.RegCloseKey(h)
		if err == nil && typ == syscall.REG_SZ {
			dirs = append(dirs, syscall.UTF16ToString(buf))
		}
	}
	return dirs
}
