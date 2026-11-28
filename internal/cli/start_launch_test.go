//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/i18n"
)

func startWithApp(launched bool, app *emu.AppLaunch, got *[]emu.StartOptions) func(context.Context, int, emu.StartOptions) (*emu.StartResult, error) {
	return func(_ context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		if got != nil {
			*got = append(*got, opt)
		}
		r := &emu.StartResult{Instance: sampleInstance(i), Launched: launched, Warnings: []string{}}
		if opt.Launch != nil {
			r.App = app
		}
		return r, nil
	}
}

func TestStartLaunchReachesStart(t *testing.T) {
	for _, c := range []struct {
		arg  string
		want emu.AppTarget
	}{
		{"com.hypergryph.arknights", emu.AppTarget{Package: "com.hypergryph.arknights"}},
		{"com.example.app/.Main", emu.AppTarget{Package: "com.example.app", Activity: ".Main"}},
	} {
		for _, launched := range []bool{true, false} { // not running before / running already
			h := newHarness()
			var got []emu.StartOptions
			h.fake.start = startWithApp(launched, &emu.AppLaunch{Package: c.want.Package, Component: c.want.Package + "/.Main", OK: true, Seconds: 2.5}, &got)
			r := h.run(t, nil, "start", "3", "--window", "--launch", c.arg)
			if r.code != 0 || len(got) != 1 || got[0].Launch == nil || *got[0].Launch != c.want || !got[0].Window {
				t.Fatalf("%s launched=%v: exit %d %q options %+v", c.arg, launched, r.code, r.stderr, got)
			}
			if !strings.Contains(r.stdout, "已打开应用 "+c.want.Package+"/.Main") {
				t.Errorf("summary: %q", r.stdout)
			}
		}
	}
	// without --launch nothing is asked for and nothing is printed about an app
	h := newHarness()
	var got []emu.StartOptions
	h.fake.start = startWithApp(true, nil, &got)
	r := h.run(t, nil, "start", "3")
	if r.code != 0 || got[0].Launch != nil || strings.Contains(r.stdout, "应用") {
		t.Errorf("no --launch: exit %d options %+v stdout %q", r.code, got, r.stdout)
	}
}

func TestStartLaunchArgumentsAreCheckedFirst(t *testing.T) {
	for _, arg := range []string{
		"", "app", "com.x;reboot", "com.x && reboot", "$(reboot).x", "com.x|id", "com.x`id`", "com.x'", "com.x\"", "com.x\nreboot",
		"com.x/.Main;reboot", "com.x/.Main$1", "com.x/", "/.Main", "com.x/a/b", "--window", "-n", "com.x -a y",
	} {
		h := newHarness()
		var got []emu.StartOptions
		h.fake.start = startWithApp(true, nil, &got)
		r := h.run(t, nil, "start", "3", "--launch="+arg, "--json")
		if r.code != 2 || len(got) != 0 || len(h.fake.callList()) != 0 {
			t.Errorf("%q: exit %d, %d starts, calls %q, stdout %s", arg, r.code, len(got), h.fake.callList(), r.stdout)
			continue
		}
		var env struct {
			OK    bool
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || env.OK || env.Error.Code != errs.InvalidArgument {
			t.Errorf("%q: %s", arg, r.stdout)
		}
	}
	h := newHarness()
	if r := h.run(t, nil, "start", "3", "--launch"); r.code != 2 {
		t.Errorf("--launch without a value: exit %d", r.code)
	}
}

// The instance is up, the app is not installed: the start is a success (ok, the instance in the result) but the exit
// code says what went wrong, and the result carries the error for scripts.
func TestStartLaunchAppNotInstalled(t *testing.T) {
	app := &emu.AppLaunch{Package: "com.example.missing", Code: errs.AppNotInstalled,
		Message: "实例里没有安装应用 com.example.missing", Advice: "先装上这个应用"}
	h := newHarness()
	h.fake.start = startWithApp(true, app, nil)
	r := h.run(t, nil, "start", "3", "--launch", "com.example.missing", "--json")
	if r.code != 3 {
		t.Fatalf("exit %d, want 3: %s", r.code, r.stdout)
	}
	var env struct {
		OK   bool
		Data struct {
			Launched bool
			Instance struct{ Index int }
			App      emu.AppLaunch
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || !env.OK || !env.Data.Launched || env.Data.Instance.Index != 3 ||
		env.Data.App.OK || env.Data.App.Code != "app_not_installed" || env.Data.App.Message == "" || env.Data.App.Advice == "" {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	if strings.Count(r.stdout, "\n") != 1 {
		t.Errorf("not one line: %q", r.stdout)
	}
	// the same in words
	r = h.run(t, nil, "start", "3", "--launch", "com.example.missing")
	if r.code != 3 || !strings.Contains(r.stdout, "没能打开应用 com.example.missing（app_not_installed）：实例里没有安装应用") ||
		!strings.Contains(r.stdout, "建议：先装上这个应用") || !strings.Contains(r.stdout, "已开机") {
		t.Errorf("human: exit %d stdout %q", r.code, r.stdout)
	}
}

// The words of the failure come out in English where the user's language is English (the messages are built from
// catalog keys, not stored as Chinese text).
func TestStartLaunchEnglish(t *testing.T) {
	t.Setenv("MODAVD_LANG", "en")
	i18n.Set("en")
	t.Cleanup(func() { i18n.Set(i18n.Chinese) })
	for _, c := range []struct {
		app  *emu.AppLaunch
		want []string
	}{
		{&emu.AppLaunch{Package: "com.example.missing", Code: errs.AppNotInstalled, Message: "实例里没有安装应用 com.example.missing",
			Advice: "先装上这个应用（modavd install <i> <apk>，或在实例里自己装），再用 --launch 打开"},
			[]string{"Could not open app com.example.missing (app_not_installed): App com.example.missing is not installed in the instance",
				"Suggestion: Install the app first"}},
		{&emu.AppLaunch{Package: "com.example.app", Component: "com.example.app/.Main", OK: true, Seconds: 1.5},
			[]string{"Opened app com.example.app/.Main (1.5 s)"}},
	} {
		h := newHarness()
		h.fake.start = startWithApp(true, c.app, nil)
		r := h.run(t, nil, "start", "3", "--launch", "com.example.app")
		for _, want := range c.want {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("stdout lacks %q: %q", want, r.stdout)
			}
		}
	}
}

func TestStartLaunchExitCodes(t *testing.T) {
	for _, c := range []struct {
		code string
		exit int
	}{{errs.AppNotInstalled, 3}, {errs.Timeout, 6}, {errs.AppLaunchFailed, 7}, {errs.Canceled, 8}} {
		h := newHarness()
		h.fake.start = startWithApp(false, &emu.AppLaunch{Package: "com.example.app", Code: c.code, Message: "m"}, nil)
		if r := h.run(t, nil, "start", "3", "--launch", "com.example.app"); r.code != c.exit {
			t.Errorf("%s: exit %d, want %d", c.code, r.code, c.exit)
		}
	}
}

func TestStartLaunchDocumented(t *testing.T) {
	h := newHarness()
	out := h.run(t, nil, "help", "start", "--json").stdout
	for _, want := range []string{"--launch", "app_not_installed", "app_launch_failed", "包名[/Activity]"} {
		if !strings.Contains(out, want) {
			t.Errorf("help start lacks %q: %s", want, out)
		}
	}
}
