//go:build !windows

// Copyright 2026 jinghero. All rights reserved.

package regional

import "fmt"

func HostLanguage() (string, error) { return "", fmt.Errorf("宿主语言设置只支持 Windows") }
func HostTimeZone() (string, error) { return "", fmt.Errorf("宿主时区设置只支持 Windows") }
