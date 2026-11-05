//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"modavd/internal/root"
	"modavd/internal/win"
)

// TestDiskCompactIntegration runs compact end to end against a real qemu-img -- the path the fake-manager unit tests
// cannot reach, and the one the `-T none` bug failed on for every real qcow2 overlay (ModAVD/ModAVD#2 P1-1). It is
// skipped unless MODAVD_TEST_SDK_ROOT points at an SDK whose emulator\qemu-img.exe exists, so CI without an SDK just
// skips it. Run it with MODAVD_TEST_SDK_ROOT set to such an SDK directory, e.g. MODAVD_TEST_SDK_ROOT=<SDK 目录>.
func TestDiskCompactIntegration(t *testing.T) {
	t.Run("EmptyBacking", func(t *testing.T) {
		testDiskCompactIntegration(t, false)
	})
	t.Run("BackingData", func(t *testing.T) {
		testDiskCompactIntegration(t, true)
	})
}

func testDiskCompactIntegration(t *testing.T, backingData bool) {
	t.Helper()
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("set MODAVD_TEST_SDK_ROOT to an SDK with emulator\\qemu-img.exe to run the compact integration test")
	}
	qemuImg := filepath.Join(sdk, "emulator", "qemu-img.exe")
	if fi, err := os.Stat(qemuImg); err != nil || fi.IsDir() {
		t.Skipf("no qemu-img at %s: %v", qemuImg, err)
	}

	// A temp root whose SDK is the real one, with one powered-off instance.
	dir := opsTestTempDir(t)
	meta := `{"adbServerPort":20391,"scriptAdbServerPort":20392,"sdkRoot":` + strconv.Quote(sdk) + `}`
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const i = 2
	avdDir := filepath.Join(r.AvdHome(), root.AvdName(i)+".avd")
	if err := os.MkdirAll(avdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.AvdHome(), root.AvdName(i)+".ini"), []byte("path="+avdDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(avdDir, "config.ini"), []byte("hw.cpu.ncore=4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fs, _ := win.VolumeFileSystem(avdDir); !compactFileSystems[strings.ToUpper(fs)] {
		t.Skipf("compact runs only on NTFS/ReFS; %s is on %s", avdDir, fs)
	}
	if free, _ := win.DiskFree(avdDir); free < 3<<30 {
		t.Skipf("need >3 GiB free on the volume of %s for the compact integration test", avdDir)
	}

	// Both fixtures use the same real compact path below.
	backing := filepath.Join(avdDir, userdataBacking)
	overlay := filepath.Join(avdDir, userdataOverlay)
	src := filepath.Join(dir, "src.raw")
	qemu := func(args ...string) []byte {
		t.Helper()
		t.Logf("qemu-img %v", args)
		cmd := exec.Command(qemuImg, args...)
		cmd.Dir = avdDir // so the relative -B backing name resolves next to the output
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", args, err, out)
		}
		return out
	}
	const inheritedOffset = 1 << 20
	const overwrittenOffset = 32 << 20
	inherited := bytes.Repeat([]byte{0x31}, 64<<10)
	overwritten := bytes.Repeat([]byte{0xa5}, 64<<10)
	if backingData {
		data := make([]byte, 64<<20)
		copy(data[inheritedOffset:], inherited)
		copy(data[overwrittenOffset:], bytes.Repeat([]byte{0x72}, len(overwritten)))
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		qemu("convert", "-f", "raw", "-O", "qcow2", src, userdataBacking)

		// Convert a delta with only the overwritten region allocated, then attach
		// the backing. The holes must inherit data rather than store a copy of it.
		clear(data)
		copy(data[overwrittenOffset:], overwritten)
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		qemu("convert", "-f", "raw", "-O", "qcow2", src, userdataOverlay)
		qemu("rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", userdataBacking, userdataOverlay)
	} else {
		writePattern(t, src, 64<<20)
		qemu("create", "-f", "qcow2", userdataBacking, "64M")
		qemu("convert", "-O", "qcow2", "-B", userdataBacking, "-o", "backing_fmt=qcow2", src, userdataOverlay)
	}
	backingBefore, err := os.ReadFile(backing)
	if err != nil {
		t.Fatal(err)
	}
	verifyBackingData := func(stage string) {
		t.Helper()
		if !backingData {
			return
		}
		var extents []struct {
			Start  int64 `json:"start"`
			Length int64 `json:"length"`
			Depth  int   `json:"depth"`
			Data   bool  `json:"data"`
		}
		if err := json.Unmarshal(qemu("map", "--output=json", overlay), &extents); err != nil {
			t.Fatal(err)
		}
		raw := filepath.Join(dir, stage+".raw")
		qemu("convert", "-f", "qcow2", "-O", "raw", overlay, raw)
		guest, err := os.Open(raw)
		if err != nil {
			t.Fatal(err)
		}
		defer guest.Close()
		for _, region := range []struct {
			offset int64
			want   []byte
			depth  int
		}{
			{inheritedOffset, inherited, 1},
			{overwrittenOffset, overwritten, 0},
		} {
			mapped := false
			for _, extent := range extents {
				if extent.Start <= region.offset && extent.Start+extent.Length >= region.offset+int64(len(region.want)) && extent.Data && extent.Depth == region.depth {
					mapped = true
				}
			}
			if !mapped {
				t.Fatalf("%s: region at %d is not data at backing depth %d: %+v", stage, region.offset, region.depth, extents)
			}
			got := make([]byte, len(region.want))
			if _, err := guest.ReadAt(got, region.offset); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, region.want) {
				t.Fatalf("%s: guest data changed at %d", stage, region.offset)
			}
		}
	}
	verifyBackingData("before")

	// Keep a pristine copy (overlay + its backing) to confirm the guest-visible content survives the rewrite.
	pristineDir := filepath.Join(dir, "pristine")
	addCompactGarbage(t, overlay) // keep the convert/check/compare path covered after low-benefit skips
	if err := os.MkdirAll(pristineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pristineOverlay := filepath.Join(pristineDir, userdataOverlay)
	copyFile(t, overlay, pristineOverlay)
	copyFile(t, backing, filepath.Join(pristineDir, userdataBacking))

	// The instance is powered off for the whole test.
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{} }
	t.Cleanup(func() { qemuScan = saved })

	m := New(r, nil)
	ctx := context.Background()

	// info first: with a real qemu-img it computes the allocated/reclaimable figures.
	ires, err := m.DiskInfo(ctx, i)
	if err != nil {
		t.Fatalf("DiskInfo: %v", err)
	}
	if ires.AllocatedBytes == nil {
		t.Fatalf("DiskInfo did not compute allocated for a stopped instance: %+v", ires)
	}

	// compact: the whole convert -> check -> compare -> swap pipeline must succeed on a real qcow2 overlay.
	res, err := m.DiskCompact(ctx, i)
	if err != nil {
		t.Fatalf("DiskCompact: %v", err)
	}
	if res.Overlay != overlay {
		t.Errorf("result overlay = %q, want %q", res.Overlay, overlay)
	}
	if res.Skipped {
		t.Fatal("reclaimable fixture skipped the convert/check/compare path")
	}
	if res.FreedBytes < 0 {
		t.Errorf("freed bytes negative: %d", res.FreedBytes)
	}
	t.Logf("DiskCompact completed: %+v", res)
	verifyBackingData("after")
	if _, err := os.Stat(filepath.Join(avdDir, userdataCompactTmp)); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after a successful compact: %v", err)
	}

	// The swapped-in overlay is a sound qcow2 ...
	if out, failure, err := m.runQemuImgCheck(ctx, overlay); err != nil || failure != "" {
		t.Fatalf("qemu-img check after compact: failure=%q err=%v\n%s", failure, err, out)
	}
	// ... and holds exactly the content it had before the rewrite.
	equal, err := m.compareEqual(ctx, pristineOverlay, overlay)
	if err != nil {
		t.Fatalf("compare after compact: %v", err)
	}
	if !equal {
		t.Fatal("content changed across compact")
	}

	// A stray temp from an interrupted run is removed and compact still runs (ModAVD/ModAVD#2 P2-1, R-1).
	strayTmp := filepath.Join(avdDir, userdataCompactTmp)
	if err := os.WriteFile(strayTmp, []byte("half-written junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DiskCompact(ctx, i); err != nil {
		t.Fatalf("DiskCompact with a stray temp present: %v", err)
	}
	if _, err := os.Stat(strayTmp); !os.IsNotExist(err) {
		t.Errorf("stray temp not cleared: %v", err)
	}
	verifyBackingData("after-stray-temp")
	backingAfter, err := os.ReadFile(backing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backingBefore, backingAfter) {
		t.Fatal("backing file changed across compact")
	}
}

// writePattern writes n bytes of a deterministic non-zero pattern, so a convert onto an empty backing allocates
// clusters for all of it.
func writePattern(t *testing.T, path string, n int) {
	t.Helper()
	buf := make([]byte, n)
	for k := range buf {
		buf[k] = byte(k*31 + 7)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
