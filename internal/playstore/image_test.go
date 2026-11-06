package playstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"modavd/internal/testutil"
)

// ---- a tiny ext4 ------------------------------------------------------------------------------------------------
//
// One block group, 1 KiB blocks: block 1 the superblock, block 2 the group descriptors, blocks 3–4 the inode table
// (16 inodes of 128 bytes), data from block 5. Directories and files use extents; a file can ask for a depth-1 tree,
// a hole before its data or an uninitialized extent.

const tbs = 1024

type tfile struct {
	data   []byte
	hole   int  // blocks of hole before the data
	uninit int  // trailing blocks in an uninitialized extent (read as zeros, part of the size)
	deep   bool // a depth-1 extent tree
}

type tfs struct {
	img    []byte
	next   int64 // next free block
	inodes uint32
}

func newTFS(blocks int) *tfs {
	return &tfs{img: make([]byte, blocks*tbs), next: 5, inodes: 2}
}

func (f *tfs) alloc(n int64) int64 { b := f.next; f.next += n; return b }

func (f *tfs) putInode(ino uint32, mode uint16, size int64, block []byte) {
	off := 3*tbs + int64(ino-1)*128
	binary.LittleEndian.PutUint16(f.img[off:], mode)
	binary.LittleEndian.PutUint32(f.img[off+4:], uint32(size))
	binary.LittleEndian.PutUint32(f.img[off+0x6C:], uint32(size>>32))
	binary.LittleEndian.PutUint32(f.img[off+0x20:], flagExtents)
	copy(f.img[off+0x28:off+0x28+60], block)
}

func extentHeader(entries, depth int) []byte {
	h := make([]byte, 12)
	binary.LittleEndian.PutUint16(h, extentMagic)
	binary.LittleEndian.PutUint16(h[2:], uint16(entries))
	binary.LittleEndian.PutUint16(h[4:], 4)
	binary.LittleEndian.PutUint16(h[6:], uint16(depth))
	return h
}

func extentLeaf(logical uint32, n uint16, phys int64) []byte {
	e := make([]byte, 12)
	binary.LittleEndian.PutUint32(e, logical)
	binary.LittleEndian.PutUint16(e[4:], n)
	binary.LittleEndian.PutUint16(e[6:], uint16(phys>>32))
	binary.LittleEndian.PutUint32(e[8:], uint32(phys))
	return e
}

// file writes a regular file and returns its inode.
func (f *tfs) file(t tfile) uint32 {
	f.inodes++
	ino := f.inodes
	nb := (int64(len(t.data)) + tbs - 1) / tbs
	phys := f.alloc(nb)
	copy(f.img[phys*tbs:], t.data)
	leaves := [][]byte{extentLeaf(uint32(t.hole), uint16(nb), phys)}
	size := int64(t.hole)*tbs + int64(len(t.data))
	if t.uninit > 0 {
		size = (int64(t.hole)+nb)*tbs + int64(t.uninit)*tbs
		junk := f.alloc(int64(t.uninit)) // allocated but never written: what is there must not be read
		for k := junk * tbs; k < (junk+int64(t.uninit))*tbs; k++ {
			f.img[k] = 0xAA
		}
		leaves = append(leaves, extentLeaf(uint32(int64(t.hole)+nb), uint16(t.uninit)+uninitExtentFlag, junk))
	}
	var block []byte
	if t.deep {
		leafBlock := f.alloc(1)
		node := extentHeader(len(leaves), 0)
		for _, l := range leaves {
			node = append(node, l...)
		}
		copy(f.img[leafBlock*tbs:], node)
		idx := make([]byte, 12)
		binary.LittleEndian.PutUint32(idx[4:], uint32(leafBlock))
		block = append(extentHeader(1, 1), idx...)
	} else {
		block = extentHeader(len(leaves), 0)
		for _, l := range leaves {
			block = append(block, l...)
		}
	}
	f.putInode(ino, 0x8000|0o644, size, block)
	return ino
}

// dir writes a directory of the given entries (name -> inode, with a type byte) at inode ino (0: a new one).
func (f *tfs) dir(ino uint32, entries map[string]uint32, isDir map[string]bool) uint32 {
	if ino == 0 {
		f.inodes++
		ino = f.inodes
	}
	var d []byte
	add := func(i uint32, name string, typ byte, last bool) {
		rec := (8 + len(name) + 3) &^ 3
		if last {
			rec = tbs - len(d)
		}
		e := make([]byte, rec)
		binary.LittleEndian.PutUint32(e, i)
		binary.LittleEndian.PutUint16(e[4:], uint16(rec))
		e[6], e[7] = byte(len(name)), typ
		copy(e[8:], name)
		d = append(d, e...)
	}
	add(ino, ".", 2, false)
	add(2, "..", 2, false)
	add(0, "", 0, false) // an empty slot, like a deleted entry or an htree node's fake one
	k := 0
	for name, i := range entries {
		k++
		typ := byte(1)
		if isDir[name] {
			typ = 2
		}
		add(i, name, typ, k == len(entries))
	}
	b := f.alloc(1)
	copy(f.img[b*tbs:], d)
	f.putInode(ino, 0x4000|0o755, tbs, append(extentHeader(1, 0), extentLeaf(0, 1, b)...))
	return ino
}

func (f *tfs) finish() []byte {
	sb := f.img[1024:]
	binary.LittleEndian.PutUint32(sb[0x00:], 16)
	binary.LittleEndian.PutUint32(sb[0x04:], uint32(len(f.img)/tbs))
	binary.LittleEndian.PutUint32(sb[0x14:], 1)
	binary.LittleEndian.PutUint32(sb[0x18:], 0)
	binary.LittleEndian.PutUint32(sb[0x20:], 8192)
	binary.LittleEndian.PutUint32(sb[0x28:], 16)
	binary.LittleEndian.PutUint16(sb[0x38:], ext4Magic)
	binary.LittleEndian.PutUint32(sb[0x4C:], 1)
	binary.LittleEndian.PutUint16(sb[0x58:], 128)
	binary.LittleEndian.PutUint32(sb[0x60:], 0x2|0x40)
	binary.LittleEndian.PutUint32(f.img[2*tbs+0x08:], 3) // inode table at block 3
	return f.img
}

// tree builds an ext4 holding files at the given "/"-separated paths.
func tree(files map[string]tfile) []byte {
	f := newTFS(1024)
	type node struct {
		kids map[string]*node
		ino  uint32
	}
	rootNode := &node{kids: map[string]*node{}}
	for p, tf := range files {
		n := rootNode
		parts := strings.Split(p, "/")
		for _, part := range parts[:len(parts)-1] {
			if n.kids[part] == nil {
				n.kids[part] = &node{kids: map[string]*node{}}
			}
			n = n.kids[part]
		}
		n.kids[parts[len(parts)-1]] = &node{ino: f.file(tf)}
	}
	var write func(n *node, ino uint32) uint32
	write = func(n *node, ino uint32) uint32 {
		entries, dirs := map[string]uint32{}, map[string]bool{}
		for name, k := range n.kids {
			if k.kids != nil {
				entries[name], dirs[name] = write(k, 0), true
			} else {
				entries[name] = k.ino
			}
		}
		return f.dir(ino, entries, dirs)
	}
	write(rootNode, 2)
	return f.finish()
}

// ---- liblp and GPT around it --------------------------------------------------------------------------------------

// systemImage is a GPT image with a vbmeta and a super partition whose dynamic partitions are parts (in order, each
// as one linear extent, or two for split).
func systemImage(t *testing.T, parts []namedPart) []byte {
	t.Helper()
	const superLBA, dataAt = 64, 65536 // super at sector 64; its first logical partition data at 64 KiB
	var data []byte
	type placed struct {
		name string
		ext  [][2]int64 // sectors, start sector in super
	}
	var ps []placed
	for _, p := range parts {
		at := int64(dataAt + len(data))
		pad := (len(p.fs) + sector - 1) / sector * sector
		buf := make([]byte, pad)
		copy(buf, p.fs)
		if p.split {
			// the first 4 KiB (superblock, group descriptors) in one extent, the rest (inodes, data) in the other;
			// stored second part first, mapped in the right order
			half := int64(8 * sector)
			data = append(append(data, buf[half:]...), buf[:half]...)
			ps = append(ps, placed{p.name, [][2]int64{{half / sector, (at + int64(pad) - half) / sector}, {(int64(pad) - half) / sector, at / sector}}})
		} else {
			data = append(data, buf...)
			ps = append(ps, placed{p.name, [][2]int64{{int64(pad / sector), at / sector}}})
		}
	}
	super := make([]byte, dataAt+len(data))
	copy(super[dataAt:], data)
	// geometry
	g := super[lpReserved:]
	binary.LittleEndian.PutUint32(g, lpGeometryMagic)
	binary.LittleEndian.PutUint32(g[4:], 52)
	binary.LittleEndian.PutUint32(g[40:], 8192) // metadata_max_size
	binary.LittleEndian.PutUint32(g[44:], 1)
	binary.LittleEndian.PutUint32(g[48:], 4096)
	sum := sha256.Sum256(g[:52])
	copy(g[8:40], sum[:])
	// tables: partitions, then extents
	var pt, et []byte
	for _, p := range ps {
		e := make([]byte, 52)
		copy(e, p.name)
		binary.LittleEndian.PutUint32(e[40:], uint32(len(et)/24))
		binary.LittleEndian.PutUint32(e[44:], uint32(len(p.ext)))
		pt = append(pt, e...)
		for _, x := range p.ext {
			b := make([]byte, 24)
			binary.LittleEndian.PutUint64(b, uint64(x[0]))
			binary.LittleEndian.PutUint64(b[12:], uint64(x[1]))
			et = append(et, b...)
		}
	}
	tables := append(append([]byte{}, pt...), et...)
	h := make([]byte, 128)
	binary.LittleEndian.PutUint32(h, lpHeaderMagic)
	binary.LittleEndian.PutUint16(h[4:], 10)
	binary.LittleEndian.PutUint32(h[8:], 128)
	binary.LittleEndian.PutUint32(h[44:], uint32(len(tables)))
	ts := sha256.Sum256(tables)
	copy(h[48:80], ts[:])
	desc := func(at, off, n, size int) {
		binary.LittleEndian.PutUint32(h[at:], uint32(off))
		binary.LittleEndian.PutUint32(h[at+4:], uint32(n))
		binary.LittleEndian.PutUint32(h[at+8:], uint32(size))
	}
	desc(80, 0, len(ps), 52)
	desc(92, len(pt), len(et)/24, 24)
	desc(104, len(tables), 0, 48)
	desc(116, len(tables), 0, 64)
	hs := sha256.Sum256(h)
	copy(h[12:44], hs[:])
	copy(super[lpReserved+2*lpGeometrySize:], append(h, tables...))

	img := make([]byte, superLBA*sector+len(super)+34*sector)
	copy(img[superLBA*sector:], super)
	// GPT header and entries (CRCs are not checked by the reader)
	copy(img[sector:], "EFI PART")
	binary.LittleEndian.PutUint64(img[sector+72:], 2)
	binary.LittleEndian.PutUint32(img[sector+80:], 4)
	binary.LittleEndian.PutUint32(img[sector+84:], 128)
	entry := func(k int, name string, first, last int64) {
		e := img[2*sector+k*128:]
		e[0] = 1 // any non-zero type
		binary.LittleEndian.PutUint64(e[32:], uint64(first))
		binary.LittleEndian.PutUint64(e[40:], uint64(last))
		for j, u := range utf16.Encode([]rune(name)) {
			binary.LittleEndian.PutUint16(e[56+2*j:], u)
		}
	}
	entry(0, "vbmeta", 8, 15)
	entry(1, "super", superLBA, superLBA+int64(len(super)/sector)-1)
	return img
}

type namedPart struct {
	name  string
	fs    []byte
	split bool
}

func fileOf(name, part, p string, data []byte) File {
	s := sha256.Sum256(data)
	return File{Name: name, Partition: part, Path: p, Size: int64(len(data)), SHA256: hex.EncodeToString(s[:])}
}

func writeImage(t *testing.T, img []byte) string {
	t.Helper()
	p := filepath.Join(testutil.TempDir(t), "system.img")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for k := range b {
		b[k] = seed + byte(k*7) + byte(k>>9)
	}
	return b
}

// TestExtract takes files out of a synthetic system.img: two dynamic partitions (one split into two extents stored
// out of order), nested directories, a depth-1 extent tree, a hole and an uninitialized extent.
func TestExtract(t *testing.T) {
	store := pattern(5000, 1)
	gms := pattern(3*tbs+17, 2)
	gsf := pattern(700, 3)
	product := tree(map[string]tfile{
		"priv-app/Phonesky/Phonesky.apk":               {data: store, deep: true},
		"priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk": {data: gms, hole: 2, uninit: 1},
		"etc/other": {data: []byte("x")},
	})
	sysext := tree(map[string]tfile{"priv-app/GoogleServicesFramework/GoogleServicesFramework.apk": {data: gsf}})
	img := writeImage(t, systemImage(t, []namedPart{{"system", tree(map[string]tfile{"a": {data: []byte("a")}}), false},
		{"product", product, true}, {"system_ext", sysext, false}}))

	gmsFull := append(append(make([]byte, 2*tbs), gms...), make([]byte, tbs-17+tbs)...)
	files := []File{
		fileOf("Phonesky.apk", "product", "priv-app/Phonesky/Phonesky.apk", store),
		fileOf("PrebuiltGmsCore.apk", "product", "priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk", gmsFull),
		fileOf("GoogleServicesFramework.apk", "system_ext", "priv-app/GoogleServicesFramework/GoogleServicesFramework.apk", gsf),
	}
	dir := testutil.TempDir(t)
	if err := Extract(img, files, dir); err != nil {
		t.Fatal(err)
	}
	for k, want := range [][]byte{store, gmsFull, gsf} {
		got, err := os.ReadFile(filepath.Join(dir, files[k].Name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, want %d (%v)", files[k].Name, len(got), len(want), err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// TestExtractRefuses: a pinned hash or size that does not match, a missing file or partition, and damaged liblp
// metadata are errors, and no file is left for a rejected one.
func TestExtractRefuses(t *testing.T) {
	data := pattern(2100, 9)
	good := fileOf("Phonesky.apk", "product", "priv-app/Phonesky/Phonesky.apk", data)
	build := func() []byte {
		return systemImage(t, []namedPart{{"product", tree(map[string]tfile{"priv-app/Phonesky/Phonesky.apk": {data: data}}), false}})
	}
	cases := []struct {
		name   string
		file   File
		damage func(img []byte)
		want   string
	}{
		{"hash", File{good.Name, good.Partition, good.Path, good.Size, strings.Repeat("0", 64)}, nil, "SHA-256"},
		{"size", File{good.Name, good.Partition, good.Path, good.Size + 1, good.SHA256}, nil, "大小"},
		{"path", File{good.Name, good.Partition, "priv-app/Other/Other.apk", good.Size, good.SHA256}, nil, "没有 Other"},
		{"partition", File{good.Name, "system_ext", good.Path, good.Size, good.SHA256}, nil, "没有 system_ext 分区"},
		{"tables", good, func(img []byte) { img[64*sector+lpReserved+2*lpGeometrySize+128] ^= 1 }, "校验和"},
		{"header", good, func(img []byte) { img[64*sector+lpReserved+2*lpGeometrySize+80] ^= 1 }, "校验和"},
		{"geometry", good, func(img []byte) { img[64*sector+lpReserved+44] ^= 1 }, "校验和"},
		{"gpt", good, func(img []byte) { img[sector] = 'X' }, "GPT"},
	}
	for _, c := range cases {
		img := build()
		if c.damage != nil {
			c.damage(img)
		}
		dir := testutil.TempDir(t)
		err := Extract(writeImage(t, img), []File{c.file}, dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one about %q", c.name, err, c.want)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: left %d files", c.name, len(entries))
		}
	}
}

// TestFilesTable: the table has the five files, each with a 64-digit hash and a guest path on product or system_ext,
// and the manifest the guest script reads lists them in order.
func TestFilesTable(t *testing.T) {
	if len(Files) != 5 {
		t.Fatalf("%d files", len(Files))
	}
	lines := strings.Split(strings.TrimSuffix(Manifest(), "\n"), "\n")
	var total int64
	for k, f := range Files {
		if len(f.SHA256) != 64 || strings.Trim(f.SHA256, "0123456789abcdef") != "" || f.Size <= 0 {
			t.Errorf("%s: %+v", f.Name, f)
		}
		if f.Partition != "product" && f.Partition != "system_ext" || !strings.HasPrefix(f.Guest(), "/"+f.Partition+"/") {
			t.Errorf("%s: guest path %s", f.Name, f.Guest())
		}
		if lines[k] != f.Name+" "+f.Guest()+" "+f.SHA256 {
			t.Errorf("manifest line %d: %q", k, lines[k])
		}
		total += f.Size
	}
	if ExtractedBytes() != total || Files[0].Name != "Phonesky.apk" || Stub != "/product/app/LicenseChecker" {
		t.Errorf("total %d, first %s, stub %s", ExtractedBytes(), Files[0].Name, Stub)
	}
}

// TestExtractRealImage takes the store's files out of Google's own r14 system.img when MODAVD_TEST_PLAY_IMAGE names
// it (a developer check; the file is 3 GB and Google's).
func TestExtractRealImage(t *testing.T) {
	img := os.Getenv("MODAVD_TEST_PLAY_IMAGE")
	if img == "" {
		t.Skip("MODAVD_TEST_PLAY_IMAGE not set")
	}
	dir := testutil.TempDir(t)
	if err := Extract(img, Files, dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range Files {
		if st, err := os.Stat(filepath.Join(dir, f.Name)); err != nil || st.Size() != f.Size {
			t.Errorf("%s: %v", f.Name, err)
		}
	}
}
