package components

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// TestFetchUnsupportedPlatform: linux has no catalogue; builds without native macOS component support have no
// darwin catalogue either. The code (not just the text) says so, before anything is read or written.
func TestFetchUnsupportedPlatform(t *testing.T) {
	old := fetchGOOS
	t.Cleanup(func() { fetchGOOS = old })
	dir := testutil.TempDir(t)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	gooses := []string{"linux"}
	if !nativeDarwinComponents {
		gooses = append(gooses, "darwin")
	}
	for _, goos := range gooses {
		fetchGOOS = goos
		_, err := Fetch(context.Background(), r, Options{AcceptLicense: true, MirrorURLs: []string{"http://127.0.0.1:1/"}})
		if errs.CodeOf(err) != errs.Unsupported || errs.ExitCode(err) != 4 {
			t.Errorf("%s: %v (%s)", goos, err, errs.CodeOf(err))
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}

func TestLicenseUnsupportedPlatform(t *testing.T) {
	old := fetchGOOS
	t.Cleanup(func() { fetchGOOS = old })
	gooses := []string{"linux"}
	if !nativeDarwinComponents {
		gooses = append(gooses, "darwin")
	}
	for _, goos := range gooses {
		fetchGOOS = goos
		_, _, err := License(context.Background(), SourceSettings{MirrorURLs: []string{"http://127.0.0.1:1/"}})
		if errs.CodeOf(err) != errs.Unsupported || errs.ExitCode(err) != 4 {
			t.Errorf("%s: %v (%s)", goos, err, errs.CodeOf(err))
		}
	}
}

// TestDefaultMirrors: the script's three mirrors in its order; --mirror picks one in any case; --mirror-url replaces
// them (repeated or comma-separated, "/" added) and wins over --mirror, which is still checked.
func TestDefaultMirrors(t *testing.T) {
	want := []Mirror{
		{"googledownloads_cn", "Google 中国下载（googledownloads.cn）", "https://googledownloads.cn/android/repository/"},
		{"tencent", "腾讯云镜像（mirrors.cloud.tencent.com）", "https://mirrors.cloud.tencent.com/AndroidSDK/"},
		{"google", "Google 官方（dl.google.com）", "https://dl.google.com/android/repository/"},
	}
	got, err := ChooseMirrors("", nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("mirror %d: %+v want %+v", k, got[k], want[k])
		}
	}
	for _, id := range []string{"tencent", "TENCENT", " Tencent "} {
		if got, err := ChooseMirrors(id, nil); err != nil || len(got) != 1 || got[0] != want[1] {
			t.Errorf("%q: %v %v", id, got, err)
		}
	}
	if _, err := ChooseMirrors("aliyun", nil); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("unknown mirror: %v", err)
	}
	if _, err := ChooseMirrors("aliyun", []string{"http://a/"}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("unknown mirror with urls: %v", err)
	}
	got, err = ChooseMirrors("google", []string{"http://a/x, https://b/y/", "http://c"})
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	for k, base := range []string{"http://a/x/", "https://b/y/", "http://c/"} {
		if got[k].Base != base || got[k].ID != "custom" || got[k].Name != "自定义镜像（"+base+"）" {
			t.Errorf("custom %d: %+v", k, got[k])
		}
	}
	for _, bad := range [][]string{{" , "}, {"ftp://a/"}, {"a/b"}} {
		if _, err := ChooseMirrors("", bad); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestCheckTimeouts(t *testing.T) {
	var o Options
	if err := o.CheckTimeouts(); err != nil || o.ConnectTimeout != 20*time.Second || o.StallTimeout != 30*time.Second ||
		o.ManifestTimeout != 40*time.Second {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	for _, o := range []Options{{ConnectTimeout: 601 * time.Second}, {StallTimeout: 601 * time.Second},
		{ManifestTimeout: 1801 * time.Second}, {StallTimeout: -time.Second}} {
		if err := o.CheckTimeouts(); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%+v: %v", o, err)
		}
	}
	o = Options{ConnectTimeout: 600 * time.Second, StallTimeout: 600 * time.Second, ManifestTimeout: 1800 * time.Second}
	if err := o.CheckTimeouts(); err != nil {
		t.Fatal(err)
	}
}

// TestFormat: the script's Format-Size and Format-Duration, and its progress line.
func TestFormat(t *testing.T) {
	for b, want := range map[float64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "2 KB", 1 << 20: "1.0 MB",
		8044989: "7.7 MB", 1563721130: "1.46 GB", 4500 << 20: "4.39 GB"} {
		if got := formatSize(b); got != want {
			t.Errorf("formatSize(%v) = %q, want %q", b, got, want)
		}
	}
	for s, want := range map[float64]string{-1: "--:--", 0: "00:00", 59.4: "00:59", 61: "01:01", 2.5: "00:02",
		3599: "59:59", 3600: "1:00:00", 3725: "1:02:05", 36000: "10:00:00"} {
		if got := formatDuration(s); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", s, got, want)
		}
	}
	var got []Progress
	m := newMeter(func(p Progress) { got = append(got, p) }, "downloading", "平台工具（adb）", 8044989, 0)
	m.add(1 << 20)
	m.add(2 << 20) // within 500 ms: dropped
	m.finish(8044989)
	if len(got) != 2 || !got[1].Final {
		t.Fatalf("%+v", got)
	}
	if want := "  平台工具（adb）   13.0%  1.0 MB / 7.7 MB  0 B/s  剩余 --:--"; got[0].Message != want {
		t.Errorf("progress line %q\nwant          %q", got[0].Message, want)
	}
	if !strings.HasPrefix(got[1].Message, "  平台工具（adb）  100.0%  7.7 MB / 7.7 MB  平均 ") || !strings.HasSuffix(got[1].Message, "/s") {
		t.Errorf("final line %q", got[1].Message)
	}
	got = nil
	m = newMeter(func(p Progress) { got = append(got, p) }, "extracting", "系统镜像", 4<<30, 0)
	m.add(1 << 30)
	m.finish(4 << 30)
	if got[0].Message != "  解压 系统镜像   25.0%  1.00 GB / 4.00 GB" || got[1].Message != "  解压 系统镜像  100.0%  4.00 GB" {
		t.Errorf("%q / %q", got[0].Message, got[1].Message)
	}
}

func TestLicenseText(t *testing.T) {
	doc := "\xef\xbb\xbf<?xml version=\"1.0\"?>\n<sdk:sdk-repository xmlns:sdk=\"http://example.invalid/repo\">" +
		"<license id=\"other\">no</license><sdk:license id=\"android-sdk-license\" type=\"text\">\n  Terms &amp; Conditions\r\n\r\nLine 2  \n" +
		"</sdk:license><license id=\"android-sdk-license\">second</license></sdk:sdk-repository>"
	got, err := licenseText([]byte(doc), "android-sdk-license")
	if err != nil || got != "Terms & Conditions\n\nLine 2" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := licenseText([]byte(`<r><license id="x">a</license></r>`), "android-sdk-license"); err == nil ||
		!strings.Contains(err.Error(), "清单里没有许可协议") {
		t.Errorf("missing: %v", err)
	}
	if _, err := licenseText([]byte(`<r><license id="android-sdk-license">a</license><broken></r>`), "android-sdk-license"); err == nil ||
		!strings.Contains(err.Error(), "不是有效的 XML") {
		t.Errorf("broken document: %v", err)
	}
	if _, err := licenseText([]byte("<html>captive portal</html>"), "android-sdk-license"); err == nil {
		t.Error("an HTML page gave a license")
	}
}

// TestSetAsideRel: the result names a set-aside directory relative to the root.
func TestSetAsideRel(t *testing.T) {
	sdk := filepath.Join(testutil.TempDir(t), "sdk")
	got := setAsideRel(sdk, filepath.Join(sdk, "platform-tools.incomplete-20261101-120000"))
	if got != filepath.Join("sdk", "platform-tools.incomplete-20261101-120000") || setAsideRel(sdk, "") != "" {
		t.Fatal(got)
	}
}
