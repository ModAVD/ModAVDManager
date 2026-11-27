package playstore

// Reading files out of an Android emulator system.img, read-only: a GPT disk whose "super" partition holds the
// dynamic (logical) partitions (liblp metadata), with ext4 or EROFS file systems. Only what these images use is
// supported: linear super extents, ext4 extents, and EROFS flat files or compact LZ4 clusters. Anything else is an
// error, never a guess; the callers check every file against its pinned size and SHA-256 anyway.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Extract writes files (taken from the system image img) into dir as dir\<Name>, each checked against its size and
// SHA-256 before it is renamed into place from a .part file. Files already there are rewritten. dir must exist.
func Extract(img string, files []File, dir string) error {
	f, err := os.Open(img)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	parts, err := dynamicPartitions(f, st.Size())
	if err != nil {
		return err
	}
	fss := map[string]imageFS{}
	for _, file := range files {
		fs := fss[file.Partition]
		if fs == nil {
			p, ok := parts[file.Partition]
			if !ok {
				return fmt.Errorf("镜像里没有 %s 分区", file.Partition)
			}
			if fs, err = openImageFS(p); err != nil {
				return fmt.Errorf("%s 分区：%w", file.Partition, err)
			}
			fss[file.Partition] = fs
		}
		if err := extractOne(fs, file, dir); err != nil {
			return fmt.Errorf("%s：%w", file.Guest(), err)
		}
	}
	return nil
}

// imageFS checks each requested file's type and pinned size before copying its bytes.
type imageFS interface {
	copyFile(path string, size int64, w io.Writer) (int64, error)
}

func openImageFS(p *partition) (imageFS, error) {
	magic := make([]byte, 4)
	if _, err := p.ReadAt(magic, 1024); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(magic) == erofsMagic {
		return openEROFS(p)
	}
	return openExt4(p)
}

func extractOne(fs imageFS, file File, dir string) error {
	dst := filepath.Join(dir, file.Name)
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = fs.copyFile(file.Path, file.Size, io.MultiWriter(out, h))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if got := hex.EncodeToString(h.Sum(nil)); got != file.SHA256 {
			err = fmt.Errorf("SHA-256 是 %s，应为 %s", got, file.SHA256)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ---- GPT and liblp ------------------------------------------------------------------------------------------------

const sector = 512

// gptPartition is the byte range of the GPT partition called name.
func gptPartition(r io.ReaderAt, size int64, name string) (off, length int64, err error) {
	h := make([]byte, 92)
	if _, err := r.ReadAt(h, sector); err != nil {
		return 0, 0, fmt.Errorf("读不了 GPT 头：%w", err)
	}
	if string(h[:8]) != "EFI PART" {
		return 0, 0, errors.New("不是 GPT 磁盘镜像")
	}
	entries := int64(binary.LittleEndian.Uint64(h[72:]))
	count := binary.LittleEndian.Uint32(h[80:])
	esize := binary.LittleEndian.Uint32(h[84:])
	if esize < 128 || esize > 4096 || count > 1024 {
		return 0, 0, errors.New("GPT 分区表不对")
	}
	table := make([]byte, int(count)*int(esize))
	if _, err := r.ReadAt(table, entries*sector); err != nil {
		return 0, 0, fmt.Errorf("读不了 GPT 分区表：%w", err)
	}
	for k := 0; k < int(count); k++ {
		e := table[k*int(esize):]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		u := make([]uint16, 36)
		for j := range u {
			u[j] = binary.LittleEndian.Uint16(e[56+2*j:])
		}
		if strings.TrimRight(string(utf16.Decode(u)), "\x00") != name {
			continue
		}
		first, last := int64(binary.LittleEndian.Uint64(e[32:])), int64(binary.LittleEndian.Uint64(e[40:]))
		off, length = first*sector, (last-first+1)*sector
		if first <= 0 || last < first || off+length > size {
			return 0, 0, fmt.Errorf("GPT 分区 %s 的范围不对", name)
		}
		return off, length, nil
	}
	return 0, 0, fmt.Errorf("GPT 里没有 %s 分区", name)
}

const (
	lpReserved      = 4096 // LP_PARTITION_RESERVED_BYTES
	lpGeometrySize  = 4096 // LP_METADATA_GEOMETRY_SIZE, twice (primary and backup)
	lpGeometryMagic = 0x616c4467
	lpHeaderMagic   = 0x414c5030
	lpTargetLinear  = 0
)

// extent is part of a logical partition: length bytes at off in the image, starting at byte at of the partition.
type extent struct{ at, off, length int64 }

// partition is a logical partition, readable as one range of bytes.
type partition struct {
	r       io.ReaderAt
	extents []extent
	size    int64
}

func (p *partition) ReadAt(b []byte, off int64) (int, error) {
	n := 0
	for n < len(b) {
		pos := off + int64(n)
		if pos >= p.size {
			return n, io.EOF
		}
		var e *extent
		for k := range p.extents {
			if x := &p.extents[k]; pos >= x.at && pos < x.at+x.length {
				e = x
				break
			}
		}
		if e == nil {
			return n, errors.New("分区里有没映射的范围")
		}
		chunk := min(int64(len(b)-n), e.at+e.length-pos)
		m, err := p.r.ReadAt(b[n:n+int(chunk)], e.off+pos-e.at)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// dynamicPartitions are the logical partitions of the image's super partition, by name, from slot 0 of the primary
// metadata (both checksums verified).
func dynamicPartitions(r io.ReaderAt, size int64) (map[string]*partition, error) {
	base, superLen, err := gptPartition(r, size, "super")
	if err != nil {
		return nil, err
	}
	g := make([]byte, 52)
	if _, err := r.ReadAt(g, base+lpReserved); err != nil {
		return nil, fmt.Errorf("读不了动态分区的几何信息：%w", err)
	}
	if binary.LittleEndian.Uint32(g) != lpGeometryMagic {
		return nil, errors.New("super 分区里没有动态分区信息")
	}
	gsize := binary.LittleEndian.Uint32(g[4:])
	if gsize < 52 || gsize > lpGeometrySize {
		return nil, errors.New("动态分区的几何信息不对")
	}
	gb := make([]byte, gsize)
	if _, err := r.ReadAt(gb, base+lpReserved); err != nil {
		return nil, err
	}
	if !checksumOK(gb, 8) {
		return nil, errors.New("动态分区的几何信息校验和不对")
	}
	metaMax := int64(binary.LittleEndian.Uint32(g[40:]))
	hdrAt := base + lpReserved + 2*lpGeometrySize
	h := make([]byte, 128)
	if _, err := r.ReadAt(h, hdrAt); err != nil {
		return nil, fmt.Errorf("读不了动态分区表：%w", err)
	}
	if binary.LittleEndian.Uint32(h) != lpHeaderMagic {
		return nil, errors.New("动态分区表的头不对")
	}
	hsize := int64(binary.LittleEndian.Uint32(h[8:]))
	tsize := int64(binary.LittleEndian.Uint32(h[44:]))
	if hsize < 128 || hsize > 4096 || tsize <= 0 || hsize+tsize > metaMax || metaMax > superLen {
		return nil, errors.New("动态分区表的大小不对")
	}
	hb := make([]byte, hsize)
	if _, err := r.ReadAt(hb, hdrAt); err != nil {
		return nil, err
	}
	if !checksumOK(hb, 12) {
		return nil, errors.New("动态分区表头的校验和不对")
	}
	t := make([]byte, tsize)
	if _, err := r.ReadAt(t, hdrAt+hsize); err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(t); !bytes.Equal(sum[:], hb[48:80]) {
		return nil, errors.New("动态分区表的校验和不对")
	}
	desc := func(at int) (off, n, esize int) {
		return int(binary.LittleEndian.Uint32(hb[at:])), int(binary.LittleEndian.Uint32(hb[at+4:])),
			int(binary.LittleEndian.Uint32(hb[at+8:]))
	}
	po, pn, pe := desc(80)
	eo, en, ee := desc(92)
	if pe < 52 || ee < 24 || po+pn*pe > len(t) || eo+en*ee > len(t) {
		return nil, errors.New("动态分区表的表项不对")
	}
	out := map[string]*partition{}
	for k := 0; k < pn; k++ {
		e := t[po+k*pe:]
		name := string(bytes.TrimRight(e[:36], "\x00"))
		first, num := int(binary.LittleEndian.Uint32(e[40:])), int(binary.LittleEndian.Uint32(e[44:]))
		if first+num > en {
			return nil, fmt.Errorf("动态分区 %s 的范围表不对", name)
		}
		p := &partition{r: r}
		for j := first; j < first+num; j++ {
			x := t[eo+j*ee:]
			sectors := int64(binary.LittleEndian.Uint64(x))
			typ := binary.LittleEndian.Uint32(x[8:])
			data := int64(binary.LittleEndian.Uint64(x[12:]))
			src := binary.LittleEndian.Uint32(x[20:])
			if typ != lpTargetLinear || src != 0 {
				return nil, fmt.Errorf("动态分区 %s 有不支持的范围（类型 %d，设备 %d）", name, typ, src)
			}
			if data < 0 || sectors <= 0 || (data+sectors)*sector > superLen {
				return nil, fmt.Errorf("动态分区 %s 的范围超出 super 分区", name)
			}
			p.extents = append(p.extents, extent{at: p.size, off: base + data*sector, length: sectors * sector})
			p.size += sectors * sector
		}
		out[name] = p
	}
	return out, nil
}

// checksumOK: the SHA-256 of b, with its 32 checksum bytes at at zeroed, is those bytes.
func checksumOK(b []byte, at int) bool {
	c := append([]byte(nil), b...)
	want := append([]byte(nil), c[at:at+32]...)
	clear(c[at : at+32])
	sum := sha256.Sum256(c)
	return bytes.Equal(sum[:], want)
}

// ---- ext4 ---------------------------------------------------------------------------------------------------------

const (
	ext4Magic        = 0xEF53
	incompatMetaBG   = 0x10
	incompat64Bit    = 0x80
	flagExtents      = 0x80000
	flagInlineData   = 0x10000000
	extentMagic      = 0xF30A
	maxExtentDepth   = 5
	maxDirBytes      = 16 << 20
	uninitExtentFlag = 32768
)

type ext4 struct {
	r          io.ReaderAt
	size       int64
	blockSize  int64
	inodeSize  int64
	perGroup   uint32
	groups     uint32
	descSize   int64
	descTable  int64 // byte offset of the group descriptor table
	is64       bool
	inodeCount uint32
}

type inode struct {
	mode  uint16
	size  int64
	flags uint32
	block []byte // i_block, 60 bytes
}

func openExt4(p *partition) (*ext4, error) {
	sb := make([]byte, 1024)
	if _, err := p.ReadAt(sb, 1024); err != nil {
		return nil, fmt.Errorf("读不了超级块：%w", err)
	}
	if binary.LittleEndian.Uint16(sb[0x38:]) != ext4Magic {
		return nil, errors.New("不是 ext4 文件系统")
	}
	logBS := binary.LittleEndian.Uint32(sb[0x18:])
	if logBS > 6 {
		return nil, errors.New("块大小不对")
	}
	fs := &ext4{r: p, size: p.size, blockSize: 1024 << logBS}
	fs.inodeCount = binary.LittleEndian.Uint32(sb[0x00:])
	blocks := uint64(binary.LittleEndian.Uint32(sb[0x04:]))
	perGroupBlocks := binary.LittleEndian.Uint32(sb[0x20:])
	fs.perGroup = binary.LittleEndian.Uint32(sb[0x28:])
	fs.inodeSize = 128
	if binary.LittleEndian.Uint32(sb[0x4C:]) >= 1 {
		fs.inodeSize = int64(binary.LittleEndian.Uint16(sb[0x58:]))
	}
	incompat := binary.LittleEndian.Uint32(sb[0x60:])
	if incompat&incompatMetaBG != 0 {
		return nil, errors.New("不支持 meta_bg")
	}
	fs.is64 = incompat&incompat64Bit != 0
	fs.descSize = 32
	if fs.is64 {
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:])) << 32
		fs.descSize = int64(binary.LittleEndian.Uint16(sb[0xFE:]))
	}
	firstData := int64(binary.LittleEndian.Uint32(sb[0x14:]))
	if perGroupBlocks == 0 || fs.perGroup == 0 || fs.inodeSize < 128 || fs.inodeSize > fs.blockSize ||
		fs.descSize < 32 || int64(blocks)*fs.blockSize > p.size+fs.blockSize {
		return nil, errors.New("超级块的参数不对")
	}
	fs.groups = uint32((blocks - uint64(firstData) + uint64(perGroupBlocks) - 1) / uint64(perGroupBlocks))
	fs.descTable = (firstData + 1) * fs.blockSize
	return fs, nil
}

func (fs *ext4) copyFile(path string, size int64, w io.Writer) (int64, error) {
	ino, err := fs.lookup(path)
	if err != nil {
		return 0, err
	}
	in, err := fs.inode(ino)
	if err != nil {
		return 0, err
	}
	if in.mode&0xF000 != 0x8000 {
		return 0, errors.New("不是普通文件")
	}
	if in.size != size {
		return 0, fmt.Errorf("大小是 %d，应为 %d", in.size, size)
	}
	return fs.copyData(in, w)
}

func (fs *ext4) read(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > fs.size {
		return nil, errors.New("读的范围超出文件系统")
	}
	b := make([]byte, n)
	if _, err := fs.r.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

func (fs *ext4) inode(ino uint32) (*inode, error) {
	if ino == 0 || ino > fs.inodeCount {
		return nil, fmt.Errorf("inode %d 不存在", ino)
	}
	g, idx := (ino-1)/fs.perGroup, (ino-1)%fs.perGroup
	if g >= fs.groups {
		return nil, fmt.Errorf("inode %d 不在任何块组里", ino)
	}
	d, err := fs.read(fs.descTable+int64(g)*fs.descSize, fs.descSize)
	if err != nil {
		return nil, err
	}
	table := int64(binary.LittleEndian.Uint32(d[0x08:]))
	if fs.is64 && fs.descSize >= 0x2C {
		table |= int64(binary.LittleEndian.Uint32(d[0x28:])) << 32
	}
	b, err := fs.read(table*fs.blockSize+int64(idx)*fs.inodeSize, 128)
	if err != nil {
		return nil, err
	}
	in := &inode{
		mode:  binary.LittleEndian.Uint16(b[0x00:]),
		size:  int64(binary.LittleEndian.Uint32(b[0x04:])) | int64(binary.LittleEndian.Uint32(b[0x6C:]))<<32,
		flags: binary.LittleEndian.Uint32(b[0x20:]),
		block: b[0x28 : 0x28+60],
	}
	if in.size < 0 {
		return nil, fmt.Errorf("inode %d 的大小不对", ino)
	}
	return in, nil
}

// leaf is one mapped run of a file: count blocks from logical block logical at physical block physical; zero for an
// uninitialized extent (reads as zeros).
type leaf struct {
	logical, physical, count int64
	zero                     bool
}

// leaves are the extents of an inode, in the order of the tree.
func (fs *ext4) leaves(in *inode) ([]leaf, error) {
	if in.flags&flagInlineData != 0 {
		return nil, errors.New("不支持内联数据")
	}
	if in.flags&flagExtents == 0 {
		return nil, errors.New("不支持不用 extents 的文件")
	}
	var out []leaf
	var walk func(node []byte, depth int) error
	walk = func(node []byte, depth int) error {
		if len(node) < 12 || binary.LittleEndian.Uint16(node) != extentMagic {
			return errors.New("extent 头不对")
		}
		entries := int(binary.LittleEndian.Uint16(node[2:]))
		d := int(binary.LittleEndian.Uint16(node[6:]))
		if d != depth || 12+entries*12 > len(node) {
			return errors.New("extent 树不对")
		}
		for k := 0; k < entries; k++ {
			e := node[12+k*12:]
			if d == 0 {
				n := int64(binary.LittleEndian.Uint16(e[4:]))
				zero := n > uninitExtentFlag
				if zero {
					n -= uninitExtentFlag
				}
				phys := int64(binary.LittleEndian.Uint16(e[6:]))<<32 | int64(binary.LittleEndian.Uint32(e[8:]))
				out = append(out, leaf{int64(binary.LittleEndian.Uint32(e)), phys, n, zero})
				continue
			}
			child := int64(binary.LittleEndian.Uint16(e[8:]))<<32 | int64(binary.LittleEndian.Uint32(e[4:]))
			b, err := fs.read(child*fs.blockSize, fs.blockSize)
			if err != nil {
				return err
			}
			if err := walk(b, d-1); err != nil {
				return err
			}
		}
		return nil
	}
	if len(in.block) < 12 {
		return nil, errors.New("extent 头不对")
	}
	depth := int(binary.LittleEndian.Uint16(in.block[6:]))
	if depth > maxExtentDepth {
		return nil, errors.New("extent 树太深")
	}
	return out, walk(in.block, depth)
}

// copyData writes the inode's in.size bytes to w (holes and uninitialized extents as zeros).
func (fs *ext4) copyData(in *inode, w io.Writer) (int64, error) {
	runs, err := fs.leaves(in)
	if err != nil {
		return 0, err
	}
	var done int64
	zeros := make([]byte, fs.blockSize)
	writeZeros := func(n int64) error {
		for n > 0 {
			c := min(n, int64(len(zeros)))
			if _, err := w.Write(zeros[:c]); err != nil {
				return err
			}
			n -= c
			done += c
		}
		return nil
	}
	buf := make([]byte, 1<<20)
	for _, run := range runs {
		start := run.logical * fs.blockSize
		if start < done {
			return done, errors.New("extent 重叠")
		}
		if start >= in.size {
			continue
		}
		if err := writeZeros(start - done); err != nil {
			return done, err
		}
		n := min(run.count*fs.blockSize, in.size-start)
		if run.zero {
			if err := writeZeros(n); err != nil {
				return done, err
			}
			continue
		}
		off := run.physical * fs.blockSize
		if off < 0 || off+n > fs.size {
			return done, errors.New("extent 超出文件系统")
		}
		for n > 0 {
			c := min(n, int64(len(buf)))
			if _, err := fs.r.ReadAt(buf[:c], off); err != nil {
				return done, err
			}
			if _, err := w.Write(buf[:c]); err != nil {
				return done, err
			}
			off, n, done = off+c, n-c, done+c
		}
	}
	return done, writeZeros(in.size - done)
}

// lookup is the inode of path (relative to the root, "/"-separated); symlinks are not followed.
func (fs *ext4) lookup(p string) (uint32, error) {
	ino := uint32(2)
	for _, name := range strings.Split(strings.Trim(p, "/"), "/") {
		if name == "" || name == "." || name == ".." {
			return 0, fmt.Errorf("路径不对：%s", p)
		}
		in, err := fs.inode(ino)
		if err != nil {
			return 0, err
		}
		if in.mode&0xF000 != 0x4000 {
			return 0, fmt.Errorf("%s 不在目录里", name)
		}
		if in.size > maxDirBytes {
			return 0, errors.New("目录太大")
		}
		var b bytes.Buffer
		if _, err := fs.copyData(in, &b); err != nil {
			return 0, err
		}
		next, err := findEntry(b.Bytes(), name)
		if err != nil {
			return 0, err
		}
		ino = next
	}
	return ino, nil
}

// findEntry is the inode of name in a directory's data (linear entries; htree nodes and checksum tails have inode 0
// and are skipped).
func findEntry(d []byte, name string) (uint32, error) {
	for pos := 0; pos+8 <= len(d); {
		ino := binary.LittleEndian.Uint32(d[pos:])
		rec := int(binary.LittleEndian.Uint16(d[pos+4:]))
		nl := int(d[pos+6])
		if rec < 8 || pos+rec > len(d) || 8+nl > rec {
			return 0, errors.New("目录项不对")
		}
		if ino != 0 && string(d[pos+8:pos+8+nl]) == name {
			return ino, nil
		}
		pos += rec
	}
	return 0, fmt.Errorf("没有 %s", name)
}
