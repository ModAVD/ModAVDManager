package root

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

func putFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWindowsComponentsMatchScript: the catalogue is fetch-components.ps1's $Components, value for value.
func TestWindowsComponentsMatchScript(t *testing.T) {
	c := WindowsComponents()
	if len(c) != 2 || c[0].ID != "platform-tools" || c[1].ID != "system-image" {
		t.Fatalf("%+v", c)
	}
	pt, img := c[0], c[1]
	if pt.FetchName != "平台工具（adb）" || pt.Version != "37.0.1" || pt.URL != "platform-tools_r37.0.1-win.zip" ||
		pt.Size != 8044989 || pt.SHA1 != "e03e78b1d80b396f1c3358e31251cb31740e1110" || pt.Target != "platform-tools" ||
		pt.ArchiveRoot != "platform-tools" || pt.ExtractedBytes != 20<<20 ||
		!slices.Equal(pt.KeyFiles, []string{"adb.exe", "AdbWinApi.dll", "AdbWinUsbApi.dll"}) ||
		pt.Properties != "Pkg.UserSrc=false\nPkg.Revision=37.0.1\n" {
		t.Errorf("platform-tools %+v", pt)
	}
	pins := map[string]string{
		"adb.exe":          "b4a6b455702684652cccf7b46258b29e653538904359a58fd4931cf3ef286b3f",
		"AdbWinApi.dll":    "c1d653030b4bde65d3e07e4d0b0979e17be56df1436cdd15528630f27808050d",
		"AdbWinUsbApi.dll": "0710e894d9b40f71a670c13c694079d564c92c1279da382cfe4850983aaebe1b",
	}
	if len(pt.KeyHashes) != len(pins) {
		t.Errorf("pins %v", pt.KeyHashes)
	}
	for k, v := range pins {
		if pt.KeyHashes[k] != v {
			t.Errorf("pin %s = %s", k, pt.KeyHashes[k])
		}
	}
	wantProps := "Pkg.Desc=System Image x86_64 with Google APIs.\nPkg.Revision=14\nPkg.Dependencies=emulator#34.2.16\n" +
		"AndroidVersion.ApiLevel=34\nAndroidVersion.ExtensionLevel=7\nAndroidVersion.IsBaseSdk=true\nSystemImage.Abi=x86_64\n" +
		"SystemImage.TagId=google_apis\nSystemImage.TagDisplay=Google APIs\nSystemImage.GpuSupport=true\n" +
		"Addon.VendorId=google\nAddon.VendorDisplay=Google Inc.\n"
	if img.FetchName != "系统镜像（Android 14 Google APIs x86_64）" || img.Version != "14" ||
		img.URL != "sys-img/google_apis/x86_64-34_r14.zip" || img.Size != 1563721130 ||
		img.SHA1 != "e0f6c9a0691aa27bd597d0deb1bcfdc943ac8ca7" || img.Target != `system-images\android-34\google_apis\x86_64` ||
		img.ArchiveRoot != "x86_64" || img.ExtractedBytes != 4500<<20 || len(img.KeyHashes) != 0 ||
		!slices.Equal(img.KeyFiles, []string{"system.img", "vendor.img", "kernel-ranchu", "ramdisk.img"}) ||
		img.Properties != wantProps {
		t.Errorf("system-image %+v", img)
	}
	// check and status keep AUTO-MAS's names and files, and those files are part of what fetch requires.
	if pt.Name != PlatformToolsName || img.Name != SystemImageName || !slices.Equal(pt.StatusFiles, []string{"adb.exe"}) ||
		!slices.Equal(img.StatusFiles, img.KeyFiles) {
		t.Errorf("status names / files: %+v %+v", pt, img)
	}
	for _, x := range c {
		for _, f := range x.StatusFiles {
			if !slices.Contains(x.KeyFiles, f) {
				t.Errorf("%s: status file %s is not a key file", x.ID, f)
			}
		}
	}
	// A fresh copy each time.
	c[0].KeyFiles[0], c[0].KeyHashes["adb.exe"] = "x", "y"
	if again := WindowsComponents(); again[0].KeyFiles[0] != "adb.exe" || again[0].KeyHashes["adb.exe"] != pins["adb.exe"] {
		t.Fatal("shared slices")
	}
}

func TestSDKComponentsOfHost(t *testing.T) {
	c := SDKComponents()
	if len(c) != 2 || c[0].ID != "platform-tools" || c[1].ID != "system-image" {
		t.Fatalf("%+v", c)
	}
	if runtime.GOOS == "windows" {
		if c[0].URL == "" || c[0].Size != WindowsComponents()[0].Size {
			t.Fatal("Windows has the catalogue")
		}
		return
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if c[0].URL == "" || c[0].Size <= 0 {
			t.Fatal("Apple Silicon macOS has the catalogue")
		}
		return
	}
	if c[0].URL != "" || c[1].URL != "" || !slices.Equal(c[0].StatusFiles, []string{CurrentPlatform().ADB}) {
		t.Fatalf("no catalogue here: %+v", c)
	}
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestComplete is fetch-components.ps1 Test-ComponentFiles: non-empty key files, pinned SHA-256 (any case), exactly
// one Pkg.Revision equal to the version; without pins the files alone.
func TestComplete(t *testing.T) {
	pinned := SDKComponent{ID: "platform-tools", Version: "37.0.1", StatusFiles: []string{"adb.exe"},
		KeyFiles: []string{"adb.exe", "AdbWinApi.dll"}, KeyHashes: map[string]string{"adb.exe": strings.ToUpper(hash("adb")), "AdbWinApi.dll": hash("dll")}}
	dir := testutil.TempDir(t)
	reset := func() {
		os.RemoveAll(dir)
		putFile(t, filepath.Join(dir, "adb.exe"), "adb")
		putFile(t, filepath.Join(dir, "AdbWinApi.dll"), "dll")
		putFile(t, filepath.Join(dir, "source.properties"), "Pkg.UserSrc=false\nPkg.Revision=37.0.1\n")
	}
	reset()
	if !pinned.Complete(dir) || !pinned.Present(dir) {
		t.Fatal("complete directory")
	}
	for _, c := range []struct {
		name   string
		change func()
	}{
		{"missing key file", func() { os.Remove(filepath.Join(dir, "AdbWinApi.dll")) }},
		{"empty key file", func() { putFile(t, filepath.Join(dir, "AdbWinApi.dll"), "") }},
		{"key file is a directory", func() {
			os.Remove(filepath.Join(dir, "AdbWinApi.dll"))
			os.Mkdir(filepath.Join(dir, "AdbWinApi.dll"), 0o755)
		}},
		{"other adb build", func() { putFile(t, filepath.Join(dir, "adb.exe"), "adb 2") }},
		{"no source.properties", func() { os.Remove(filepath.Join(dir, "source.properties")) }},
		{"revision prefix", func() { putFile(t, filepath.Join(dir, "source.properties"), "Pkg.Revision=37.0.10\n") }},
		{"revision suffix", func() { putFile(t, filepath.Join(dir, "source.properties"), "Pkg.Revision=137.0.1\n") }},
		{"two revisions", func() {
			putFile(t, filepath.Join(dir, "source.properties"), "Pkg.Revision=37.0.1\npkg.revision = 37.0.1\n")
		}},
		{"no revision", func() { putFile(t, filepath.Join(dir, "source.properties"), "Pkg.UserSrc=false\n") }},
	} {
		reset()
		c.change()
		if pinned.Complete(dir) {
			t.Errorf("%s: complete", c.name)
		}
	}
	for _, props := range []string{"\xef\xbb\xbf  Pkg.Revision = 37.0.1  \r\n", "#c\rPKG.REVISION=37.0.1", "Pkg.Revision=37.0.1"} {
		reset()
		putFile(t, filepath.Join(dir, "source.properties"), props)
		if !pinned.Complete(dir) {
			t.Errorf("%q: not complete", props)
		}
	}

	image := SDKComponent{ID: "system-image", Version: "14", StatusFiles: []string{"system.img"}, KeyFiles: []string{"system.img", "vendor.img"}}
	dir = testutil.TempDir(t)
	putFile(t, filepath.Join(dir, "system.img"), "x")
	putFile(t, filepath.Join(dir, "vendor.img"), "x")
	if !image.Complete(dir) {
		t.Fatal("an image needs no source.properties")
	}
	putFile(t, filepath.Join(dir, "source.properties"), "Pkg.Revision=13\n")
	if !image.Complete(dir) || Revision(dir) != "13" {
		t.Fatal("another revision of an image is complete")
	}
	putFile(t, filepath.Join(dir, "vendor.img"), "")
	if image.Complete(dir) || !image.Present(dir) {
		t.Fatal("an empty vendor.img is present for check, not complete for fetch")
	}
}

// TestPresentIsAUTOMASRule: check's rule stays AUTO-MAS's (the status files exist, any size), looser than Complete.
func TestPresentIsAUTOMASRule(t *testing.T) {
	pt := WindowsComponents()[0]
	dir := testutil.TempDir(t)
	if pt.Present(dir) {
		t.Fatal("empty directory")
	}
	putFile(t, filepath.Join(dir, "adb.exe"), "")
	if !pt.Present(dir) || pt.Complete(dir) {
		t.Fatal("adb.exe alone: present, not complete")
	}
}

func TestRevision(t *testing.T) {
	dir := testutil.TempDir(t)
	if Revision(dir) != "" {
		t.Fatal("no file")
	}
	for props, want := range map[string]string{
		"Pkg.Revision=37.0.1\n":                   "37.0.1",
		"Pkg.Revision=\npkg.revision = 14 \r\n":   "14",
		"\xef\xbb\xbfPkg.Revision=1\rx=2":         "1",
		"Pkg.RevisionX=3\nPkg.Revision.Major=4\n": "",
		"Pkg.Desc=x\n":                            "",
	} {
		putFile(t, filepath.Join(dir, "source.properties"), props)
		if got := Revision(dir); got != want {
			t.Errorf("%q: %q, want %q", props, got, want)
		}
	}
}

// TestComponentsUseTable: check and status take the directory and the files from the table.
func TestComponentsUseTable(t *testing.T) {
	r, err := New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range SDKComponents() {
		dir := c.Dir(r.SDK())
		for _, f := range c.StatusFiles {
			putFile(t, filepath.Join(dir, f), "")
		}
	}
	comps := r.Components()
	if !comps[0].Installed || !comps[2].Installed || comps[0].Name != PlatformToolsName || comps[2].Name != CurrentPlatform().SystemImageName {
		t.Fatalf("%+v", comps)
	}
	os.Remove(filepath.Join(SDKComponents()[1].Dir(r.SDK()), SDKComponents()[1].StatusFiles[0]))
	if r.Components()[2].Installed {
		t.Fatal("image without its first file")
	}
}
