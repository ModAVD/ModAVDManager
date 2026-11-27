//go:build !windows

package adb

import "unicode/utf8"

func ansiCodePage() uint32                             { return cpUTF8 }
func decodeCodePage(_ uint32, b []byte) (string, bool) { return string(b), utf8.Valid(b) }
