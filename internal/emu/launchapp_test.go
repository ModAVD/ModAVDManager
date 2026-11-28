package emu

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

func TestParseAppTarget(t *testing.T) {
	for _, c := range []struct {
		in   string
		want AppTarget
	}{
		{"com.hypergryph.arknights", AppTarget{Package: "com.hypergryph.arknights"}},
		{"com.example.app/.Main", AppTarget{Package: "com.example.app", Activity: ".Main"}},
		{"com.example.app/com.example.app.ui.Main", AppTarget{Package: "com.example.app", Activity: "com.example.app.ui.Main"}},
		{"com.a_b.c9/_Main", AppTarget{Package: "com.a_b.c9", Activity: "_Main"}},
	} {
		got, err := ParseAppTarget(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: %+v, %v; want %+v", c.in, got, err, c.want)
		}
		if got.String() != c.in {
			t.Errorf("%q: String %q", c.in, got.String())
		}
	}
	for _, in := range []string{
		"", "/", "app", "com", "com.", ".com.x", "com..x", "com.x/", "com.x/.", "com.x/..Main", "com.x/a/b",
		"1com.x", "com.1x", "com._x", "com.x y", "com.x\n", " com.x", "com.x ",
		"com.x;reboot", "com.x&&reboot", "com.x|id", "com.x`id`", "$(id).x", "com.x$y", "com.x'y", "com.x\"y", "com.x>/sdcard/a",
		"com.x/.Main;reboot", "com.x/.Main$Inner", "com.x/.Main -a x", "com.x/$(id)", "com.x/.Mäin", "com.é.x",
		"-n.com.x", "com.x\x00", strings.Repeat("a", 300) + ".b",
	} {
		_, err := ParseAppTarget(in)
		if errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%q accepted: %v", in, err)
		}
		if bad := (AppTarget{Package: in}).Validate(); bad == nil {
			t.Errorf("%q passes Validate", in)
		}
	}
	if err := (AppTarget{Package: "com.x", Activity: ".A;b"}).Validate(); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("activity with ; passes Validate: %v", err)
	}
	if err := (AppTarget{Package: "com.x", Activity: ".A"}).Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// fakeApp is a guest for launchApp: canned answers per command, every command recorded, a clock that only moves when
// the code sleeps.
type fakeApp struct {
	t        *testing.T
	now      time.Time
	commands []string
	pmPath   adb.Result
	resolve  adb.Result
	amStart  adb.Result
	tops     []string // successive foregroundTopScript outputs; the last one repeats
	slept    time.Duration
	probes   int
}

func (f *fakeApp) guest() appGuest {
	f.now = time.Unix(1_000, 0)
	return appGuest{
		shell: func(command string, _ time.Duration) adb.Result {
			f.commands = append(f.commands, command)
			switch {
			case strings.HasPrefix(command, "pm path "):
				return f.pmPath
			case strings.HasPrefix(command, "cmd package resolve-activity "):
				return f.resolve
			case strings.HasPrefix(command, "am start "):
				return f.amStart
			case command == foregroundTopScript:
				k := min(f.probes, len(f.tops)-1)
				f.probes++
				return adb.Result{Out: f.tops[k]}
			}
			f.t.Errorf("unexpected command %q", command)
			return adb.Result{Code: 1}
		},
		sleep:   func(_ context.Context, d time.Duration) { f.now = f.now.Add(d); f.slept += d },
		now:     func() time.Time { return f.now },
		timeout: 3 * time.Second,
	}
}

func topOf(component string) string {
	return fmt.Sprintf("top:   topResumedActivity=ActivityRecord{1d5b2a5 u0 %s t5}\r\n", component)
}

const (
	installed = "package:/data/app/~~x/com.hypergryph.arknights-y/base.apk\r\n"
	resolved  = "priority=0 preferredOrder=0 match=0x108000 specificIndex=-1 isDefault=false\r\n" +
		"com.hypergryph.arknights/com.u8.sdk.U8UnityContext\r\n"
)

func TestLaunchAppResolvesLauncherActivity(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, resolve: adb.Result{Out: resolved},
		tops: []string{topOf("com.android.launcher3/.Launcher"), "top: topResumedActivity=null", topOf("com.hypergryph.arknights/com.u8.sdk.U8UnityContext")}}
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.hypergryph.arknights"})
	if !r.OK || r.Code != "" || r.Component != "com.hypergryph.arknights/com.u8.sdk.U8UnityContext" || r.Err() != nil {
		t.Fatalf("%+v", r)
	}
	want := []string{
		"pm path com.hypergryph.arknights",
		"cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.LAUNCHER com.hypergryph.arknights",
		"am start -n com.hypergryph.arknights/com.u8.sdk.U8UnityContext",
		foregroundTopScript, foregroundTopScript, foregroundTopScript,
	}
	if fmt.Sprint(f.commands) != fmt.Sprint(want) {
		t.Errorf("commands %q, want %q", f.commands, want)
	}
	if f.slept != 2*appPollEvery || r.Seconds != (2*appPollEvery).Seconds() {
		t.Errorf("slept %v, seconds %v", f.slept, r.Seconds)
	}
}

func TestLaunchAppGivenActivityNeedsNoLookup(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, tops: []string{topOf("com.example.app/.Main")}}
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if !r.OK || r.Component != "com.example.app/.Main" {
		t.Fatalf("%+v", r)
	}
	for _, c := range f.commands {
		if strings.HasPrefix(c, "cmd package") {
			t.Errorf("looked the activity up: %q", c)
		}
	}
	if got := f.commands[1]; got != "am start -n com.example.app/.Main" {
		t.Errorf("start command %q", got)
	}
}

// The app is in front already (this is the path of an instance that was running): am start prints its warning, the
// first probe says it is there, no waiting.
func TestLaunchAppAlreadyInFront(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed},
		amStart: adb.Result{Out: "Starting: Intent { cmp=com.example.app/.Main }\r\nWarning: Activity not started, its current task has been brought to the front\r\n"},
		tops:    []string{topOf("com.example.app/.Main")}}
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if !r.OK || f.slept != 0 {
		t.Fatalf("%+v slept %v", r, f.slept)
	}
}

func TestLaunchAppNotInstalled(t *testing.T) {
	for _, out := range []adb.Result{{Code: 1}, {Code: 0}, {Code: 1, Out: "\r\n"}} {
		f := &fakeApp{t: t, pmPath: out}
		r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.missing"})
		if r.OK || r.Code != errs.AppNotInstalled || !strings.Contains(r.Message, "com.example.missing") || r.Advice == "" {
			t.Fatalf("%+v", r)
		}
		if errs.CodeOf(r.Err()) != errs.AppNotInstalled || errs.ExitCode(r.Err()) != 3 {
			t.Errorf("err %v exit %d", r.Err(), errs.ExitCode(r.Err()))
		}
		if len(f.commands) != 1 {
			t.Errorf("went on after pm path: %q", f.commands)
		}
	}
}

func TestLaunchAppPackageManagerUnreachable(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Code: 1, Out: "error: device offline"}}
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app"})
	if r.OK || r.Code != errs.AppLaunchFailed || !strings.Contains(r.Message, "device offline") || errs.ExitCode(r.Err()) != 7 {
		t.Fatalf("%+v", r)
	}
}

func TestLaunchAppNoLauncherEntry(t *testing.T) {
	for _, out := range []string{"No activity found\r\n", "", "com.other.app/.Main\r\n", "com.example.app/.Main$Inner\r\n", "com.example.app/.A;reboot\r\n"} {
		f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, resolve: adb.Result{Out: out}}
		r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app"})
		if r.OK || r.Code != errs.AppLaunchFailed || r.Advice == "" {
			t.Fatalf("%q: %+v", out, r)
		}
		for _, c := range f.commands {
			if strings.HasPrefix(c, "am start") {
				t.Errorf("%q: started %q", out, c)
			}
		}
	}
}

func TestLaunchAppStartRefused(t *testing.T) {
	for _, out := range []adb.Result{
		{Out: "Starting: Intent { cmp=com.example.app/.Nope }\r\nError type 3\r\nError: Activity class {com.example.app/com.example.app.Nope} does not exist.\r\n"},
		{Code: 255, Out: "Exception occurred while executing 'start':\r\njava.lang.SecurityException: Permission Denial"},
		{Code: 1},
	} {
		f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, amStart: out, tops: []string{"top: topResumedActivity=null"}}
		r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Nope"})
		if r.OK || r.Code != errs.AppLaunchFailed {
			t.Fatalf("%+v", out)
		}
		if f.probes != 0 {
			t.Errorf("waited for the front after a refused start")
		}
	}
}

func TestLaunchAppNeverInFront(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed},
		tops: []string{topOf("com.android.permissioncontroller/.GrantPermissionsActivity")}}
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if r.OK || r.Code != errs.Timeout || !strings.Contains(r.Message, "permissioncontroller") || errs.ExitCode(r.Err()) != 6 {
		t.Fatalf("%+v", r)
	}
	if f.slept < 3*time.Second || f.slept > 4*time.Second {
		t.Errorf("waited %v for a timeout of 3 s", f.slept)
	}
	f = &fakeApp{t: t, pmPath: adb.Result{Out: installed}, tops: []string{"garbage"}}
	if r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"}); r.Code != errs.Timeout ||
		!strings.Contains(r.Message, "读不出来") {
		t.Fatalf("%+v", r)
	}
}

func TestLaunchAppCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, tops: []string{"top: topResumedActivity=null"}}
	g := f.guest()
	g.sleep = func(context.Context, time.Duration) { cancel() }
	r := launchApp(ctx, g, AppTarget{Package: "com.example.app", Activity: ".Main"})
	if r.OK || r.Code != errs.Canceled || errs.ExitCode(r.Err()) != 8 {
		t.Fatalf("%+v", r)
	}
}

// Whatever the guest echoes back is never made part of a command line unless it is a clean name.
func TestLaunchAppCommandsOnlyCarryCheckedNames(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, resolve: adb.Result{Out: resolved},
		tops: []string{topOf("com.hypergryph.arknights/com.u8.sdk.U8UnityContext")}}
	launchApp(context.Background(), f.guest(), AppTarget{Package: "com.hypergryph.arknights"})
	for _, c := range f.commands {
		if c != foregroundTopScript && strings.ContainsAny(c, ";&|`$'\"<>(){}\\\n") {
			t.Errorf("command with a shell character: %q", c)
		}
	}
}

func TestStartLaunchesAfterTheBoot(t *testing.T) {
	oldBoot, oldGuest := startBootedStep, appGuestOf
	defer func() { startBootedStep, appGuestOf = oldBoot, oldGuest }()
	m := &Manager{Log: logx.Nop()}
	var order []string
	for _, launched := range []bool{true, false} { // booted by this call / running already
		order = nil
		startBootedStep = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
			order = append(order, "boot")
			return &StartResult{Launched: launched, Warnings: []string{}}, nil
		}
		f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}, tops: []string{topOf("com.example.app/.Main")}}
		g := f.guest()
		inner := g.shell
		g.shell = func(c string, d time.Duration) adb.Result {
			if len(order) == 1 {
				order = append(order, "app")
			}
			return inner(c, d)
		}
		appGuestOf = func(*Manager, context.Context, int) appGuest { return g }
		res, err := m.Start(context.Background(), 3, StartOptions{Launch: &AppTarget{Package: "com.example.app", Activity: ".Main"}})
		if err != nil || res.Launched != launched || res.App == nil || !res.App.OK || res.App.Component != "com.example.app/.Main" {
			t.Fatalf("launched=%v: %+v, %v", launched, res, err)
		}
		if fmt.Sprint(order) != "[boot app]" {
			t.Errorf("order %v", order)
		}
	}
}

// An app that is not there leaves the start a success.
func TestStartLaunchFailureKeepsTheStart(t *testing.T) {
	oldBoot, oldGuest := startBootedStep, appGuestOf
	defer func() { startBootedStep, appGuestOf = oldBoot, oldGuest }()
	startBootedStep = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
		return &StartResult{Launched: true}, nil
	}
	f := &fakeApp{t: t, pmPath: adb.Result{Code: 1}}
	appGuestOf = func(*Manager, context.Context, int) appGuest { return f.guest() }
	res, err := (&Manager{Log: logx.Nop()}).Start(context.Background(), 3, StartOptions{Launch: &AppTarget{Package: "com.example.missing"}})
	if err != nil || !res.Launched || res.App == nil || res.App.OK || res.App.Code != errs.AppNotInstalled {
		t.Fatalf("%+v, %v", res, err)
	}
}

// A bad name stops the call before anything is booted; a boot that fails has no app result.
func TestStartLaunchChecksBeforeBooting(t *testing.T) {
	oldBoot := startBootedStep
	defer func() { startBootedStep = oldBoot }()
	booted := 0
	startBootedStep = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
		booted++
		return nil, errs.New(errs.LaunchFailed, "qemu exited")
	}
	m := &Manager{Log: logx.Nop()}
	if _, err := m.Start(context.Background(), 3, StartOptions{Launch: &AppTarget{Package: "com.x;reboot"}}); errs.CodeOf(err) != errs.InvalidArgument || booted != 0 {
		t.Fatalf("bad name: %v, boots %d", err, booted)
	}
	if _, err := m.Start(context.Background(), 3, StartOptions{Launch: &AppTarget{Package: "com.example.app", Activity: ".A$B"}}); errs.CodeOf(err) != errs.InvalidArgument || booted != 0 {
		t.Fatalf("bad activity: %v, boots %d", err, booted)
	}
	res, err := m.Start(context.Background(), 3, StartOptions{Launch: &AppTarget{Package: "com.example.app"}})
	if errs.CodeOf(err) != errs.LaunchFailed || res != nil || booted != 1 {
		t.Fatalf("failed boot: %+v, %v", res, err)
	}
}
