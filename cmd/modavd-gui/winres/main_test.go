package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"modavd/internal/buildinfo"
)

// The committed .syso files are what winres writes now: a buildinfo.Version bump (or a new icon or manifest) without
// running it again would ship exes whose Properties show the old version.
func TestSysoUpToDate(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	files, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("build wrote %d files, want 2", len(files))
	}
	for dir, want := range files {
		got, err := os.ReadFile(filepath.Join(root, dir, sysoName))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s/%s is out of date (buildinfo.Version %s): run go generate ./cmd/modavd-gui and commit it",
				dir, sysoName, buildinfo.Version)
		}
	}
}

func TestVersionParts(t *testing.T) {
	for v, want := range map[string][4]uint16{"0.6.0": {0, 6, 0, 0}, "1.2.3.4": {1, 2, 3, 4}, "7": {7}, "65535.0": {65535}} {
		got, err := versionParts(v)
		if err != nil || got != want {
			t.Errorf("versionParts(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"", "1.2.3.4.5", "0.6.0-rc1", "65536.0", "1..2", "v1.2", "-1.0"} {
		if _, err := versionParts(v); err == nil {
			t.Errorf("versionParts(%q): no error", v)
		}
	}
}

type verBlock struct {
	key      string
	typ      uint16
	value    []byte
	children []verBlock
}

// parseVer reads one structure the way Windows' VerQueryValue walks it, checking every length on the way.
func parseVer(t *testing.T, b []byte) (verBlock, int) {
	t.Helper()
	le := binary.LittleEndian
	if len(b) < 6 {
		t.Fatalf("block of %d bytes", len(b))
	}
	n, valueLen, typ := int(le.Uint16(b)), int(le.Uint16(b[2:])), le.Uint16(b[4:])
	if n > len(b) || n < 6 {
		t.Fatalf("wLength %d with %d bytes left", n, len(b))
	}
	b = b[:n]
	var key []uint16
	at := 6
	for ; ; at += 2 {
		if at+2 > n {
			t.Fatal("key without NUL")
		}
		c := le.Uint16(b[at:])
		if c == 0 {
			at += 2
			break
		}
		key = append(key, c)
	}
	at = (at + 3) &^ 3
	size := valueLen
	if typ == 1 {
		size *= 2
	}
	if at+size > n {
		t.Fatalf("%q: value of %d bytes past wLength %d", string(utf16.Decode(key)), size, n)
	}
	blk := verBlock{key: string(utf16.Decode(key)), typ: typ, value: b[at : at+size]}
	at += size
	for {
		at = (at + 3) &^ 3
		if at >= n {
			break
		}
		c, used := parseVer(t, b[at:])
		blk.children = append(blk.children, c)
		at += used
	}
	return blk, n
}

func TestVersionInfo(t *testing.T) {
	tg := targets[1]
	b := versionInfo([4]uint16{1, 2, 3, 4}, "1.2.3.4", tg)
	root, n := parseVer(t, b)
	if n != len(b) || root.key != "VS_VERSION_INFO" || root.typ != 0 || len(root.value) != 52 {
		t.Fatalf("root: %q type %d, %d of %d bytes, value %d bytes", root.key, root.typ, n, len(b), len(root.value))
	}
	le := binary.LittleEndian
	fixed := root.value
	if le.Uint32(fixed) != 0xFEEF04BD {
		t.Fatalf("signature %#x", le.Uint32(fixed))
	}
	for k, want := range []uint32{0x00010002, 0x00030004, 0x00010002, 0x00030004} {
		if got := le.Uint32(fixed[8+4*k:]); got != want {
			t.Errorf("version word %d = %#x, want %#x", k, got, want)
		}
	}
	if len(root.children) != 2 || root.children[0].key != "StringFileInfo" || root.children[1].key != "VarFileInfo" {
		t.Fatalf("children %+v", root.children)
	}
	tables := root.children[0].children
	if len(tables) != 1 || tables[0].key != "040904b0" {
		t.Fatalf("string tables %+v", tables)
	}
	got := map[string]string{}
	for _, s := range tables[0].children {
		if s.typ != 1 || len(s.value) < 2 || le.Uint16(s.value[len(s.value)-2:]) != 0 {
			t.Fatalf("string %q: type %d, value not NUL-terminated", s.key, s.typ)
		}
		u := make([]uint16, len(s.value)/2-1)
		for k := range u {
			u[k] = le.Uint16(s.value[2*k:])
		}
		got[s.key] = string(utf16.Decode(u))
	}
	want := map[string]string{
		"CompanyName": "ModAVD", "FileDescription": tg.description, "FileVersion": "1.2.3.4", "InternalName": "modavd",
		"LegalCopyright": copyright, "OriginalFilename": "modavd.exe", "ProductName": "ModAVD", "ProductVersion": "1.2.3.4",
	}
	if len(got) != len(want) {
		t.Errorf("strings %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	vars := root.children[1].children
	if len(vars) != 1 || vars[0].key != "Translation" || !bytes.Equal(vars[0].value, []byte{0x09, 0x04, 0xb0, 0x04}) {
		t.Fatalf("VarFileInfo %+v", vars)
	}
}
