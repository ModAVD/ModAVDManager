package apk

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"modavd/internal/errs"
)

// ---- a small binary XML writer -------------------------------------------------------------------------------------

type attr struct {
	name  uint32 // string index
	raw   uint32 // string index, or noIndex
	typ   byte
	value uint32
}

const noIndex = 0xFFFFFFFF

type doc struct {
	strs   []string
	utf8   bool
	ids    []uint32 // resource map; omitted when nil
	extra  [][]byte // chunks between the resource map and the start tag
	tag    uint32   // string index of the first start tag
	attrs  []attr
	noTag  bool
	header uint16 // XML chunk type, 0 = resXMLType
}

func le16(b []byte, v int) []byte    { return binary.LittleEndian.AppendUint16(b, uint16(v)) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func chunk(typ, headerSize int, header, body []byte) []byte {
	c := le16(nil, typ)
	c = le16(c, headerSize)
	c = le32(c, uint32(8+len(header)+len(body)))
	return append(append(c, header...), body...)
}

func stringPoolChunk(strs []string, utf8 bool) []byte {
	var data []byte
	var offsets []byte
	for _, s := range strs {
		offsets = le32(offsets, uint32(len(data)))
		if utf8 {
			n16 := len(utf16.Encode([]rune(s)))
			for _, n := range []int{n16, len(s)} {
				if n > 0x7F {
					data = append(data, byte(0x80|n>>8), byte(n))
				} else {
					data = append(data, byte(n))
				}
			}
			data = append(append(data, s...), 0)
		} else {
			units := utf16.Encode([]rune(s))
			if n := len(units); n > 0x7FFF {
				data = le16(data, 0x8000|n>>16)
				data = le16(data, n&0xFFFF)
			} else {
				data = le16(data, n)
			}
			for _, u := range units {
				data = le16(data, int(u))
			}
			data = le16(data, 0)
		}
	}
	for len(data)%4 != 0 {
		data = append(data, 0)
	}
	var flags uint32
	if utf8 {
		flags = utf8Flag
	}
	h := le32(nil, uint32(len(strs))) // stringCount
	h = le32(h, 0)                    // styleCount
	h = le32(h, flags)
	h = le32(h, uint32(28+len(offsets))) // stringsStart, from the chunk start
	h = le32(h, 0)                       // stylesStart
	return chunk(resStringPoolType, 28, h, append(offsets, data...))
}

func (d doc) bytes() []byte {
	body := stringPoolChunk(d.strs, d.utf8)
	if d.ids != nil {
		var ids []byte
		for _, id := range d.ids {
			ids = le32(ids, id)
		}
		body = append(body, chunk(resXMLResourceMapType, 8, nil, ids)...)
	}
	for _, e := range d.extra {
		body = append(body, e...)
	}
	if !d.noTag {
		h := le32(nil, 1)         // lineNumber
		h = le32(h, noIndex)      // comment
		ext := le32(nil, noIndex) // ns
		ext = le32(ext, d.tag)
		ext = le16(ext, 20) // attributeStart
		ext = le16(ext, 20) // attributeSize
		ext = le16(ext, len(d.attrs))
		ext = le16(ext, 0)
		ext = le16(ext, 0)
		ext = le16(ext, 0)
		for _, a := range d.attrs {
			ext = le32(ext, noIndex)
			ext = le32(ext, a.name)
			ext = le32(ext, a.raw)
			ext = le16(ext, 8)
			ext = append(ext, 0, a.typ)
			ext = le32(ext, a.value)
		}
		body = append(body, chunk(resXMLStartElementType, 16, h, ext)...)
		// an end tag after it, never looked at
		body = append(body, chunk(0x0103, 16, append(le32(nil, 1), le32(nil, noIndex)...), le32(le32(nil, noIndex), d.tag))...)
	}
	typ := int(d.header)
	if typ == 0 {
		typ = resXMLType
	}
	return chunk(typ, 8, nil, body)
}

// namespaceChunk is a start-namespace chunk (0x0100) as aapt writes before <manifest>.
func namespaceChunk(prefix, uri uint32) []byte {
	h := le32(le32(nil, 1), noIndex)
	return chunk(0x0100, 16, h, le32(le32(nil, prefix), uri))
}

// standard: strings 0 versionCode (resource id), 1 versionName, 2 package, 3 manifest, 4 android, 5 uri, 6 value.
func standard(utf8 bool, pkg string) doc {
	return doc{
		strs: []string{"versionCode", "versionName", "package", "manifest", "android",
			"http://schemas.android.com/apk/res/android", pkg, "1.0"},
		utf8:  utf8,
		ids:   []uint32{versionCodeAttr, 0x0101021c},
		extra: [][]byte{namespaceChunk(4, 5)},
		tag:   3,
		attrs: []attr{
			{name: 0, raw: noIndex, typ: typeIntDec, value: 57},
			{name: 1, raw: 7, typ: typeString, value: 7},
			{name: 2, raw: 6, typ: typeString, value: 6},
		},
	}
}

// ---- parser ------------------------------------------------------------------------------------------------------

func TestParse(t *testing.T) {
	long := strings.Repeat("a", 200) + ".pkg" // 2-byte UTF-8 lengths
	huge := strings.Repeat("b", 0x8001)       // 2-word UTF-16 length
	cjk := "包名.測試" + strings.Repeat("字", 60)  // UTF-16 length < 0x80 but UTF-8 length > 0x7F

	cases := []struct {
		name string
		doc  doc
		want Manifest
	}{
		{"utf8", standard(true, "de.jrpie.android.launcher"), Manifest{"de.jrpie.android.launcher", 57}},
		{"utf16", standard(false, "org.modavd.ime"), Manifest{"org.modavd.ime", 57}},
		{"utf8 long", standard(true, long), Manifest{long, 57}},
		{"utf16 huge", standard(false, huge), Manifest{huge, 57}},
		{"utf8 cjk", standard(true, cjk), Manifest{cjk, 57}},
		{"utf16 cjk", standard(false, cjk), Manifest{cjk, 57}},
		{"hex and max", func() doc {
			d := standard(true, "p")
			d.attrs[0] = attr{name: 0, raw: noIndex, typ: typeIntHex, value: 0xFFFFFFFF}
			return d
		}(), Manifest{"p", 0xFFFFFFFF}},
		{"versionCode string", func() doc {
			d := standard(false, "p")
			d.strs = append(d.strs, " 123 ")
			d.attrs[0] = attr{name: 0, raw: uint32(len(d.strs) - 1), typ: typeString}
			return d
		}(), Manifest{"p", 123}},
		{"versionCode other type", func() doc {
			d := standard(true, "p")
			d.attrs[0] = attr{name: 0, raw: noIndex, typ: 0x01, value: 0x7f010000} // a reference
			return d
		}(), Manifest{"p", 0}},
		{"by name without resource map", func() doc {
			d := standard(false, "p")
			d.ids = nil
			d.attrs[0].typ, d.attrs[0].value = typeIntHex, 0x42
			return d
		}(), Manifest{"p", 0x42}},
		{"by resource id under another name", func() doc {
			d := standard(true, "p")
			d.strs[0] = "a" // obfuscated attribute name, the id still says versionCode
			return d
		}(), Manifest{"p", 57}},
		{"name versionCode, other id", func() doc {
			d := standard(true, "p")
			d.ids[0] = 0x01010000 // not android:versionCode, and not 0: the name does not count
			return d
		}(), Manifest{"p", 0}},
		{"no package, no versionCode", func() doc {
			d := standard(true, "p")
			d.attrs = d.attrs[1:2]
			return d
		}(), Manifest{"", 0}},
		{"attribute order", func() doc {
			d := standard(false, "q")
			d.attrs[0], d.attrs[2] = d.attrs[2], d.attrs[0]
			return d
		}(), Manifest{"q", 57}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parseManifest(c.doc.bytes())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *m != c.want {
				t.Errorf("got %q/%d, want %q/%d", trim(m.Package), m.VersionCode, trim(c.want.Package), c.want.VersionCode)
			}
		})
	}
}

func trim(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func TestParseRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":        nil,
		"short":        {3, 0, 8, 0},
		"text":         []byte("<?xml version=\"1.0\"?><manifest package=\"a\"/>"),
		"wrong header": doc{strs: []string{"manifest"}, tag: 0, header: 0x0002}.bytes(),
		"no start tag": func() []byte { d := standard(true, "p"); d.noTag = true; return d.bytes() }(),
		"not manifest": func() []byte { d := standard(true, "p"); d.tag = 4; return d.bytes() }(),
		"tag index out of range": func() []byte {
			d := standard(true, "p")
			d.tag = 99
			return d.bytes()
		}(),
		"package raw index out of range": func() []byte {
			d := standard(true, "p")
			d.attrs[2].raw = 99
			return d.bytes()
		}(),
		"attribute name out of range": func() []byte {
			d := standard(true, "p")
			d.attrs[1].name = 99
			return d.bytes()
		}(),
		"versionCode not a number": func() []byte {
			d := standard(true, "p")
			d.attrs[0] = attr{name: 0, raw: 7, typ: typeString} // "1.0"
			return d.bytes()
		}(),
		"lone surrogate": func() []byte {
			d := standard(false, "p")
			b := d.bytes()
			// "p" is a 1-unit string; find it (len 1, 'p', NUL) and make the unit a lone high surrogate
			i := bytes.Index(b, []byte{1, 0, 'p', 0, 0, 0})
			b[i+2], b[i+3] = 0x00, 0xD8
			return b
		}(),
		"chunk size too big": func() []byte {
			b := standard(true, "p").bytes()
			binary.LittleEndian.PutUint32(b[8+4:], 1<<30) // the string pool's size
			return b
		}(),
		"chunk size too small": func() []byte {
			b := standard(true, "p").bytes()
			binary.LittleEndian.PutUint32(b[8+4:], 4)
			return b
		}(),
	}
	for name, data := range cases {
		if m, err := parseManifest(data); err == nil {
			t.Errorf("%s: got %+v, want an error", name, m)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// Every prefix and many random corruptions of valid manifests must give a result or an error, never a panic.
func TestParseMalformedNoPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, utf8 := range []bool{true, false} {
		good := standard(utf8, "org.modavd.ime").bytes()
		for n := 0; n <= len(good); n++ {
			parseManifest(good[:n])
		}
		for k := 0; k < 20000; k++ {
			b := bytes.Clone(good)
			for j := rng.Intn(4); j >= 0; j-- {
				b[rng.Intn(len(b))] = byte(rng.Intn(256))
			}
			parseManifest(b)
		}
	}
}

func FuzzParseManifest(f *testing.F) {
	f.Add(standard(true, "a.b").bytes())
	f.Add(standard(false, "a.b").bytes())
	f.Add([]byte{3, 0, 8, 0, 8, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := parseManifest(data)
		if (m == nil) == (err == nil) {
			t.Fatalf("got %v, %v", m, err)
		}
	})
}

// ---- zip ---------------------------------------------------------------------------------------------------------

type entry struct {
	name   string
	data   []byte
	store  bool
	header *zip.FileHeader // written raw when set
}

func writeZip(t *testing.T, entries ...entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.apk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		var w io.Writer
		switch {
		case e.header != nil:
			w, err = zw.CreateRaw(e.header)
		case e.store:
			w, err = zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Store})
		default:
			w, err = zw.Create(e.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadManifestZip(t *testing.T) {
	good := standard(true, "org.modavd.ime").bytes()
	for _, store := range []bool{false, true} {
		path := writeZip(t, entry{name: "classes.dex", data: []byte("dex\n035")},
			entry{name: manifestEntry, data: good, store: store})
		m, err := ReadManifest(path)
		if err != nil || m.Package != "org.modavd.ime" || m.VersionCode != 57 {
			t.Errorf("store=%v: %+v, %v", store, m, err)
		}
	}
	// a duplicated entry: the last one wins, as in Python's zipfile
	other := standard(true, "second").bytes()
	path := writeZip(t, entry{name: manifestEntry, data: good}, entry{name: manifestEntry, data: other})
	if m, err := ReadManifest(path); err != nil || m.Package != "second" {
		t.Errorf("duplicate: %+v, %v", m, err)
	}
}

func TestReadManifestRejects(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "not.apk")
	if err := os.WriteFile(text, []byte("this is not a zip file"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := standard(true, "p").bytes()
	cases := map[string]string{
		"missing":     filepath.Join(dir, "missing.apk"),
		"directory":   dir,
		"text":        text,
		"no manifest": writeZip(t, entry{name: "classes.dex", data: []byte("x")}),
		"nested name": writeZip(t, entry{name: "res/" + manifestEntry, data: good}),
		"text manifest": writeZip(t, entry{name: manifestEntry,
			data: []byte(`<?xml version="1.0"?><manifest package="p" android:versionCode="1"/>`)}),
		"bad crc": writeZip(t, entry{header: &zip.FileHeader{Name: manifestEntry, Method: zip.Store,
			CRC32: crc32.ChecksumIEEE(good) ^ 1, CompressedSize64: uint64(len(good)),
			UncompressedSize64: uint64(len(good))}, data: good}),
		"too big": writeZip(t, entry{header: &zip.FileHeader{Name: manifestEntry, Method: zip.Store,
			CRC32: crc32.ChecksumIEEE(good), CompressedSize64: uint64(len(good)),
			UncompressedSize64: maxManifestBytes + 1}, data: good}),
		"bad deflate": writeZip(t, entry{header: &zip.FileHeader{Name: manifestEntry, Method: zip.Deflate,
			CRC32: crc32.ChecksumIEEE(good), CompressedSize64: 16, UncompressedSize64: uint64(len(good))},
			data: bytes.Repeat([]byte{0xff}, 16)}),
		"unknown method": writeZip(t, entry{header: &zip.FileHeader{Name: manifestEntry, Method: 99,
			CRC32: crc32.ChecksumIEEE(good), CompressedSize64: uint64(len(good)),
			UncompressedSize64: uint64(len(good))}, data: good}),
	}
	wantMsg := map[string]string{
		"missing":       "APK 文件不存在",
		"text":          "不是 APK（不是 zip 文件）",
		"directory":     "不是 APK",
		"no manifest":   "不是 APK（里面没有 AndroidManifest.xml）",
		"nested name":   "不是 APK（里面没有 AndroidManifest.xml）",
		"text manifest": "的包名和版本号: 不是二进制 XML",
		"bad crc":       "里的 AndroidManifest.xml",
		"too big":       "太大",
	}
	for name, path := range cases {
		m, err := ReadManifest(path)
		if err != nil && !strings.Contains(err.Error(), wantMsg[name]) {
			t.Errorf("%s: message %q, want it to contain %q", name, err.Error(), wantMsg[name])
		}
		if m != nil || errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%s: got %+v, %v; want invalid_argument", name, m, err)
			continue
		}
		t.Logf("%s: %v", name, err)
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: message %q does not name the file", name, err.Error())
		}
	}
}

// ---- real APKs ---------------------------------------------------------------------------------------------------

// Expected values come from Get-ApkManifestInfo in the launcher embedded helper, run with pwsh on the same files.
// The IME's APKs are looked for in $MODAVD_TEST_IME_APK_DIR (the IME build's output directory); without it those
// cases are skipped.
func TestReadManifestRealAPKs(t *testing.T) {
	imeDir := os.Getenv("MODAVD_TEST_IME_APK_DIR")
	cases := []struct {
		dir, name string
		want      Manifest
	}{
		{`D:\ModAVD\components`, "de.jrpie.android.launcher_57.apk", Manifest{"de.jrpie.android.launcher", 57}},
		{imeDir, "modavd-ime.apk", Manifest{"org.modavd.ime", 1}},
		{imeDir, "modavd-ime-testapp.apk", Manifest{"org.modavd.ime.testapp", 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.dir == "" {
				t.Skip("MODAVD_TEST_IME_APK_DIR 没设")
			}
			path := filepath.Join(c.dir, c.name)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Skipf("%s 不存在", path)
			}
			m, err := ReadManifest(path)
			if err != nil {
				t.Fatalf("ReadManifest: %v", err)
			}
			if *m != c.want {
				t.Errorf("got %+v, want %+v", *m, c.want)
			}
		})
	}
}
