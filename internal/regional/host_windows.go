//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package regional

import (
	"fmt"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// HostLanguage reads the user's UI language, not the regional format or the keyboard layout.
func HostLanguage() (string, error) {
	id, _, _ := kernel32.NewProc("GetUserDefaultUILanguage").Call()
	if id == 0 {
		return "", fmt.Errorf("GetUserDefaultUILanguage 没有返回语言")
	}
	var name [85]uint16 // LOCALE_NAME_MAX_LENGTH
	n, _, err := kernel32.NewProc("LCIDToLocaleName").Call(id, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), 0)
	if n == 0 {
		return "", fmt.Errorf("LCIDToLocaleName: %w", err)
	}
	return Language(syscall.UTF16ToString(name[:]))
}

// The layout is DYNAMIC_TIME_ZONE_INFORMATION, including the stable TimeZoneKeyName.
// StandardName and DaylightName are localized and are deliberately unused.
type dynamicTimeZone struct {
	Bias                        int32
	StandardName                [32]uint16
	StandardDate                syscall.Systemtime
	StandardBias                int32
	DaylightName                [32]uint16
	DaylightDate                syscall.Systemtime
	DaylightBias                int32
	TimeZoneKeyName             [128]uint16
	DynamicDaylightTimeDisabled byte
}

func HostTimeZone() (string, error) {
	var info dynamicTimeZone
	r, _, err := kernel32.NewProc("GetDynamicTimeZoneInformation").Call(uintptr(unsafe.Pointer(&info)))
	if uint32(r) == 0xffffffff {
		return "", fmt.Errorf("GetDynamicTimeZoneInformation: %w", err)
	}
	return WindowsZone(syscall.UTF16ToString(info.TimeZoneKeyName[:]))
}
