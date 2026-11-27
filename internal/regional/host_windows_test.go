//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package regional

import (
	"testing"
	"unsafe"
)

func TestWindowsRegionalReadOnly(t *testing.T) {
	if unsafe.Sizeof(dynamicTimeZone{}) != 432 {
		t.Fatal("DYNAMIC_TIME_ZONE_INFORMATION layout changed")
	}
	language, err := HostLanguage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Language(language); err != nil {
		t.Fatal(err)
	}
	zone, err := HostTimeZone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TimeZone(zone); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual Windows UI language %s, IANA time zone %s", language, zone)
}
