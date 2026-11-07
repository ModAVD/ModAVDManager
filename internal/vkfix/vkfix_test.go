package vkfix

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// The real libraries (read-only) in $MODAVD_TEST_VKLIBS: the image's libvulkan_enc.so and the patched copy AUTO-MAS
// vulkanfix.py made of it, libvulkan_enc.khrfix-bindlock2.so. The tests that need them are skipped when they are absent.
var (
	origLib = filepath.Join(os.Getenv("MODAVD_TEST_VKLIBS"), "libvulkan_enc.so")
	fixLib  = filepath.Join(os.Getenv("MODAVD_TEST_VKLIBS"), "libvulkan_enc.khrfix-bindlock2.so")
)

// Golden bytes printed by AUTO-MAS vulkanfix.py (_wrapper_bind / _wrapper_bind2 at their caves, _jmp, _EXIT_MAGIC).
const (
	goldenW1 = "535541544155415641574883ec18488b5f084989f44989ce4d89c74c89cd488dbb60060000e8110a0300488b44245048890424" +
		"4889df4c89e64c89f14d89f84989e9e874b1e5ff4189c5488dbb60060000e8150a03004489e83d3253414d751c4c89e74c89f6" +
		"4c89fa4889e94c8b44245041b901000000e8af1303004883c418415f415e415d415c5d5bc3"
	goldenW2 = "535541544155415641574883ec08488b5f084989f44989ce4d89c74c89cd488dbb60060000e8860903004889df4c89e64c89f1" +
		"4d89f84989e9e832b3e5ff4189c5488dbb60060000e8930903004489e83d3253414d75174c89e74c89f64489fa4889e941b80100" +
		"0000e8521303004883c408415f415e415d415c5d5bc3"
	goldenJmpKhr    = "e9ebe9ffff"
	goldenJmpDyn    = "e9cb200200"
	goldenExitMagic = "b83253414d5b415c415d415e415f5dc3"

	// the companion's khrfix + bindlock2 library (no template rollover fix)
	companionFixSHA256 = "d5258af6244679fd5ce50245b7dcc1f645d971a04f7e64fc9872fcfb9faa7ecb"
)

// The template rollover sites (checked by running both functions under unicorn, original and patched, 11-07):
// same length as the original, inside .text, not overlapping each other or any other patched range.
func TestTemplateSites(t *testing.T) {
	golden := map[int][2]string{
		0x1FED62: {"4a8d346d000000004c01ee4189d6", "4b8d746d00448d73ff0f1f440000"},
		0x1FEE2C: {"468d3411", "448d72ff"},
	}
	if len(templateSites) != len(golden) {
		t.Fatalf("%d template sites, want %d", len(templateSites), len(golden))
	}
	others := [][2]int{
		{khrEntry, khrEntry + 5}, {khrDyn, khrDynEnd}, {exitBind, exitBind + len(exitBindOrig)},
		{exitBind2, exitBind2 + len(exitBind2Orig)}, {fwdBind, fwdBind + 5}, {fwdBind2, fwdBind2 + 5},
		{fwdBind2KHR, fwdBind2KHR + 5},
	}
	for _, s := range templateSites {
		g, ok := golden[s.va]
		if !ok {
			t.Errorf("%s: unexpected site 0x%x", s.name, s.va)
			continue
		}
		if hex.EncodeToString(s.orig) != g[0] || hex.EncodeToString(s.fix) != g[1] {
			t.Errorf("%s (0x%x): %x -> %x, want %s -> %s", s.name, s.va, s.orig, s.fix, g[0], g[1])
		}
		if len(s.orig) != len(s.fix) {
			t.Errorf("%s: %d bytes replaced by %d", s.name, len(s.orig), len(s.fix))
		}
		if s.va < 0x1fcee0 || s.va+len(s.fix) > 0x1fcee0+0x1e951b {
			t.Errorf("%s: 0x%x outside .text", s.name, s.va)
		}
		for _, o := range append(others, func() (r [][2]int) {
			for _, u := range templateSites {
				if u.va != s.va {
					r = append(r, [2]int{u.va, u.va + len(u.fix)})
				}
			}
			return
		}()...) {
			if s.va < o[1] && o[0] < s.va+len(s.fix) {
				t.Errorf("%s (0x%x) overlaps 0x%x-0x%x", s.name, s.va, o[0], o[1])
			}
		}
	}
}

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
	if SHA256Hex(got) != FixSHA256 {
		t.Fatalf("patched sha256 %s, want %s", SHA256Hex(got), FixSHA256)
	}
	// the companion's library has steps 1 and 2 only: equal to ours once the template sites are applied to it
	if SHA256Hex(want) != companionFixSHA256 {
		t.Fatalf("%s sha256 %s, want %s", fixLib, SHA256Hex(want), companionFixSHA256)
	}
	for _, s := range templateSites {
		copy(want[s.va-textDelta:], s.fix)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("patched library differs from %s plus the template sites", fixLib)
	}
	for _, s := range templateSites {
		off := s.va - textDelta
		if !bytes.Equal(keep[off:off+len(s.orig)], s.orig) || !bytes.Equal(got[off:off+len(s.fix)], s.fix) {
			t.Errorf("%s (0x%x): original %x, patched %x", s.name, s.va, keep[off:off+len(s.orig)], got[off:off+len(s.fix)])
		}
	}
	if !bytes.Equal(orig, keep) {
		t.Fatal("Patch modified its input")
	}
	// For the cross-check against the Python module: dump the result where asked.
	if out := os.Getenv("MODAVD_VKFIX_DUMP"); out != "" {
		if err := os.WriteFile(out, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPatchWrongInput(t *testing.T) {
	cases := map[string][]byte{"empty": nil, "junk": []byte("x")}
	if orig, err := os.ReadFile(origLib); err == nil {
		flipped := bytes.Clone(orig)
		flipped[len(flipped)/2] ^= 0xff
		cases["flipped"] = flipped
		// the already patched library is not an input either
		if fix, err := os.ReadFile(fixLib); err == nil {
			cases["patched"] = fix
		}
	}
	for name, in := range cases {
		out, err := Patch(in)
		if out != nil || errs.CodeOf(err) != errs.Unsupported {
			t.Errorf("%s: got %v, %v; want unsupported", name, out != nil, err)
			continue
		}
		want := "客体驱动库 sha256 " + SHA256Hex(in) + " 不是预期版本"
		if err.Error() != want {
			t.Errorf("%s: message %q, want %q", name, err.Error(), want)
		}
	}
}

func TestWrappersMatchPython(t *testing.T) {
	cave1 := khrDyn + 5
	w1, err := wrapperBind(cave1)
	if err != nil {
		t.Fatal(err)
	}
	cave2 := cave1 + len(w1)
	w2, err := wrapperBind2(cave2)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(w1); got != goldenW1 {
		t.Errorf("wrapperBind\n got %s\nwant %s", got, goldenW1)
	}
	if got := hex.EncodeToString(w2); got != goldenW2 {
		t.Errorf("wrapperBind2\n got %s\nwant %s", got, goldenW2)
	}
	if cave2+len(w2) > khrDynEnd {
		t.Errorf("wrappers (%d + %d) do not fit", len(w1), len(w2))
	}
	if got := hex.EncodeToString(jmp(khrEntry, khrCore)); got != goldenJmpKhr {
		t.Errorf("jmp khr %s, want %s", got, goldenJmpKhr)
	}
	if got := hex.EncodeToString(jmp(khrDyn, khrCore)); got != goldenJmpDyn {
		t.Errorf("jmp dyn %s, want %s", got, goldenJmpDyn)
	}
	if got := hex.EncodeToString(exitMagic); got != goldenExitMagic {
		t.Errorf("exit magic %s, want %s", got, goldenExitMagic)
	}
	// the replaced exits must not grow
	if len(exitMagic) > len(exitBindOrig) || len(exitMagic) > len(exitBind2Orig) {
		t.Error("exitMagic longer than an original exit")
	}
}

func TestCallDisplacement(t *testing.T) {
	a := &asm{base: 0x1000}
	a.raw("90 90")
	a.call(0x1000)
	// call at 0x1002, next instruction at 0x1007: rel = 0x1000 - 0x1007 = -7
	if got := hex.EncodeToString(a.buf); got != "9090e8f9ffffff" {
		t.Errorf("call bytes %s", got)
	}
}

func TestEpilogueOutOfRange(t *testing.T) {
	for _, n := range []int{0x7f, 0x80, 0x100} {
		a := &asm{base: 0}
		a.raw("7500")
		at := len(a.buf) - 1
		a.buf = append(a.buf, bytes.Repeat([]byte{0x90}, n)...)
		err := epilogue(a, 0x08, at)
		if n < 0x80 {
			if err != nil || a.buf[at] != byte(n) {
				t.Errorf("rel %d: err %v, rel8 %#x", n, err, a.buf[at])
			}
			continue
		}
		if errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "jne 跳不到") {
			t.Errorf("rel %d: err %v", n, err)
		}
	}
}

func TestMustHex(t *testing.T) {
	if got := hex.EncodeToString(mustHex(" 48 8b\t7f08 e9 ")); got != "488b7f08e9" {
		t.Errorf("mustHex %s", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("mustHex accepted an odd digit count")
		}
	}()
	mustHex("4")
}

func TestSHA256Hex(t *testing.T) {
	if got := SHA256Hex(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("SHA256Hex(nil) = %s", got)
	}
}
