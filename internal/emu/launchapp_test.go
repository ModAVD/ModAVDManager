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

// fakeApp is a guest for launchApp: canned answers per command, every command recorded. The wait for the front is
// short in real time (200 ms, a look every 5 ms).
type fakeApp struct {
	t        *testing.T
	commands []string
	pmPath   adb.Result
	resolve  adb.Result
	amStart  adb.Result
	tops     []string // successive foregroundTopScript outputs; the last one repeats
	probes   int
	// probe, when set, answers a look at the front instead of tops (k counts from 0); it gets the context of the call.
	probe func(ctx context.Context, k int) adb.Result
}

func (f *fakeApp) guest() appGuest {
	return appGuest{
		shell: func(ctx context.Context, command string, _ time.Duration) adb.Result {
			f.commands = append(f.commands, command)
			switch {
			case strings.HasPrefix(command, "pm path "):
				return f.pmPath
			case strings.HasPrefix(command, "cmd package resolve-activity "):
				return f.resolve
			case strings.HasPrefix(command, "am start "):
				return f.amStart
			case command == foregroundTopScript:
				k := f.probes
				f.probes++
				if f.probe != nil {
					return f.probe(ctx, k)
				}
				return adb.Result{Out: f.tops[min(k, len(f.tops)-1)]}
			}
			f.t.Errorf("unexpected command %q", command)
			return adb.Result{Code: 1}
		},
		timeout: 200 * time.Millisecond,
		poll:    5 * time.Millisecond,
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
	if f.probes != 3 || r.Seconds < (2*5*time.Millisecond).Seconds() {
		t.Errorf("probes %d, seconds %v", f.probes, r.Seconds)
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
	if !r.OK || f.probes != 1 {
		t.Fatalf("%+v probes %d", r, f.probes)
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
	began := time.Now()
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if r.OK || r.Code != errs.Timeout || !strings.Contains(r.Message, "permissioncontroller") || errs.ExitCode(r.Err()) != 6 {
		t.Fatalf("%+v", r)
	}
	if d := time.Since(began); d < 200*time.Millisecond || d > 3*time.Second {
		t.Errorf("waited %v for a timeout of 200 ms", d)
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
	f.probe = func(context.Context, int) adb.Result {
		cancel()
		return adb.Result{Out: topOf("com.example.app/.Main")} // the app is there, but the call was canceled meanwhile
	}
	r := launchApp(ctx, f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if r.OK || r.Code != errs.Canceled || errs.ExitCode(r.Err()) != 8 {
		t.Fatalf("%+v", r)
	}
}

// One deadline covers the whole wait: an adb call that hangs ends with it (it gets the deadline's context), and a
// success that only comes back after the deadline is not the app being in front.
func TestLaunchAppDeadlineCoversEveryProbe(t *testing.T) {
	f := &fakeApp{t: t, pmPath: adb.Result{Out: installed}}
	f.probe = func(ctx context.Context, k int) adb.Result {
		<-ctx.Done() // a hanging adb: only the context ends it
		return adb.Result{Code: -1}
	}
	began := time.Now()
	r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"})
	if r.OK || r.Code != errs.Timeout || f.probes != 1 || time.Since(began) > 3*time.Second {
		t.Fatalf("hanging probe: %+v, probes %d, took %v", r, f.probes, time.Since(began))
	}

	f = &fakeApp{t: t, pmPath: adb.Result{Out: installed}}
	f.probe = func(ctx context.Context, k int) adb.Result {
		time.Sleep(400 * time.Millisecond) // does not look at ctx: answers late, past the 200 ms deadline
		return adb.Result{Out: topOf("com.example.app/.Main")}
	}
	if r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"}); r.OK || r.Code != errs.Timeout {
		t.Fatalf("late success counted: %+v", r)
	}

	// looks that come in time still win before the deadline, however slow each one is
	f = &fakeApp{t: t, pmPath: adb.Result{Out: installed}}
	f.probe = func(ctx context.Context, k int) adb.Result {
		time.Sleep(30 * time.Millisecond)
		if k < 3 {
			return adb.Result{Out: "top: topResumedActivity=null"}
		}
		return adb.Result{Out: topOf("com.example.app/.Main")}
	}
	if r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: ".Main"}); !r.OK || f.probes != 4 {
		t.Fatalf("slow looks: %+v, probes %d", r, f.probes)
	}
}

// The word Exception in an activity name is no error: only the diagnostic lines of am count.
func TestLaunchAppActivityNameWithException(t *testing.T) {
	for _, activity := range []string{".ExceptionActivity", ".UncaughtExceptionHandlerActivity", "com.example.app.ErrorActivity"} {
		f := &fakeApp{t: t, pmPath: adb.Result{Out: installed},
			amStart: adb.Result{Out: "Starting: Intent { cmp=com.example.app/" + activity + " }\r\nWarning: Activity not started, its current task has been brought to the front\r\n"},
			tops:    []string{topOf("com.example.app/" + activity)}}
		if r := launchApp(context.Background(), f.guest(), AppTarget{Package: "com.example.app", Activity: activity}); !r.OK {
			t.Errorf("%s: %+v", activity, r)
		}
	}
	for _, out := range []string{
		"Starting: Intent { cmp=com.example.app/.Main }\r\nError: Activity class {com.example.app/com.example.app.Main} does not exist.",
		"Starting: Intent { cmp=com.example.app/.Main }\r\nError type 3",
		"Exception occurred while executing 'start':\r\njava.lang.SecurityException: Permission Denial: starting Intent",
		"java.lang.SecurityException: Permission Denial: starting Intent",
	} {
		if failed, _ := amStartFailed(adb.Result{Out: out}); !failed {
			t.Errorf("not taken as an error: %q", out)
		}
	}
	if failed, _ := amStartFailed(adb.Result{Out: "Starting: Intent { cmp=com.example.app/.ExceptionActivity }"}); failed {
		t.Error("the echoed activity name was taken as an error")
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
		g.shell = func(ctx context.Context, c string, d time.Duration) adb.Result {
			if len(order) == 1 {
				order = append(order, "app")
			}
			return inner(ctx, c, d)
		}
		appGuestOf = func(*Manager, int) appGuest { return g }
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
	appGuestOf = func(*Manager, int) appGuest { return f.guest() }
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
