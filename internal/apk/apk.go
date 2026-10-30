// Package apk reads the package name and versionCode from an APK's binary AndroidManifest.xml, without aapt
// (neither the release package nor dev SDKs have it). Port of Get-ApkManifestInfo in tools/launcher/avd-p0.ps1:
// walk the chunks — string pool (0x0001), resource ids (0x0180), the first start tag (<manifest>) — and read its
// "package" (string attribute) and "versionCode" (integer attribute; resource id 0x0101021b).
//
// AUTO-MAS has the same port (app/utils/common/emulator2/avd/apk_manifest.py). The two only differ on malformed
// input; this fails the whole parse wherever either of them fails (a string index out of range, a truncated string,
// broken UTF-16, a versionCode string that is not a number), and otherwise reads the same values.
package apk

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"unicode/utf16"

	"modavd/internal/errs"
)

type Manifest struct {
	Package     string
	VersionCode int64
}

const (
	manifestEntry = "AndroidManifest.xml"

	resXMLType             = 0x0003
	resStringPoolType      = 0x0001
	resXMLResourceMapType  = 0x0180
	resXMLStartElementType = 0x0102
	utf8Flag               = 0x100
	versionCodeAttr        = 0x0101021B // android:versionCode
	// attribute value types: decimal / hexadecimal integer, string
	typeIntDec, typeIntHex, typeString = 0x10, 0x11, 0x03

	// Real manifests are well under 1 MB; the cap only keeps a zip bomb from eating the memory.
	maxManifestBytes = 32 << 20
)

// ReadManifest opens the APK (a zip) and parses AndroidManifest.xml. errs.InvalidArgument when it is not such an APK.
//
// Package is "" when the manifest has no package attribute, VersionCode 0 when it has no versionCode (or one of an
// unexpected type), like AUTO-MAS's None; callers check both, as the launcher and AUTO-MAS do.
func ReadManifest(path string) (*Manifest, error) {
	data, err := readEntry(path)
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(data)
	if err != nil {
		return nil, errs.Wrap(errs.InvalidArgument, err, fmt.Sprintf("读不出 %s 的包名和版本号: %v", path, err))
	}
	return m, nil
}

func readEntry(path string) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	// with GODEBUG zipinsecurepath=0 the reader comes back together with ErrInsecurePath and is still usable
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && zr != nil) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, errs.Wrap(errs.InvalidArgument, err, "APK 文件不存在: "+path)
		case errors.Is(err, zip.ErrFormat):
			return nil, errs.Wrap(errs.InvalidArgument, err, path+" 不是 APK（不是 zip 文件）")
		}
		return nil, errs.Wrap(errs.InvalidArgument, err, "打不开 APK 文件: "+path)
	}
	defer zr.Close()
	// Python's zipfile (AUTO-MAS) takes the last entry of a duplicated name
	var entry *zip.File
	for _, f := range zr.File {
		if f.Name == manifestEntry {
			entry = f
		}
	}
	if entry == nil {
		return nil, errs.New(errs.InvalidArgument, path+" 不是 APK（里面没有 AndroidManifest.xml）")
	}
	if entry.UncompressedSize64 > maxManifestBytes {
		return nil, errs.Newf(errs.InvalidArgument, "%s 里的 AndroidManifest.xml 太大（%d 字节）", path, entry.UncompressedSize64)
	}
	// Bad compressed data, unknown methods, encrypted entries, CRC mismatches: all just "cannot read", never more.
	// The zip error stays the cause (debug output); the message is the same for all of them.
	unreadable := func(err error) error {
		return errs.Wrap(errs.InvalidArgument, err, "读不出 "+path+" 里的 AndroidManifest.xml")
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, unreadable(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return nil, unreadable(err)
	}
	if len(data) > maxManifestBytes {
		return nil, errs.Newf(errs.InvalidArgument, "%s 里的 AndroidManifest.xml 太大", path)
	}
	return data, nil
}

// parseError carries a malformed-input reason out of the bounds-checked readers (AUTO-MAS catches IndexError /
// struct.error / ValueError around the whole parse the same way).
type parseError string

func fail(format string, args ...any) { panic(parseError(fmt.Sprintf(format, args...))) }

type buf []byte

func (b buf) u8(off int) int {
	if off < 0 || off >= len(b) {
		fail("偏移 %d 越界", off)
	}
	return int(b[off])
}

func (b buf) u16(off int) int {
	if off < 0 || off > len(b)-2 {
		fail("偏移 %d 越界", off)
	}
	return int(binary.LittleEndian.Uint16(b[off:]))
}

func (b buf) u32(off int) uint32 {
	if off < 0 || off > len(b)-4 {
		fail("偏移 %d 越界", off)
	}
	return binary.LittleEndian.Uint32(b[off:])
}

// at is base + a u32 offset; anything past the end could not be read anyway (and would overflow int on 386).
func (b buf) at(base int, add uint32) int {
	v := int64(base) + int64(add)
	if v > int64(len(b)) {
		fail("偏移 %d 越界", v)
	}
	return int(v)
}

func (b buf) span(off, n int) []byte {
	if off < 0 || n < 0 || off > len(b) || n > len(b)-off {
		fail("偏移 %d 长度 %d 越界", off, n)
	}
	return b[off : off+n]
}

type pool []string

func (p pool) get(i uint32) string {
	if uint64(i) >= uint64(len(p)) {
		fail("字符串下标 %d 越界（共 %d 个）", i, len(p))
	}
	return p[i]
}

func parseManifest(data []byte) (m *Manifest, err error) {
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(parseError)
			if !ok {
				panic(r)
			}
			m, err = nil, errors.New(string(pe))
		}
	}()
	b := buf(data)
	if len(b) < 8 || b.u16(0) != resXMLType {
		return nil, errors.New("不是二进制 XML")
	}
	var strs pool
	var ids []uint32
	pos := b.u16(2)
	for pos+8 <= len(b) {
		chunkType, headerSize, chunkSize := b.u16(pos), b.u16(pos+2), b.u32(pos+4)
		if chunkSize < 8 || int64(pos)+int64(chunkSize) > int64(len(b)) {
			return nil, fmt.Errorf("数据块（偏移 %d）大小 %d 不对", pos, chunkSize)
		}
		end := pos + int(chunkSize)
		switch chunkType {
		case resStringPoolType:
			strs = stringPool(b, pos, headerSize)
		case resXMLResourceMapType:
			if pos+headerSize > end {
				fail("资源 ID 表头大小 %d 不对", headerSize)
			}
			ids = ids[:0]
			for o := pos + headerSize; o+4 <= end; o += 4 {
				ids = append(ids, b.u32(o))
			}
		case resXMLStartElementType:
			return manifestAttributes(b, pos+headerSize, strs, ids)
		}
		pos = end
	}
	return nil, errors.New("没有找到开始标签")
}

func stringPool(b buf, pos, headerSize int) pool {
	count := b.u32(pos + 8)
	utf8 := b.u32(pos+16)&utf8Flag != 0
	start := b.at(pos, b.u32(pos+20))
	// each string needs a 4-byte offset entry, so a bigger count fails below anyway; do not trust it for the allocation
	result := make(pool, 0, min(uint64(count), uint64(len(b)/4)))
	for i := uint32(0); i < count; i++ {
		off := b.at(start, b.u32(pos+headerSize+4*int(i)))
		if utf8 {
			// the UTF-16 length, then the UTF-8 byte length, 1 or 2 bytes each
			if b.u8(off)&0x80 != 0 {
				off += 2
			} else {
				off++
			}
			n := b.u8(off)
			if n&0x80 != 0 {
				n = (n&0x7F)<<8 | b.u8(off+1)
				off += 2
			} else {
				off++
			}
			result = append(result, strings.ToValidUTF8(string(b.span(off, n)), "�"))
		} else {
			n := b.u16(off)
			off += 2
			if n&0x8000 != 0 {
				n = (n&0x7FFF)<<16 | b.u16(off)
				off += 2
			}
			raw := b.span(off, 2*n)
			units := make([]uint16, n)
			for k := range units {
				units[k] = binary.LittleEndian.Uint16(raw[2*k:])
			}
			if !validUTF16(units) {
				fail("字符串 %d 不是合法的 UTF-16", i) // Python's strict utf-16-le decode raises here
			}
			result = append(result, string(utf16.Decode(units)))
		}
	}
	return result
}

// validUTF16: every surrogate is part of a high-low pair.
func validUTF16(units []uint16) bool {
	for k := 0; k < len(units); k++ {
		switch u := units[k]; {
		case u >= 0xD800 && u < 0xDC00:
			if k+1 >= len(units) || units[k+1] < 0xDC00 || units[k+1] >= 0xE000 {
				return false
			}
			k++
		case u >= 0xDC00 && u < 0xE000:
			return false
		}
	}
	return true
}

func manifestAttributes(b buf, ext int, strs pool, ids []uint32) (*Manifest, error) {
	// ResXMLTree_attrExt: namespace, tag name, then where the attributes start, the size and the count of them
	if name := strs.get(b.u32(ext + 4)); name != "manifest" {
		return nil, fmt.Errorf("第一个开始标签是 <%s>，不是 <manifest>", name)
	}
	attrStart, attrSize, attrCount := b.u16(ext+8), b.u16(ext+10), b.u16(ext+12)
	m := &Manifest{}
	for k := 0; k < attrCount; k++ {
		at := ext + attrStart + k*attrSize
		nameIndex, rawValue := b.u32(at+4), b.u32(at+8)
		dataType := b.u8(at + 15)
		value := b.u32(at + 16)
		var resID uint32
		if uint64(nameIndex) < uint64(len(ids)) {
			resID = ids[nameIndex]
		}
		if resID == versionCodeAttr || (resID == 0 && strs.get(nameIndex) == "versionCode") {
			switch dataType {
			case typeIntDec, typeIntHex:
				m.VersionCode = int64(value) // unsigned, like the launcher's [long] of a UInt32
			case typeString:
				s := strs.get(rawValue)
				v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("versionCode %q 不是整数", s)
				}
				m.VersionCode = v
			default:
				m.VersionCode = 0
			}
		} else if strs.get(nameIndex) == "package" {
			m.Package = strs.get(rawValue)
		}
	}
	return m, nil
}
