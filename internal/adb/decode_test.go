//go:build windows

package adb

import "testing"

var gbkZhongWen = []byte{0xD6, 0xD0, 0xCE, 0xC4} // "中文" in GBK

func TestDecodeCodePage(t *testing.T) {
	if s, ok := decodeCodePage(cpGBK, gbkZhongWen); !ok || s != "中文" {
		t.Errorf("GBK = %q, %v", s, ok)
	}
	if _, ok := decodeCodePage(cpGBK, []byte{0xD6}); ok { // a lead byte without its trail byte
		t.Errorf("truncated GBK decoded")
	}
	if s, ok := decodeCodePage(cpGB18030, []byte{0x81, 0x30, 0x81, 0x30}); !ok || s != "\u0080" {
		t.Errorf("GB18030 four-byte = %q, %v", s, ok)
	}
	if s, ok := decodeCodePage(cpGBK, nil); !ok || s != "" {
		t.Errorf("empty = %q, %v", s, ok)
	}
}

func TestDecodeOutput(t *testing.T) {
	if s := decodeOutput([]byte("List of devices attached\r\n中文\n")); s != "List of devices attached\r\n中文\n" {
		t.Errorf("UTF-8 = %q", s)
	}
	if s := decodeOutput(nil); s != "" {
		t.Errorf("nil = %q", s)
	}
	// After UTF-8 the ANSI code page comes first, as in AUTO-MAS (locale.getpreferredencoding).
	want := "中文"
	if cp := ansiCodePage(); cp != 0 && cp != cpUTF8 {
		if s, ok := decodeCodePage(cp, gbkZhongWen); ok {
			want = s
		}
	}
	if s := decodeOutput(gbkZhongWen); s != want {
		t.Errorf("GBK output (ACP %d) = %q, want %q", ansiCodePage(), s, want)
	}
}

func TestLatin1AndReplace(t *testing.T) {
	if s := latin1([]byte{0xFF, 'A', 0x80}); s != "ÿA\u0080" {
		t.Errorf("latin1 = %q", s)
	}
	if s := utf8Replace("a\xffb\xfe"); s != "a�b�" {
		t.Errorf("utf8Replace = %q", s)
	}
	if s := utf8Replace("中文"); s != "中文" {
		t.Errorf("utf8Replace valid = %q", s)
	}
}
