//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"modavd/internal/root"
)

// Capture the Windows file ID now. os.SameFile lazily reopens the path on
// Windows and would compare the replacement with itself after a rename.
func compactFileID(t *testing.T, path string) [3]uint32 {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		t.Fatal(err)
	}
	return [3]uint32{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}
}

// Nonreferenced trailing storage models space left after discarded clusters.
// It gives the existing integration tests (including #56's BackingData) a real
// reason to convert, rather than accidentally testing only the new skip path.
func addCompactGarbage(t *testing.T, overlay string) {
	t.Helper()
	f, err := os.OpenFile(overlay, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := bytes.Repeat([]byte{0x97}, 1<<20)
	for range 32 {
		if _, err := f.Write(b); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactBudgetRealImages(t *testing.T) {
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("set MODAVD_TEST_SDK_ROOT to run real qemu-img compact/skip checks")
	}
	qemuImg := filepath.Join(sdk, "emulator", "qemu-img.exe")
	dir := opsTestTempDir(t)
	// No emulator or adb is started. These roots, SDK and reserved ports belong
	// only to this test, independently of other acceptance sessions.
	meta := `{"adbServerPort":25191,"scriptAdbServerPort":25192,"sdkRoot":` + strconv.Quote(sdk) + `}`
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const i = 2
	avdDir := filepath.Join(r.AvdHome(), root.AvdName(i)+".avd")
	if err := os.MkdirAll(avdDir, 0755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		filepath.Join(r.AvdHome(), root.AvdName(i)+".ini"): "path=" + avdDir + "\n",
		filepath.Join(avdDir, "config.ini"):                "hw.cpu.ncore=4\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	qemu := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(qemuImg, args...)
		cmd.Dir = avdDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", args, err, out)
		}
		return out
	}
	backing := filepath.Join(avdDir, userdataBacking)
	overlay := filepath.Join(avdDir, userdataOverlay)
	src := filepath.Join(dir, "source.raw")
	expected := make([]byte, 64<<20)
	inherited := bytes.Repeat([]byte{0x31}, 64<<10)
	overwritten := bytes.Repeat([]byte{0xa5}, 64<<10)
	copy(expected[1<<20:], inherited)
	copy(expected[32<<20:], bytes.Repeat([]byte{0x72}, len(overwritten)))
	if err := os.WriteFile(src, expected, 0600); err != nil {
		t.Fatal(err)
	}
	qemu("convert", "-f", "raw", "-O", "qcow2", src, backing)
	clear(expected)
	copy(expected[32<<20:], overwritten)
	if err := os.WriteFile(src, expected, 0600); err != nil {
		t.Fatal(err)
	}
	qemu("convert", "-f", "raw", "-O", "qcow2", src, overlay)
	qemu("rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", userdataBacking, overlay)
	copy(expected[1<<20:], inherited)
	addCompactGarbage(t, overlay)
	backingBefore, err := os.ReadFile(backing)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	ctx := context.Background()
	verify := func(stage string) {
		t.Helper()
		var extents []struct {
			Start, Length int64
			Depth         int
			Data          bool
		}
		if err := json.Unmarshal(qemu("map", "--output=json", overlay), &extents); err != nil {
			t.Fatal(err)
		}
		for _, region := range []struct {
			start int64
			depth int
		}{{1 << 20, 1}, {32 << 20, 0}} {
			found := false
			for _, e := range extents {
				if e.Start <= region.start && e.Start+e.Length >= region.start+65536 && e.Data && e.Depth == region.depth {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: lost backing depth at %d: %+v", stage, region.start, extents)
			}
		}
		raw := filepath.Join(dir, stage+".raw")
		qemu("convert", "-f", "qcow2", "-O", "raw", overlay, raw)
		guest, err := os.ReadFile(raw)
		if err != nil || !bytes.Equal(guest, expected) {
			t.Fatalf("%s: guest content changed: %v", stage, err)
		}
		b, err := os.ReadFile(backing)
		if err != nil || !bytes.Equal(b, backingBefore) {
			t.Fatalf("%s: backing changed: %v", stage, err)
		}
	}
	verify("before")
	oldIdentity := compactFileID(t, overlay)
	info, err := m.DiskInfo(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	// Keep diagnostics valid when DiskInfo represents an unavailable size with
	// a pointer. Formatting that pointer with %d prints its address, not bytes.
	infoJSON, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before: %s", infoJSON)
	res, err := m.DiskCompact(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrite: %+v", res)
	if res.Skipped || res.FreedBytes < 16<<20 || res.OnDiskDeltaBytes != -res.FreedBytes {
		t.Fatalf("did not execute a useful compact: %+v", res)
	}
	newIdentity := compactFileID(t, overlay)
	if oldIdentity == newIdentity {
		t.Fatal("compact did not replace original")
	}
	verify("rewritten")
	beforeSkip, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	res, err = m.DiskCompact(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("repeat: %+v", res)
	afterSkip, err := os.ReadFile(overlay)
	skipIdentity := compactFileID(t, overlay)
	if err != nil || !res.Skipped || res.OnDiskDeltaBytes != 0 || !bytes.Equal(beforeSkip, afterSkip) || newIdentity != skipIdentity {
		t.Fatalf("repeat rewrote original: %+v %v", res, err)
	}
	verify("skipped")
	// check opens the backing chain even on the skip path. A missing backing
	// must remain an error rather than a successful low-benefit result.
	savedBacking := backing + ".saved"
	if err := os.Rename(backing, savedBacking); err != nil {
		t.Fatal(err)
	}
	_, missingErr := m.DiskCompact(ctx, i)
	if err := os.Rename(savedBacking, backing); err != nil {
		t.Fatal(err)
	}
	if missingErr == nil {
		t.Fatal("missing backing accepted on skip path")
	}
	unchanged, err := os.ReadFile(overlay)
	if err != nil || !bytes.Equal(unchanged, afterSkip) {
		t.Fatalf("missing backing modified original: %v", err)
	}
	// Damage a metadata refcount without changing guest clusters. info still
	// opens it, but check must refuse it even though estimated savings are tiny.
	table := binary.BigEndian.Uint64(afterSkip[48:56])
	block := binary.BigEndian.Uint64(afterSkip[table : table+8])
	corrupt := bytes.Clone(afterSkip)
	corrupt[block], corrupt[block+1] = 0, 0
	if err := os.WriteFile(overlay, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if res, err := m.DiskCompact(ctx, i); err == nil || res != nil {
		t.Fatalf("corrupt low-gain image accepted: %+v %v", res, err)
	}
	afterFailure, err := os.ReadFile(overlay)
	if err != nil || !bytes.Equal(afterFailure, corrupt) {
		t.Fatalf("corrupt original modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(avdDir, userdataCompactTmp)); !os.IsNotExist(err) {
		t.Fatal("scratch image left behind")
	}
}
