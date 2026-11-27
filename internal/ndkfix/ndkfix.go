// Package ndkfix builds the patched ARM translation layer from the image's own /system/lib64/libndk_translation.so
// (Google's android-34 google_apis x86_64 r14 image, ndk_translation 0.2.3) on the user's machine; the patched file is
// never distributed (ModAVD/ModAVD#131).
//
// The library has three AArch64 decoders (heavy optimizer, light translator, interpreter; the interpreter's is inlined
// into InterpretInsn), and all three decode HINT (op0=00, CRn=0010) the same way, on CRm:op2 = insn[11:5]:
//
//	if CRm:op2 >= 6               -> NOP path       (ja  rel32)
//	if (CRm:op2 & ~1) - 2 <= 3    -> UndefinedInsn  (jbe rel32)   CRm:op2 = 2, 3, 4, 5
//	                              -> NOP path       (jmp rel32)   CRm:op2 = 0, 1
//
// So NOP, YIELD and every hint from 6 up (BTI, PAC*, CSDB, unallocated ones) already run as NOP, while WFE (2), WFI (3),
// SEV (4) and SEVL (5) reach UndefinedInsn, which logs "Undefined instruction 0x%08x at 0x%016zx" and raises SIGILL.
// ARMv8 executes an unimplemented HINT as a NOP, and Linux lets EL0 run all four: Unity 6's spin locks do (World of
// Airports 3.7.3 died on SEVL, 0xd50320bf). Each decoder's 6-byte `jbe` becomes a 6-byte NOP, so the four fall
// through into the `jmp` NOP and YIELD take: the translators emit nothing, the interpreter moves pc on by 4.
//
// WFE and WFI may return at once (spurious wake-ups; software loops on its own condition), and SEV / SEVL only set the
// event register that WFE reads, so as NOPs the only visible difference is that a WFE wait spins.
//
// The edit is in place and of the same length: build-id, dynamic symbols, relocations, GOT and data stay as they are.
// The execution bridge's companion depends on that: it turns on its fork locks by this build-id, at fixed .bss
// offsets, and finds the translator's exports by name.
package ndkfix

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"modavd/internal/errs"
)

const (
	// The image's library (API 34 google_apis x86_64 r14, 2400920 bytes, build-id BuildID) and the patched one.
	OrigSHA256 = "a2086fbc807f697f9b91222065c9d9000ca9727748699c0d9b751319258cbb3e"
	FixSHA256  = "3efa8977dbc8c96c9de790ea6e88b9dcd2ccdc18fcec8545f8927e6636a040ee"
	BuildID    = "553ba82b76bb5a11fd1d67b2c7e01b75"
	// Where the guest library lives, and where the host uploads the patched copy for libfix.sh.
	GuestPath  = "/system/lib64/libndk_translation.so"
	UploadPath = "/data/local/tmp/ndkfix-upload.so"
	// File name of the cached patched library in <root>\guest-patches and in the guest's /data/local/tmp. A new patch
	// gets a new name.
	CacheName = "libndk_translation.hintnop1.so"
)

// nop6 is nopw 0x0(%rax,%rax,1), the length of a jbe rel32.
var nop6 = mustHex("66 0f 1f 44 00 00")

// site is one decoder's HINT block: from `and $0xfc0` (CRm:op2 >> 1) through the jmp to the NOP path.
type site struct {
	name  string
	va    int    // virtual address of the block
	block []byte // its bytes in the image's library
	jbe   int    // offset of the `jbe UndefinedInsn` in block
}

var sites = []site{
	{"Decoder<SemanticsPlayer<HeavyOptimizerFrontend>>::DecodeSystemClass", 0xd9a96,
		mustHex("89 c1 81 e1 c0 0f 00 00 81 f9 bf 00 00 00 0f 87 c2 01 00 00 c1 e8 05 83 e0 7e 83 c0 fe" +
			" 83 f8 03 0f 86 42 01 00 00 e9 ab 01 00 00"), 32},
	{"Decoder<SemanticsPlayer<LightTranslator>>::DecodeSystemClass", 0x140744,
		mustHex("89 c1 81 e1 c0 0f 00 00 81 f9 bf 00 00 00 0f 87 ad 01 00 00 c1 e8 05 83 e0 7e 83 c0 fe" +
			" 83 f8 03 0f 86 33 01 00 00 e9 96 01 00 00"), 32},
	{"InterpretInsn（内联的 Decoder<SemanticsPlayer<Interpreter>>::DecodeSystemClass）", 0x1d44ca,
		mustHex("89 d8 25 c0 0f 00 00 3d bf 00 00 00 0f 87 14 14 00 00 c1 eb 05 83 e3 7e 83 c3 fe 83 fb 03" +
			" 0f 86 f5 13 00 00 e9 fd 13 00 00"), 30},
}

// mustHex is bytes.fromhex: whitespace between the byte pairs is ignored.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic("ndkfix: bad hex constant " + s)
	}
	return b
}

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Patch returns the patched library. errs.Unsupported when the input's hash, any original byte sequence, a branch
// target or the result's hash does not match; the input is never changed.
func Patch(original []byte) ([]byte, error) {
	if digest := SHA256Hex(original); digest != OrigSHA256 {
		return nil, errs.Newf(errs.Unsupported, "ARM 转译层库 sha256 %s 不是预期版本", digest)
	}
	data := bytes.Clone(original)
	if err := apply(data); err != nil {
		return nil, err
	}
	if digest := SHA256Hex(data); digest != FixSHA256 {
		return nil, errs.Newf(errs.Unsupported, "改完的 ARM 转译层库 sha256 %s 不是预期值", digest)
	}
	return data, nil
}

// apply checks every site of data against the image's bytes and replaces its jbe with nop6. Nothing is written
// unless all sites check out.
func apply(data []byte) error {
	offs := make([]int, len(sites))
	for k, s := range sites {
		off, err := fileOffset(data, s.va, len(s.block))
		if err != nil {
			return errs.Newf(errs.Unsupported, "%s（0x%x）: %v", s.name, s.va, err)
		}
		if got := data[off : off+len(s.block)]; !bytes.Equal(got, s.block) {
			return errs.Newf(errs.Unsupported, "%s（0x%x）原字节不符: %s", s.name, s.va, hex.EncodeToString(got))
		}
		// The jmp the four hints fall through to after the edit must be the NOP path, where the ja of CRm:op2 >= 6 goes.
		ja, jmp, err := s.targets()
		if err != nil {
			return err
		}
		if ja != jmp {
			return errs.Newf(errs.Unsupported, "%s（0x%x）: 落下去的 jmp 去 0x%x，不是 NOP 出口 0x%x", s.name, s.va, jmp, ja)
		}
		offs[k] = off
	}
	for k, s := range sites {
		copy(data[offs[k]+s.jbe:], nop6)
	}
	return nil
}

// targets reads the block's branches: where the ja (CRm:op2 >= 6) and the jmp after the jbe go.
func (s site) targets() (ja, jmp int, err error) {
	at := bytes.Index(s.block, []byte{0x0f, 0x87})
	if at < 0 || at+6 > s.jbe || !bytes.HasPrefix(s.block[s.jbe:], []byte{0x0f, 0x86}) ||
		len(s.block) != s.jbe+6+5 || s.block[s.jbe+6] != 0xe9 {
		return 0, 0, errs.Newf(errs.Unsupported, "%s: 补丁点的指令不是 ja / jbe / jmp", s.name)
	}
	rel := func(p int) int { return int(int32(binary.LittleEndian.Uint32(s.block[p:]))) }
	ja = s.va + at + 6 + rel(at+2)
	jmp = s.va + s.jbe + 6 + 5 + rel(s.jbe+7)
	return ja, jmp, nil
}

// fileOffset maps the virtual address range [va, va+n) to a file offset through the executable PT_LOAD that holds
// it in the file.
func fileOffset(data []byte, va, n int) (int, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Flags&elf.PF_X == 0 {
			continue
		}
		start, end := p.Vaddr, p.Vaddr+p.Filesz
		if uint64(va) >= start && uint64(va+n) <= end {
			off := int(p.Off + uint64(va) - start)
			if off < 0 || off+n > len(data) {
				break
			}
			return off, nil
		}
	}
	return 0, errs.Newf(errs.Unsupported, "不在可执行段里")
}
