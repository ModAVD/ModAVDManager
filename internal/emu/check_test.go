//go:build windows

package emu

import (
	"errors"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
)

func TestAccelSummaryAndOKText(t *testing.T) {
	// What emulator.exe -accel-check prints on a WHPX host (frame lines around the explanation).
	out := "accel:\r\n0\r\nWHPX(10.0.26200) is installed and usable.\r\naccel\r\n"
	sum := accelSummary(out)
	if sum != "WHPX(10.0.26200) is installed and usable." {
		t.Fatalf("summary = %q", sum)
	}
	if got := accelOKText(sum); got != "可用（WHPX 10.0.26200）" {
		t.Fatalf("ok text = %q", got)
	}
	if got := accelOKText("something usable"); got != "可用" {
		t.Fatalf("ok text without version = %q", got)
	}
	// A failure: negative return code line dropped, two explanation lines joined.
	fail := "accel:\n-8\nWHPX is not installed\nPlease enable it\naccel\n"
	if got := accelSummary(fail); got != "WHPX is not installed Please enable it" {
		t.Fatalf("failure summary = %q", got)
	}
	// Only frame lines: fall back to the whole text.
	if got := accelSummary(" accel:\n0\n"); got != "accel:\n0" {
		t.Fatalf("frame-only summary = %q", got)
	}
	if got := accelSummary("-\n"); got != "-" {
		t.Fatalf(`"-" is not a number: %q`, got)
	}
}

func TestParseAccelCheck(t *testing.T) {
	cases := []struct {
		code int
		out  string
		ok   bool
	}{
		{0, "WHPX(10.0.26200) is installed and usable.", true},
		{0, "WHPX is installed and USABLE", true},
		{1, "WHPX(10.0.26200) is installed and usable.", false},
		{0, "WHPX is not installed", false},
	}
	for _, c := range cases {
		if got := parseAccelCheck(c.code, c.out); got.OK != c.ok {
			t.Errorf("parseAccelCheck(%d, %q).OK = %v, want %v", c.code, c.out, got.OK, c.ok)
		}
	}
	if got := parseAccelCheck(0, "  x \n"); got.Detail != "x" {
		t.Errorf("detail not trimmed: %q", got.Detail)
	}
}

func TestAccelItem(t *testing.T) {
	it := accelItem(accelResult{OK: true, Detail: "WHPX(10.0.26200) is installed and usable."}, "WHPX(10.0.26200) is installed and usable.")
	if it.ID != "acceleration" || it.OK == nil || !*it.OK || !it.Blocking || it.Reason != "可用（WHPX 10.0.26200）" {
		t.Fatalf("ok item = %+v", it)
	}
	it = accelItem(accelResult{OK: false}, "")
	if it.OK == nil || *it.OK || !strings.Contains(it.Reason, "检查结果：无输出") || it.Advice != accelAdvice {
		t.Fatalf("failed item = %+v", it)
	}
}

func TestPathItem(t *testing.T) {
	if it := pathItem(`D:\Mod AVD`); it.OK == nil || !*it.OK || it.Reason != `D:\Mod AVD` {
		t.Fatalf("ascii path with a space must pass: %+v", it)
	}
	it := pathItem(`D:\模拟器`)
	if it.OK == nil || *it.OK || !it.Blocking || !strings.Contains(it.Reason, `D:\模拟器`) || it.Advice == "" {
		t.Fatalf("non-ascii path must fail: %+v", it)
	}
}

func opsTestComponents(adb, emulator, image bool) []root.ComponentStatus {
	return []root.ComponentStatus{
		{ID: "platform-tools", Name: "平台工具（adb）", Installed: adb},
		{ID: "emulator", Name: "Android 模拟器", Installed: emulator},
		{ID: "system-image", Name: "系统镜像 Android 14 Google APIs x86_64", Installed: image},
		{ID: "launcher", Name: "轻量桌面 µLauncher", Optional: true},
		{ID: "ime", Name: "宿主输入法", Optional: true},
	}
}

func TestComponentsItem(t *testing.T) {
	advice := func(missing []string) string { return "补:" + strings.Join(missing, ",") }
	// The emulator is judged by its own item, optional ones not at all.
	it := componentsItem(opsTestComponents(true, false, true), advice)
	if it.OK == nil || !*it.OK || it.Reason != "平台工具（adb）、系统镜像 Android 14 Google APIs x86_64" {
		t.Fatalf("complete = %+v", it)
	}
	it = componentsItem(opsTestComponents(false, true, false), advice)
	if it.OK == nil || *it.OK || !it.Blocking {
		t.Fatalf("missing must fail: %+v", it)
	}
	if it.Reason != "这个目录里缺少：平台工具（adb）、系统镜像 Android 14 Google APIs x86_64" {
		t.Fatalf("reason = %q", it.Reason)
	}
	if it.Advice != "补:平台工具（adb）,系统镜像 Android 14 Google APIs x86_64" {
		t.Fatalf("advice = %q", it.Advice)
	}
	if got := requiredMissing(opsTestComponents(true, false, false)); len(got) != 1 || got[0] != "系统镜像 Android 14 Google APIs x86_64" {
		t.Fatalf("requiredMissing = %v", got)
	}
}

func TestEmulatorItem(t *testing.T) {
	it := emulatorItem(emulatorFacts{})
	if it.OK == nil || *it.OK || it.Reason != "这个目录里还没有魔改 AVD 内测包" || it.Advice != testPackageAdvice {
		t.Fatalf("absent = %+v", it)
	}
	it = emulatorItem(emulatorFacts{Present: true, Version: "37.1.11"})
	if it.OK == nil || *it.OK || it.Reason != "这里的模拟器（37.1.11）不是魔改 AVD 内测包里的版本，只支持内测包" {
		t.Fatalf("google build = %+v", it)
	}
	it = emulatorItem(emulatorFacts{Present: true})
	if !strings.Contains(it.Reason, "（版本未知）") {
		t.Fatalf("unknown version = %+v", it)
	}
	it = emulatorItem(emulatorFacts{Present: true, SelfBuilt: true, Version: "37.2.10", Shown: "0.35.0", Build: 35, HasBuild: true})
	if it.OK == nil || !*it.OK || it.Reason != "内测包 0.35.0（37.2.10）" {
		t.Fatalf("ok = %+v", it)
	}
	it = emulatorItem(emulatorFacts{Present: true, SelfBuilt: true, Version: "37.2.10", Shown: "0.24.0", Build: 24, HasBuild: true})
	if it.OK == nil || *it.OK || it.Reason != "内测包版本过旧（0.24.0）" || !strings.Contains(it.Advice, "0.25.0") {
		t.Fatalf("too old = %+v", it)
	}
	if strings.Contains(it.Advice, "「") {
		t.Fatalf("advice must not point at AUTO-MAS buttons: %q", it.Advice)
	}
	it = emulatorItem(emulatorFacts{Present: true, SelfBuilt: true, Version: "37.2.10"})
	if it.OK == nil || *it.OK || it.Reason != "内测包版本过旧（未标版本号）" {
		t.Fatalf("no build id = %+v", it)
	}
}

func TestMemoryItem(t *testing.T) {
	it := memoryItem(6144, 8000, nil)
	if it.OK == nil || !*it.OK || it.Reason != "实例内存 6 GB，启动约需 7.5 GB，当前可用 7.8 GB" {
		t.Fatalf("enough = %+v", it)
	}
	it = memoryItem(6144, 7679, nil)
	if it.OK == nil || *it.OK || !strings.HasPrefix(it.Reason, "电脑可用内存不足：实例内存 6 GB") || it.Advice == "" {
		t.Fatalf("short = %+v", it)
	}
	if it := memoryItem(6144, 7680, nil); !*it.OK {
		t.Fatalf("exactly enough must pass: %+v", it)
	}
	if it := memoryItem(4096, 0, errors.New("x")); it.OK != nil || !it.Blocking {
		t.Fatalf("unreadable = %+v", it)
	}
}

func TestDiskItem(t *testing.T) {
	it := diskItem(`E:\`, 8<<30, nil)
	if it.OK == nil || !*it.OK || it.Reason != `实例所在盘 E:\ 剩余 8.0 GB` {
		t.Fatalf("enough = %+v", it)
	}
	it = diskItem(`E:\`, 8<<30-1, nil)
	if it.OK == nil || *it.OK || !strings.Contains(it.Reason, "低于开机所需的 8 GB") || !strings.Contains(it.Advice, `E:\`) {
		t.Fatalf("short = %+v", it)
	}
	if it := diskItem(`E:\`, 0, errors.New("denied")); it.OK != nil || it.Reason != `查不了 E:\ 的剩余空间: denied` {
		t.Fatalf("unreadable = %+v", it)
	}
	if got := diskDrive(`D:\r\avd`); got != `D:\` {
		t.Fatalf("diskDrive = %q", got)
	}
	if got := diskDrive(`\\srv\share\avd`); got != `\\srv\share\` {
		t.Fatalf("diskDrive UNC = %q", got)
	}
	if got := diskExistingAncestor(`C:\Windows\no-such-dir-modavd\a\b`); !strings.EqualFold(got, `C:\Windows`) {
		t.Fatalf("diskExistingAncestor = %q", got)
	}
}

func TestFirstBlockingFailureAndPrecheckError(t *testing.T) {
	items := []CheckItem{
		{ID: "path", OK: checkOK(true), Blocking: true},
		{ID: "acceleration", OK: nil, Blocking: true},       // not checkable: does not stop the boot
		{ID: "vulkan", OK: checkOK(false), Blocking: false}, // only a warning
		{ID: "memory", OK: checkOK(false), Blocking: true, Reason: "r", Advice: "a"},
		{ID: "disk", OK: checkOK(false), Blocking: true},
	}
	bad, failed := firstBlockingFailure(items)
	if !failed || bad.ID != "memory" {
		t.Fatalf("first failure = %+v %v", bad, failed)
	}
	if _, failed := firstBlockingFailure(items[:3]); failed {
		t.Fatal("ok=nil and non-blocking items must not fail")
	}

	err := precheckError(bad, func() error { return nil })
	if errs.CodeOf(err) != errs.PrecheckFailed || err.Error() != "r。a" {
		t.Fatalf("memory -> %v (%s)", err, errs.CodeOf(err))
	}
	err = precheckError(CheckItem{ID: "emulator", Reason: "old"}, func() error { return nil })
	if errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("emulator -> %s", errs.CodeOf(err))
	}
	missing := errs.New(errs.NotReady, "这个目录里缺少：x")
	err = precheckError(CheckItem{ID: "components"}, func() error { return missing })
	if !errors.Is(err, missing) {
		t.Fatalf("components must use the root's error, got %v", err)
	}
	err = precheckError(CheckItem{ID: "components", Reason: "缺"}, func() error { return nil })
	if errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("components fallback -> %s", errs.CodeOf(err))
	}
}

func TestPrecheckSummary(t *testing.T) {
	got := precheckSummary([]CheckItem{
		{Title: "目录路径", OK: checkOK(true), Reason: `D:\r`},
		{Title: "硬件虚拟化", OK: nil, Reason: "没装"},
		{Title: "显卡 Vulkan", OK: checkOK(false), Reason: "没有"},
	})
	want := `魔改 AVD 开机前检查：目录路径通过（D:\r）；硬件虚拟化未查（没装）；显卡 Vulkan未通过（没有）`
	if got != want {
		t.Fatalf("summary\n got %s\nwant %s", got, want)
	}
	if itemMessage(CheckItem{Reason: "r"}) != "r" || itemMessage(CheckItem{Reason: "r", Advice: "a"}) != "r。a" {
		t.Fatal("itemMessage")
	}
}
