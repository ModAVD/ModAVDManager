package i18n

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
	procLCIDToLocaleName         = kernel32.NewProc("LCIDToLocaleName")
)

// systemLocale is the Windows display language of the user (GetUserDefaultUILanguage), as a locale name.
func systemLocale() string {
	if procGetUserDefaultUILanguage.Find() != nil {
		return ""
	}
	r, _, _ := procGetUserDefaultUILanguage.Call()
	langID := uint32(r) & 0xffff
	if langID == 0 {
		return ""
	}
	if procLCIDToLocaleName.Find() == nil {
		buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
		n, _, _ := procLCIDToLocaleName.Call(uintptr(langID), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n > 1 {
			return syscall.UTF16ToString(buf[:n-1])
		}
	}
	// LANG_CHINESE is primary language 0x04.
	if langID&0x3ff == 0x04 {
		return "zh-CN"
	}
	return "en-US"
}
