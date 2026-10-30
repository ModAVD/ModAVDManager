package vkfix

import (
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// The real libraries (read-only); the tests that need them are skipped when they are absent.
const (
	origLib = `C:\emu-src\work\gfx\guestlibs\libvulkan_enc.so`
	fixLib  = `C:\emu-src\work\gfx\guestlibs\libvulkan_enc.khrfix-bindlock2.so`
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
	if !bytes.Equal(got, want) {
		t.Fatalf("patched library differs from %s", fixLib)
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
