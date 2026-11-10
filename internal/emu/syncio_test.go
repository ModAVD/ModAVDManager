//go:build windows

package emu

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// The table for 6 CPUs and 100 us as an independent generator wrote it (Python, 11-10); the emulator booted with
// exactly these bytes and the guest registered acpi_idle with C1 latency 100.
const cst6x100 = "535344547401000001a44d4f444156444350554353542020010000004d41564401000000" +
	"10375c2f035f53425f4350555343303030085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803" +
	"10375c2f035f53425f4350555343303031085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803" +
	"10375c2f035f53425f4350555343303032085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803" +
	"10375c2f035f53425f4350555343303033085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803" +
	"10375c2f035f53425f4350555343303034085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803" +
	"10375c2f035f53425f4350555343303035085f43535412210201121d0411140a11820c007f00000000000000000000007900010a640be803"

func TestCstTable(t *testing.T) {
	want, err := hex.DecodeString(cst6x100)
	if err != nil {
		t.Fatal(err)
	}
	if got := cstTable(6, 100); !bytes.Equal(got, want) {
		t.Errorf("6 CPUs, 100 us:\n got %x\nwant %x", got, want)
	}
	// other sizes against the same independent generator: the name of every CPU, the integer and PkgLength encodings
	for _, c := range []struct {
		cpus, latency, size int
		sha256              string
	}{
		{1, 100, 92, "88a6c245d7a6485ca0b9ec9fbceca1a85c44c97ddc90c14b0a30ca2bad992766"},
		{16, 100, 932, "c4c54a4ad368b999c6ac808b99e341b79a2dc0f257ccf1c98ac9bb4fa58a5236"},
		{255, 100, 14316, "829adc39854317053694b541a6524a6690011578d5c11bd3b46a28f3e6a583e1"},
		{6, 300, 378, "87d9798795647e88dcbc4ee3e31668d489304b5937ef91ac97d67bc13ed84464"},
		{6, 70000, 390, "33da78008ccec42e1c75154f6e623f7f05843990e45f0b0d342f42bfc5513cc7"},
	} {
		got := cstTable(c.cpus, c.latency)
		sum := sha256.Sum256(got)
		if len(got) != c.size || hex.EncodeToString(sum[:]) != c.sha256 {
			t.Errorf("%d CPUs, %d us: %d bytes, sha256 %x", c.cpus, c.latency, len(got), sum)
		}
		// what a guest checks before it looks inside: the length field and the byte sum
		if n := binary.LittleEndian.Uint32(got[4:]); int(n) != len(got) {
			t.Errorf("%d CPUs: length field %d, table %d bytes", c.cpus, n, len(got))
		}
		var s byte
		for _, b := range got {
			s += b
		}
		if s != 0 {
			t.Errorf("%d CPUs: bytes sum to %d, want 0", c.cpus, s)
		}
	}
}

func TestAmlPackage(t *testing.T) {
	for _, c := range []struct {
		content int
		want    []byte
	}{
		{0, []byte{0x12, 0x01}},
		{62, []byte{0x12, 0x3f}},               // 62 + 1 = 63: the last one-byte length
		{63, []byte{0x12, 0x41, 0x04}},         // 63 + 2 = 65 = 0x41: nibble 1, then 0x04
		{4093, []byte{0x12, 0x4f, 0xff}},       // 4093 + 2 = 4095: the last two-byte length
		{4094, []byte{0x12, 0x81, 0x00, 0x01}}, // 4094 + 3 = 4097 = 0x1001
	} {
		got := amlPackage(0x12, make([]byte, c.content))
		if !bytes.Equal(got[:len(c.want)], c.want) || len(got) != len(c.want)+c.content {
			t.Errorf("content %d: head %x (%d bytes), want %x", c.content, got[:len(c.want)], len(got), c.want)
		}
	}
}

func TestWriteCstTable(t *testing.T) {
	dir := t.TempDir()
	emuDir := filepath.Join(dir, "sdk", "emulator")
	instDir := filepath.Join(dir, "avd", "mas_3.avd")
	for _, d := range []string{emuDir, instDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := writeCstTable(emuDir, instDir, 4)
	if err != nil || rel != `..\..\avd\mas_3.avd\modavd-cst.aml` {
		t.Fatalf("rel %q err %v", rel, err)
	}
	path := filepath.Join(instDir, cstFileName)
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, cstTable(4, cstLatencyUs)) {
		t.Fatalf("file: %d bytes, err %v", len(b), err)
	}
	// the relative path names the file from the emulator's working directory
	if b, err := os.ReadFile(filepath.Join(emuDir, rel)); err != nil || !bytes.Equal(b, cstTable(4, cstLatencyUs)) {
		t.Errorf("through the relative path: %d bytes, err %v", len(b), err)
	}
	// another CPU count replaces it
	if _, err := writeCstTable(emuDir, instDir, 6); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, cstTable(6, cstLatencyUs)) {
		t.Errorf("after the CPU count changed: %d bytes", len(b))
	}
	// an unchanged table is left alone (a read-only file from the last start is not an error)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := writeCstTable(emuDir, instDir, 6); err != nil {
		t.Errorf("same table, read-only file: %v", err)
	}
	os.Chmod(path, 0o644)

	// names QEMU's command line cannot carry: nothing is written, the caller starts without the table
	for _, bad := range []string{"mas,3.avd", "实例3.avd"} {
		inst := filepath.Join(dir, "avd", bad)
		if err := os.MkdirAll(inst, 0o755); err != nil {
			t.Fatal(err)
		}
		if rel, err := writeCstTable(emuDir, inst, 4); err == nil {
			t.Errorf("%s: got %q, want an error", bad, rel)
		}
		if _, err := os.Stat(filepath.Join(inst, cstFileName)); err == nil {
			t.Errorf("%s: the table was written", bad)
		}
	}
	// a folder that does not exist
	if _, err := writeCstTable(emuDir, filepath.Join(dir, "avd", "mas_9.avd"), 4); err == nil {
		t.Error("missing instance folder: want an error")
	}
}

func syncIOManager(t *testing.T) (*Manager, *avd.Instance) {
	t.Helper()
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(r.AvdHome(), "mas_3.avd")
	for _, d := range []string{filepath.Dir(r.EmulatorExe()), instDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(instDir, "config.ini")
	if err := os.WriteFile(cfg, []byte("hw.cpu.ncore=4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ini, err := avd.ReadIni(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: r, Log: logx.Nop(), ADB: adb.New(r, logx.Nop())}
	return m, &avd.Instance{Index: 3, Name: "mas_3", Dir: instDir, Config: ini}
}

func TestSyncIOQemuArgs(t *testing.T) {
	t.Setenv(envSyncIO, "")
	m, inst := syncIOManager(t)
	rel, err := filepath.Rel(filepath.Dir(m.Root.EmulatorExe()), filepath.Join(inst.Dir, cstFileName))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-global", "virtio-blk-pci.ioeventfd=off", "-acpitable", "file=" + rel}
	if got := m.syncIOQemuArgs(inst); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(inst.Dir, cstFileName)); err != nil || !bytes.Equal(b, cstTable(4, cstLatencyUs)) {
		t.Errorf("table for the instance's 4 CPUs: %d bytes, err %v", len(b), err)
	}
	// they end up after the manager's other QEMU arguments
	args := buildLaunchArgs("mas_3", 20030, 4096, true, m.syncIOQemuArgs(inst)...)
	if n := len(masArgsHeadless); !reflect.DeepEqual(args[:n], masArgsHeadless) || !reflect.DeepEqual(args[n:], want) {
		t.Errorf("launch arguments: %q", args)
	}

	// no CPU count: the disk argument alone, no table
	os.Remove(filepath.Join(inst.Dir, cstFileName))
	for _, cfg := range []string{"", "hw.cpu.ncore=0\n", "hw.cpu.ncore=256\n", "hw.cpu.ncore=many\n"} {
		path := filepath.Join(inst.Dir, "config.ini")
		if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		ini, err := avd.ReadIni(path)
		if err != nil {
			t.Fatal(err)
		}
		got := m.syncIOQemuArgs(&avd.Instance{Index: 3, Name: "mas_3", Dir: inst.Dir, Config: ini})
		if !reflect.DeepEqual(got, want[:2]) {
			t.Errorf("config %q: got %q", cfg, got)
		}
		if _, err := os.Stat(filepath.Join(inst.Dir, cstFileName)); err == nil {
			t.Errorf("config %q: a table was written", cfg)
		}
	}

	// the table cannot be written (the instance folder is gone): still the disk argument
	gone := &avd.Instance{Index: 3, Name: "mas_3", Dir: filepath.Join(m.Root.AvdHome(), "mas_8.avd"), Config: inst.Config}
	if got := m.syncIOQemuArgs(gone); !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("missing folder: got %q", got)
	}
}

func TestSyncIOOff(t *testing.T) {
	m, inst := syncIOManager(t)
	for _, v := range []string{"off", "OFF", " off "} {
		t.Setenv(envSyncIO, v)
		if got := m.syncIOQemuArgs(inst); got != nil {
			t.Errorf("%s=%q: got %q", envSyncIO, v, got)
		}
		if got := buildLaunchArgs("mas_3", 20030, 4096, true, m.syncIOQemuArgs(inst)...); !reflect.DeepEqual(got, masArgsHeadless) {
			t.Errorf("%s=%q: launch arguments %q", envSyncIO, v, got)
		}
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, cstFileName)); err == nil {
		t.Error("switched off, yet a table was written")
	}
	// anything else is not "off"
	for _, v := range []string{"0", "no", "on"} {
		t.Setenv(envSyncIO, v)
		if got := m.syncIOQemuArgs(inst); len(got) != 4 {
			t.Errorf("%s=%q: got %q", envSyncIO, v, got)
		}
	}
}
