// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func regString(hive syscall.Handle, key, value string, view uint32) string {
	sub, _ := syscall.UTF16PtrFromString(key)
	name, _ := syscall.UTF16PtrFromString(value)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(hive, sub, 0, syscall.KEY_QUERY_VALUE|view, &h) != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil || typ != syscall.REG_SZ {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
func mumuDirs() []string {
	var dirs []string
	for _, view := range []uint32{syscall.KEY_WOW64_64KEY, syscall.KEY_WOW64_32KEY} {
		dirs = append(dirs, regString(syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\MuMuPlayer`, "InstallLocation", view))
	}
	return dirs
}
func ldInstallations() []installation {
	var result []installation
	for _, version := range []string{"14", "9"} {
		key := `Software\leidian\LDPlayer` + version
		dir := regString(syscall.HKEY_CURRENT_USER, key, "InstallDir", 0)
		data := regString(syscall.HKEY_CURRENT_USER, key, "DataDir", 0)
		for _, base := range []string{dir, filepath.Join(os.Getenv("SystemDrive")+`\`, "leidian", "LDPlayer"+version)} {
			if !filepath.IsAbs(base) {
				continue
			}
			vms := filepath.Join(base, "vms")
			if base == dir && filepath.IsAbs(data) {
				vms = data
			}
			result = append(result, installation{kind: "ldplayer", exe: filepath.Join(base, "ldconsole.exe"), data: vms})
		}
	}
	return result
}
