package adb

import (
	"strings"
	"unicode/utf8"
)

// Code pages tried after UTF-8, in AUTO-MAS process_runner.ENCODINGS order: utf-8, utf-8-sig (the same result for
// strict input), locale.getpreferredencoding() (the ANSI code page on Windows), gbk, gb18030.
const (
	cpUTF8    = 65001
	cpGBK     = 936
	cpGB18030 = 54936
)

func DecodeOutput(b []byte) string { return decodeOutput(b) }

// decodeOutput is AUTO-MAS decode_bytes: the first encoding that decodes b strictly, else Latin-1. adb prints UTF-8
// itself; the fallbacks are for text Windows writes in the system language (error messages of the shell, a guest
// program printing GBK).
func decodeOutput(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	tried := map[uint32]bool{0: true, cpUTF8: true}
	for _, cp := range []uint32{ansiCodePage(), cpGBK, cpGB18030} {
		if tried[cp] {
			continue
		}
		tried[cp] = true
		if s, ok := decodeCodePage(cp, b); ok {
			return s
		}
	}
	return latin1(b)
}

// latin1 maps every byte to the code point of the same value (Python's last resort, which never fails).
func latin1(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) * 2)
	for _, c := range b {
		s.WriteRune(rune(c))
	}
	return s.String()
}

// utf8Replace decodes UTF-8 with every invalid byte replaced by U+FFFD (Python errors="replace").
func utf8Replace(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string([]rune(s))
}
