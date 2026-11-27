package playstore

// Read the ARM64 Google Play image's EROFS flat files and compact LZ4 files.
// Other compression algorithms, chunk files, fragments and external devices are unsupported.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

const erofsMagic = 0xE0F5E1E2

type erofs struct {
	r           io.ReaderAt
	size        int64
	blockSize   int64
	metaOff     int64
	root        uint64
	zeroPadding bool
	maxPcluster int64
	bigPcluster bool
}

type erofsInode struct {
	mode      uint16
	size      int64
	dataOff   int64
	inlineOff int64 // -1 for plain files; otherwise the tail's metadata offset
	layout    uint16
	metaEnd   int64
}

func openEROFS(p *partition) (*erofs, error) {
	sb := make([]byte, 128)
	if _, err := p.ReadAt(sb, 1024); err != nil {
		return nil, fmt.Errorf("读不了 EROFS 超级块：%w", err)
	}
	if binary.LittleEndian.Uint32(sb) != erofsMagic {
		return nil, errors.New("不是 EROFS 文件系统")
	}
	// New addressing, external devices, chunks, fragments and metaboxes
	// change the addressing rules and require another reader.
	if binary.LittleEndian.Uint32(sb[80:])&^uint32(3) != 0 ||
		binary.LittleEndian.Uint16(sb[86:]) != 0 || sb[90] != 0 {
		return nil, errors.New("不支持 EROFS 的文件系统特性")
	}
	if sb[12] < 10 || sb[12] > 16 {
		return nil, errors.New("EROFS 块大小不对")
	}
	fs := &erofs{r: p, blockSize: 1 << sb[12], root: uint64(binary.LittleEndian.Uint16(sb[14:]))}
	fs.size = int64(binary.LittleEndian.Uint32(sb[36:])) * fs.blockSize
	fs.metaOff = int64(binary.LittleEndian.Uint32(sb[40:])) * fs.blockSize
	if fs.size > p.size || fs.size < fs.blockSize || fs.metaOff >= fs.size ||
		128+int64(sb[13])*16 > fs.blockSize-1024 {
		return nil, errors.New("EROFS 超级块的范围不对")
	}
	if binary.LittleEndian.Uint32(sb[8:])&1 != 0 {
		b, err := fs.read(1024, fs.blockSize-1024)
		if err != nil {
			return nil, err
		}
		want := binary.LittleEndian.Uint32(b[4:])
		clear(b[4:8])
		if ^crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)) != want {
			return nil, errors.New("EROFS 超级块的校验和不对")
		}
	}
	fs.zeroPadding = binary.LittleEndian.Uint32(sb[80:])&1 != 0
	fs.bigPcluster = binary.LittleEndian.Uint32(sb[80:])&2 != 0
	fs.maxPcluster = 1
	if fs.bigPcluster {
		if binary.LittleEndian.Uint16(sb[84:]) != 1 {
			return nil, errors.New("EROFS 只支持 LZ4 压缩")
		}
		at := int64(1024 + 128 + int(sb[13])*16)
		h, err := fs.read(at, 2)
		if err != nil {
			return nil, err
		}
		n := int64(binary.LittleEndian.Uint16(h))
		if n < 14 {
			return nil, errors.New("EROFS LZ4 配置太短")
		}
		cfg, err := fs.read(at+2, n)
		if err != nil {
			return nil, err
		}
		fs.maxPcluster = max(1, int64(binary.LittleEndian.Uint16(cfg[2:])))
		if fs.maxPcluster > (1<<20)/fs.blockSize {
			return nil, errors.New("EROFS 压缩块太大")
		}
	}
	return fs, nil
}

func (fs *erofs) read(off, size int64) ([]byte, error) {
	if off < 0 || size < 0 || off > fs.size || size > fs.size-off {
		return nil, errors.New("读的范围超出 EROFS 文件系统")
	}
	b := make([]byte, size)
	if _, err := fs.r.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

func (fs *erofs) inode(nid uint64) (*erofsInode, error) {
	if nid > uint64((fs.size-fs.metaOff-32)/32) {
		return nil, fmt.Errorf("EROFS nid %d 超出文件系统", nid)
	}
	off := fs.metaOff + int64(nid)*32
	b, err := fs.read(off, 32)
	if err != nil {
		return nil, err
	}
	format := binary.LittleEndian.Uint16(b)
	layout := (format >> 1) & 7
	if format&^uint16(15) != 0 || (layout != 0 && layout != 2 && layout != 3) {
		return nil, fmt.Errorf("不支持 EROFS inode 格式 %#x", format)
	}
	inodeSize := int64(32)
	if format&1 != 0 {
		inodeSize = 64
		b, err = fs.read(off, inodeSize)
		if err != nil {
			return nil, err
		}
	}
	size := uint64(binary.LittleEndian.Uint32(b[8:]))
	if inodeSize == 64 {
		size = binary.LittleEndian.Uint64(b[8:])
	}
	if size > 1<<63-1 || layout != 3 && size > uint64(fs.size) {
		return nil, errors.New("EROFS 文件大小不对")
	}
	xattrSize := int64(0)
	if n := int64(binary.LittleEndian.Uint16(b[2:])); n != 0 {
		xattrSize = 12 + (n-1)*4
	}
	metaEnd := off + inodeSize + xattrSize
	if metaEnd > fs.size {
		return nil, errors.New("EROFS inode 元数据超出文件系统")
	}
	in := &erofsInode{
		mode:      binary.LittleEndian.Uint16(b[4:]),
		size:      int64(size),
		dataOff:   int64(binary.LittleEndian.Uint32(b[16:])) * fs.blockSize,
		inlineOff: -1,
		layout:    layout,
		metaEnd:   metaEnd,
	}
	if layout == 3 {
		return in, nil
	}
	dataSize := in.size
	if layout == 2 {
		in.inlineOff = metaEnd
		tail := in.size % fs.blockSize
		if tail > fs.blockSize-metaEnd%fs.blockSize || tail > fs.size-metaEnd {
			return nil, errors.New("EROFS 内联数据超出元数据块")
		}
		dataSize -= tail
	}
	if dataSize > 0 && (in.dataOff > fs.size || dataSize > fs.size-in.dataOff) {
		return nil, errors.New("EROFS 文件数据超出文件系统")
	}
	return in, nil
}

func (fs *erofs) copyData(in *erofsInode, w io.Writer) (int64, error) {
	if in.layout == 3 {
		return fs.copyCompressed(in, w)
	}
	dataSize := in.size
	if in.inlineOff >= 0 {
		dataSize -= in.size % fs.blockSize
	}
	n, err := io.Copy(w, io.NewSectionReader(fs.r, in.dataOff, dataSize))
	if err != nil || n != dataSize {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	if dataSize == in.size {
		return n, nil
	}
	m, err := io.Copy(w, io.NewSectionReader(fs.r, in.inlineOff, in.size-dataSize))
	if err == nil && m != in.size-dataSize {
		err = io.ErrUnexpectedEOF
	}
	return n + m, err
}

func (fs *erofs) copyFile(path string, size int64, w io.Writer) (int64, error) {
	nid, err := fs.lookup(path)
	if err != nil {
		return 0, err
	}
	in, err := fs.inode(nid)
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

// lookup walks directories only; symlinks are never followed.
func (fs *erofs) lookup(path string) (uint64, error) {
	nid := fs.root
	for _, name := range strings.Split(strings.Trim(path, "/"), "/") {
		if name == "" || name == "." || name == ".." || len(name) > 255 {
			return 0, fmt.Errorf("路径不对：%s", path)
		}
		in, err := fs.inode(nid)
		if err != nil {
			return 0, err
		}
		if in.mode&0xF000 != 0x4000 {
			return 0, fmt.Errorf("%s 不在目录里", name)
		}
		if in.size > maxDirBytes {
			return 0, errors.New("EROFS 目录太大")
		}
		var b bytes.Buffer
		if _, err := fs.copyData(in, &b); err != nil {
			return 0, err
		}
		next, err := fs.findEntry(b.Bytes(), name)
		if err != nil {
			return 0, err
		}
		nid = next
	}
	return nid, nil
}

func (fs *erofs) findEntry(dir []byte, name string) (uint64, error) {
	for off := 0; off < len(dir); off += int(fs.blockSize) {
		b := dir[off:min(off+int(fs.blockSize), len(dir))]
		if len(b) < 12 {
			return 0, errors.New("EROFS 目录块太短")
		}
		end := int(binary.LittleEndian.Uint16(b[8:]))
		if end < 12 || end%12 != 0 || end > len(b) {
			return 0, errors.New("EROFS 目录项范围不对")
		}
		for pos := 0; pos < end; pos += 12 {
			start := int(binary.LittleEndian.Uint16(b[pos+8:]))
			next := len(b)
			if pos+12 < end {
				next = int(binary.LittleEndian.Uint16(b[pos+20:]))
			}
			if start < end || next <= start || next > len(b) {
				return 0, errors.New("EROFS 目录项名称范围不对")
			}
			n := b[start:next]
			if pos+12 == end {
				if zero := bytes.IndexByte(n, 0); zero >= 0 {
					n = n[:zero]
				}
			}
			if len(n) == 0 || len(n) > 255 || bytes.IndexByte(n, 0) >= 0 || bytes.IndexByte(n, '/') >= 0 {
				return 0, errors.New("EROFS 目录项名称不对")
			}
			if string(n) == name {
				return binary.LittleEndian.Uint64(b[pos:]), nil
			}
		}
	}
	return 0, fmt.Errorf("没有 %s", name)
}

// Each compact index pack has either two 16-bit entries or sixteen 14-bit
// entries followed by a physical-block base. Non-head entries encode distances
// or, immediately after a head, the number of physical blocks in that cluster.
type erofsIndex struct {
	typ   uint32
	lo    uint32
	block int64
}

type erofsCluster struct {
	logical int64
	block   int64
	blocks  int64
	plain   bool
}

func (fs *erofs) compressedClusters(in *erofsInode) ([]erofsCluster, error) {
	at := (in.metaEnd + 7) &^ 7
	h, err := fs.read(at, 8)
	if err != nil {
		return nil, err
	}
	advise := binary.LittleEndian.Uint16(h[4:])
	// The pinned image uses 4 KiB logical clusters, LZ4 for both head types,
	// and optional 2-byte indexes/big clusters, without compressed tail packing.
	if fs.blockSize != 4096 || !fs.zeroPadding || binary.LittleEndian.Uint32(h) != 0 ||
		h[6] != 0 || h[7] != 0 || advise&^uint16(7) != 0 || (advise>>1)&3 != 0 && (advise>>1)&3 != 3 {
		return nil, errors.New("不支持 EROFS 压缩 inode 的布局或算法")
	}
	big := advise&2 != 0
	if big && !fs.bigPcluster {
		return nil, errors.New("EROFS 压缩块缺少文件系统特性")
	}
	base := at + 8
	total := in.size / fs.blockSize
	if in.size%fs.blockSize != 0 {
		total++
	}
	initial := ((32 - base%32) % 32) / 4
	compact := int64(0)
	if advise&1 != 0 && total > initial {
		compact = ((total - initial) / 16) * 16
	}
	load := func(lcn int64) (erofsIndex, error) {
		pos, width, count := base+lcn*4, int64(4), int64(2)
		if lcn >= initial {
			if lcn < initial+compact {
				pos, width, count = base+initial*4+(lcn-initial)*2, 2, 16
			} else {
				pos = base + initial*4 + compact*2 + (lcn-initial-compact)*4
			}
		}
		packSize := width * count
		packOff := pos - pos%packSize
		b, err := fs.read(packOff, packSize)
		if err != nil {
			return erofsIndex{}, err
		}
		i := (pos - packOff) / width
		bits := (packSize - 4) * 8 / count
		decode := func(j int64) (typ, lo uint32) {
			bit := j * bits
			v := binary.LittleEndian.Uint32(b[bit/8:]) >> (bit % 8)
			return (v >> 12) & 3, v & 4095
		}
		typ, lo := decode(i)
		if typ == 2 {
			// The pack's final non-head stores a forward distance; its
			// backward distance is one plus that of its predecessor.
			if i == count-1 && lo&2048 == 0 {
				pt, pl := decode(i - 1)
				lo = 1
				if pt == 2 {
					lo = pl + 1
					if pl&2048 != 0 {
						lo = 2
					}
				}
			}
			return erofsIndex{typ: typ, lo: lo}, nil
		}
		block := int64(binary.LittleEndian.Uint32(b[packSize-4:]))
		if !big {
			block++ // legacy packs store the block preceding their first head
		}
		// Count physical blocks represented by earlier heads in this pack.
		for j := i - 1; j >= 0; j-- {
			pt, pl := decode(j)
			if pt != 2 {
				block++
				continue
			}
			if big {
				if pl&2048 != 0 {
					block += int64(pl &^ 2048)
					j-- // that count also accounts for its preceding head
				} else {
					if pl <= 1 {
						return erofsIndex{}, errors.New("EROFS 压缩块距离不对")
					}
					j -= int64(pl) - 2
				}
			} else {
				j -= int64(pl)
				if j >= 0 {
					block++
				}
			}
		}
		return erofsIndex{typ, lo, block}, nil
	}
	var clusters []erofsCluster
	var headLCN int64
	for lcn := int64(0); lcn < total; lcn++ {
		x, err := load(lcn)
		if err != nil {
			return nil, err
		}
		if x.typ == 2 {
			if len(clusters) == 0 {
				return nil, errors.New("EROFS 压缩索引缺少起始块")
			}
			distance := lcn - headLCN
			if x.lo&2048 != 0 {
				blocks := int64(x.lo &^ 2048)
				if !big || distance != 1 || blocks == 0 || blocks > fs.maxPcluster {
					return nil, errors.New("EROFS 压缩块数量不对")
				}
				clusters[len(clusters)-1].blocks = blocks
			} else if int64(x.lo) != distance || big && distance == 1 {
				return nil, errors.New("EROFS 压缩索引距离不对")
			}
			continue
		}
		logical := lcn*fs.blockSize + int64(x.lo)
		// A final head exactly at EOF terminates the preceding cluster.
		if logical == in.size && lcn == total-1 && len(clusters) > 0 {
			break
		}
		if logical >= in.size || len(clusters) == 0 && logical != 0 ||
			len(clusters) != 0 && logical <= clusters[len(clusters)-1].logical {
			return nil, fmt.Errorf("EROFS 压缩索引的文件偏移不对（簇 %d，偏移 %d，大小 %d）", lcn, logical, in.size)
		}
		clusters = append(clusters, erofsCluster{logical, x.block, 1, x.typ == 0})
		headLCN = lcn
	}
	return clusters, nil
}

func (fs *erofs) copyCompressed(in *erofsInode, w io.Writer) (int64, error) {
	clusters, err := fs.compressedClusters(in)
	if err != nil {
		return 0, err
	}
	var done int64
	for i, c := range clusters {
		end := in.size
		if i+1 < len(clusters) {
			end = clusters[i+1].logical
		}
		length := end - c.logical
		if c.logical != done || length <= 0 || length > 12<<20 || c.blocks > fs.maxPcluster {
			return done, errors.New("EROFS 压缩簇范围不对")
		}
		b, err := fs.read(c.block*fs.blockSize, c.blocks*fs.blockSize)
		if err != nil {
			return done, err
		}
		if c.plain {
			if length > fs.blockSize {
				return done, errors.New("EROFS 普通压缩簇太大")
			}
			b = b[:length]
		} else {
			padding := 0
			for padding < min(len(b), int(fs.blockSize)) && b[padding] == 0 {
				padding++
			}
			if padding == int(fs.blockSize) {
				return done, errors.New("EROFS 压缩簇没有数据")
			}
			b, err = decodeEROFSBlock(b[padding:], int(length))
			if err != nil {
				return done, err
			}
		}
		n, err := w.Write(b)
		done += int64(n)
		if err != nil {
			return done, err
		}
		if n != len(b) {
			return done, io.ErrShortWrite
		}
	}
	if done != in.size {
		return done, io.ErrUnexpectedEOF
	}
	return done, nil
}

// decodeEROFSBlock decodes a raw LZ4 block into exactly size bytes, with bounds
// checked before every read, copy and overlapping match. No frame/dictionary is used.
func decodeEROFSBlock(src []byte, size int) ([]byte, error) {
	if size <= 0 || size > 12<<20 || len(src) > 1<<20 {
		return nil, errors.New("LZ4 块大小不对")
	}
	dst := make([]byte, size)
	ip, op := 0, 0
	length := func(n int) (int, error) {
		if n == 15 {
			for {
				if ip >= len(src) {
					return 0, io.ErrUnexpectedEOF
				}
				b := int(src[ip])
				ip++
				if n > size-b {
					return 0, errors.New("LZ4 长度超出输出范围")
				}
				n += b
				if b != 255 {
					break
				}
			}
		}
		return n, nil
	}
	for ip < len(src) {
		token := src[ip]
		ip++
		n, err := length(int(token >> 4))
		if err != nil {
			return nil, err
		}
		if n > len(src)-ip || n > size-op {
			return nil, errors.New("LZ4 原文长度不对")
		}
		copy(dst[op:op+n], src[ip:ip+n])
		ip, op = ip+n, op+n
		if ip == len(src) {
			if op != size {
				return nil, errors.New("LZ4 输出大小不对")
			}
			return dst, nil
		}
		if len(src)-ip < 2 {
			return nil, io.ErrUnexpectedEOF
		}
		distance := int(binary.LittleEndian.Uint16(src[ip:]))
		ip += 2
		n, err = length(int(token & 15))
		if err != nil {
			return nil, err
		}
		n += 4
		if distance == 0 || distance > op || n > size-op {
			return nil, errors.New("LZ4 匹配范围不对")
		}
		for end := op + n; op < end; op++ {
			dst[op] = dst[op-distance]
		}
	}
	return nil, io.ErrUnexpectedEOF
}
