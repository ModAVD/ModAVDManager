package ndkfix

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// The real libraries (read-only) in $MODAVD_TEST_NDKLIBS: the image's libndk_translation.so and the patched copy,
// libndk_translation.hintnop1.so (the Python reference make_sevl_patch.py made it). Copies of the image's file, never
// a production root. The tests that need them are skipped when they are absent.
var (
	origLib = filepath.Join(os.Getenv("MODAVD_TEST_NDKLIBS"), "libndk_translation.so")
	fixLib  = filepath.Join(os.Getenv("MODAVD_TEST_NDKLIBS"), CacheName)
)

func readOrSkip(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("%s 不存在", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jbeRanges are the file ranges Patch may change: the jbe of each site.
func jbeRanges(t *testing.T, data []byte) [][2]int {
	t.Helper()
	var r [][2]int
	for _, s := range sites {
		off, err := fileOffset(data, s.va, len(s.block))
		if err != nil {
			t.Fatal(err)
		}
		r = append(r, [2]int{off + s.jbe, off + s.jbe + 6})
	}
	return r
}

func buildID(t *testing.T, data []byte) string {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sec := f.Section(".note.gnu.build-id")
	if sec == nil {
		t.Fatal("no .note.gnu.build-id")
	}
	b, err := sec.Data()
	if err != nil || len(b) < 16+16 {
		t.Fatalf("build-id note: %d bytes, %v", len(b), err)
	}
	return hex.EncodeToString(b[16:32]) // namesz, descsz, type, "GNU\0", then the 16-byte id
}

func TestPatchRealLibrary(t *testing.T) {
	orig := readOrSkip(t, origLib)
	want := readOrSkip(t, fixLib)
	if got := SHA256Hex(orig); got != OrigSHA256 {
		t.Fatalf("original sha256 %s, want %s", got, OrigSHA256)
	}
	keep := bytes.Clone(orig)
	got, err := Patch(orig)
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !bytes.Equal(orig, keep) {
		t.Fatal("Patch modified its input")
	}
	if SHA256Hex(got) != FixSHA256 || !bytes.Equal(got, want) {
		t.Fatalf("patched sha256 %s, want %s (the file in %s)", SHA256Hex(got), FixSHA256, fixLib)
	}
	// Same size, and every changed byte inside a declared jbe: build-id, dynamic symbols, GOT and data untouched.
	if len(got) != len(orig) {
		t.Fatalf("size %d, want %d", len(got), len(orig))
	}
	ranges := jbeRanges(t, orig)
	changed := 0
	for k := range orig {
		if orig[k] == got[k] {
			continue
		}
		changed++
		inside := false
		for _, r := range ranges {
			inside = inside || r[0] <= k && k < r[1]
		}
		if !inside {
			t.Errorf("byte 0x%x changed outside the patch sites", k)
		}
	}
	if changed != 12 { // 3 x (0f 86 xx xx 00 00 -> 66 0f 1f 44 00 00): the last two bytes stay
		t.Errorf("%d bytes changed, want 12", changed)
	}
	if a, b := buildID(t, orig), buildID(t, got); a != BuildID || b != BuildID {
		t.Errorf("build-id %s -> %s, want %s", a, b, BuildID)
	}
	// textDelta as the vkfix port writes it would be VA - 0x1000 here: check the PT_LOAD mapping says the same.
	for _, s := range sites {
		if off, _ := fileOffset(orig, s.va, len(s.block)); off != s.va-0x1000 {
			t.Errorf("%s: file offset 0x%x, want 0x%x", s.name, off, s.va-0x1000)
		}
	}
	// For the cross-check against the Python reference: dump the result where asked.
	if out := os.Getenv("MODAVD_NDKFIX_DUMP"); out != "" {
		if err := os.WriteFile(out, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPatchWrongInput: anything but the image's library is refused before a byte is changed, the already patched
// library included.
func TestPatchWrongInput(t *testing.T) {
	cases := map[string][]byte{"empty": nil, "junk": []byte("x"), "elf": synthetic(t)}
	if orig, err := os.ReadFile(origLib); err == nil {
		flipped := bytes.Clone(orig)
		flipped[len(flipped)/2] ^= 0xff
		cases["flipped"] = flipped
		if fix, err := os.ReadFile(fixLib); err == nil {
			cases["patched"] = fix
		}
	}
	for name, in := range cases {
		keep := bytes.Clone(in)
		out, err := Patch(in)
		if out != nil || errs.CodeOf(err) != errs.Unsupported {
			t.Errorf("%s: got %v, %v; want unsupported", name, out != nil, err)
			continue
		}
		if want := "ARM 转译层库 sha256 " + SHA256Hex(in) + " 不是预期版本"; err.Error() != want {
			t.Errorf("%s: message %q, want %q", name, err.Error(), want)
		}
		if !bytes.Equal(in, keep) {
			t.Errorf("%s: input changed", name)
		}
	}
}

// synthetic is an ELF with the library's executable PT_LOAD (file 0xc74c0, VA 0xc84c0) holding the three blocks
// where the image's library has them, and zeros elsewhere.
func synthetic(t *testing.T) []byte {
	t.Helper()
	const off, va, size = 0xc74c0, 0xc84c0, 0x14de30
	data := make([]byte, off+size)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	le := binary.LittleEndian
	le.PutUint16(data[16:], 3)  // ET_DYN
	le.PutUint16(data[18:], 62) // EM_X86_64
	le.PutUint32(data[20:], 1)
	le.PutUint64(data[32:], 64) // e_phoff
	le.PutUint16(data[52:], 64) // e_ehsize
	le.PutUint16(data[54:], 56) // e_phentsize
	le.PutUint16(data[56:], 1)  // e_phnum
	le.PutUint16(data[58:], 64) // e_shentsize
	ph := data[64:]
	le.PutUint32(ph[0:], uint32(elf.PT_LOAD))
	le.PutUint32(ph[4:], uint32(elf.PF_R|elf.PF_X))
	le.PutUint64(ph[8:], off)
	le.PutUint64(ph[16:], va)
	le.PutUint64(ph[24:], va)
	le.PutUint64(ph[32:], size)
	le.PutUint64(ph[40:], size)
	le.PutUint64(ph[48:], 0x1000)
	for _, s := range sites {
		copy(data[s.va-va+off:], s.block)
	}
	return data
}

// TestApply: on the library's layout exactly the three jbe become nop6, the rest of each block stays.
func TestApply(t *testing.T) {
	data := synthetic(t)
	before := bytes.Clone(data)
	if err := apply(data); err != nil {
		t.Fatal(err)
	}
	for _, s := range sites {
		off := s.va - 0x1000
		want := bytes.Clone(s.block)
		copy(want[s.jbe:], nop6)
		if got := data[off : off+len(s.block)]; !bytes.Equal(got, want) {
			t.Errorf("%s: %x, want %x", s.name, got, want)
		}
		copy(data[off:], s.block) // back to the original for the comparison below
	}
	if !bytes.Equal(data, before) {
		t.Error("bytes changed outside the blocks")
	}
}

// TestApplyRefuses: one byte off in any block, or a block outside the executable segment, changes nothing at all.
func TestApplyRefuses(t *testing.T) {
	for k, s := range sites {
		data := synthetic(t)
		data[s.va-0x1000+3] ^= 0x01
		before := bytes.Clone(data)
		err := apply(data)
		if errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "原字节不符") {
			t.Errorf("site %d: %v", k, err)
		}
		if !bytes.Equal(data, before) {
			t.Errorf("site %d: data changed although a block did not match", k)
		}
	}
	data := synthetic(t)
	binary.LittleEndian.PutUint32(data[64+4:], uint32(elf.PF_R)) // the segment loses its X
	if err := apply(data); errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "不在可执行段里") {
		t.Errorf("non-executable segment: %v", err)
	}
	if err := apply([]byte("not an elf")); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("not an ELF: %v", err)
	}
}

// TestSiteTargets: golden branch targets (llvm-objdump of the image's library). The jmp each jbe falls through to
// after the edit is the ja target of CRm:op2 >= 6, the NOP path; the jbe went to UndefinedInsn.
func TestSiteTargets(t *testing.T) {
	want := []struct{ ja, jbe, jmp int }{
		{0xd9c6c, 0xd9bfe, 0xd9c6c},
		{0x140905, 0x14089d, 0x140905},
		{0x1d58f0, 0x1d58e3, 0x1d58f0},
	}
	for k, s := range sites {
		ja, jmp, err := s.targets()
		if err != nil {
			t.Fatal(err)
		}
		jbe := s.va + s.jbe + 6 + int(int32(binary.LittleEndian.Uint32(s.block[s.jbe+2:])))
		if ja != want[k].ja || jbe != want[k].jbe || jmp != want[k].jmp {
			t.Errorf("%s: ja 0x%x jbe 0x%x jmp 0x%x, want %+v", s.name, ja, jbe, jmp, want[k])
		}
	}
	if got := hex.EncodeToString(nop6); got != "660f1f440000" {
		t.Errorf("nop6 = %s", got)
	}
	// a block whose jmp went elsewhere is refused
	bad := sites[0]
	bad.block = bytes.Clone(bad.block)
	bad.block[len(bad.block)-4]++
	if ja, jmp, _ := bad.targets(); ja == jmp {
		t.Error("changed jmp displacement still reads as the NOP path")
	}
}

func TestMustHex(t *testing.T) {
	if got := hex.EncodeToString(mustHex(" 0f 86\t42 01 ")); got != "0f864201" {
		t.Errorf("mustHex %s", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("mustHex accepted an odd digit count")
		}
	}()
	mustHex("4")
}
