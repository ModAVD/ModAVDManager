package emu

import (
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
)

func TestEarlyBootState(t *testing.T) {
	for _, c := range []struct {
		name string
		res  adb.Result
		want bootState
		ok   bool
	}{
		{"booting", adb.Result{Out: "zygote=running booted=\r\n"}, bootState{zygote: true}, true},
		{"booted", adb.Result{Out: "zygote=running booted=1\n"}, bootState{zygote: true, booted: true}, true},
		{"zygote not yet", adb.Result{Out: "zygote= booted=\n"}, bootState{}, true},
		{"zygote restarting", adb.Result{Out: "zygote=restarting booted=\n"}, bootState{}, true},
		{"noise before", adb.Result{Out: "WARNING: linker\nzygote=running booted=\n"}, bootState{zygote: true}, true},
		{"adb failed", adb.Result{Code: 1, Out: "error: device offline"}, bootState{}, false},
		{"no line", adb.Result{Out: "something else\n"}, bootState{}, false},
	} {
		got, ok := earlyBootState(c.res)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if !strings.Contains(earlyBootProbe, "getprop init.svc.zygote") || !strings.Contains(earlyBootProbe, "getprop sys.boot_completed") {
		t.Errorf("probe %q", earlyBootProbe)
	}
}

func TestPixelLauncherListed(t *testing.T) {
	for out, want := range map[string]bool{
		"package:com.google.android.apps.nexuslauncher\r\n":     true,
		"package:com.google.android.apps.nexuslauncher.extra\n": false,
		"": false,
		"package:de.jrpie.android.launcher\npackage:com.google.android.apps.nexuslauncher": true,
	} {
		if got := pixelLauncherListed(out); got != want {
			t.Errorf("%q: %v", out, got)
		}
	}
}

func TestPixelLauncherRemoveResult(t *testing.T) {
	for _, c := range []struct {
		res       adb.Result
		ok, saved bool
	}{
		{adb.Result{Out: "Success\r\nSAVED\r\n"}, true, true},
		{adb.Result{Out: "Success\nUNSAVED\n"}, true, false},
		{adb.Result{Code: 1, Out: "Failure [not installed for 0]\n"}, false, false},
		{adb.Result{Code: -1, Out: "adb shell 超时（45 秒）\nSAVED"}, false, false},
		{adb.Result{Out: "Success\n"}, false, false},
	} {
		if ok, saved := pixelLauncherRemoveResult(c.res); ok != c.ok || saved != c.saved {
			t.Errorf("%+v: %v %v", c.res, ok, saved)
		}
	}
}

func TestPixelLauncherRemoveReport(t *testing.T) {
	for _, c := range []struct {
		ok, saved bool
		out       string
		warn      bool
		has       string
	}{
		{true, true, "Success\nSAVED\n", false, "实例 2 做：已卸载并写盘"},
		{true, false, "Success\nUNSAVED\n", true, "实例 2 做：已卸载，但 20 秒内没见系统写盘，下次开机会再检查"},
		{false, false, "Failure [DELETE_FAILED_INTERNAL_ERROR]\n", true, "实例 2 做失败，下次开机再试: Failure [DELETE_FAILED_INTERNAL_ERROR]"},
		{false, false, "", true, "失败，下次开机再试: 无输出"},
	} {
		r := pixelLauncherRemoveReport(2, "做", c.ok, c.saved, c.out)
		if r.warn != c.warn || !strings.Contains(r.msg, c.has) {
			t.Errorf("%+v: %+v", c, r)
		}
	}
}

// The removal runs inside `su 0 sh -c '...'` (no single quote), uninstalls for user 0 only, and waits on the file
// the package manager writes the per-user state to.
func TestPixelLauncherRemoveScript(t *testing.T) {
	s := pixelLauncherRemoveScript
	for _, want := range []string{"pm uninstall --user 0 " + pixelLauncherPackage + " || exit 1",
		"/data/system/users/0/package-restrictions.xml", "stat -c %y", "echo SAVED", "echo UNSAVED", "-lt 40", "sleep 0.5"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q: %s", want, s)
		}
	}
	if strings.Contains(s, "'") {
		t.Error("script has a single quote")
	}
	if strings.Index(s, "b=$(stat") < strings.Index(s, "pm uninstall") {
		t.Error("the time before the write is taken before pm returns")
	}
	if pixelLauncherRemoveTimeout <= 20*time.Second {
		t.Errorf("timeout %v does not cover the 20 s wait", pixelLauncherRemoveTimeout)
	}
}
