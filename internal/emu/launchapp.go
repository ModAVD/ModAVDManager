package emu

// start --launch: open one app once the instance is booted and its setup is done (StartOptions.Launch). The package
// and the activity go into guest shell command lines without any quoting, so they are checked here, before anything is
// booted, against the characters Java names are made of; what the guest itself prints (the resolved launcher
// component) is checked the same way before it is used.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
)

const (
	// AppLaunchTimeout is how long start --launch waits for the app to be the resumed activity.
	AppLaunchTimeout = 30 * time.Second
	appPollEvery     = 500 * time.Millisecond
	appMaxNameLen    = 255
)

// AppTarget is the app start --launch opens: a package, and optionally the activity (".Main" relative to the
// package, or the full class name); without one the package's launcher activity is looked up in the guest.
type AppTarget struct {
	Package  string
	Activity string
}

// ParseAppTarget reads "<package>" or "<package>/<activity>". The error is errs.InvalidArgument.
func ParseAppTarget(s string) (AppTarget, error) {
	pkg, act, hasAct := strings.Cut(s, "/")
	if !validAppPackage(pkg) {
		return AppTarget{}, errs.Newf(errs.InvalidArgument, "应用包名不对：「%s」（只能是 com.example.app 这样的 Java 包名，字母、数字、下划线和点）", s).
			WithAdvice("格式：<包名> 或 <包名>/<Activity>")
	}
	if hasAct && !validAppActivity(act) {
		return AppTarget{}, errs.Newf(errs.InvalidArgument, "Activity 名不对：「%s」（只能是类名，字母、数字、下划线和点，可以以点开头；不支持带 $ 的内部类）", act).
			WithAdvice("格式：<包名> 或 <包名>/<Activity>")
	}
	return AppTarget{Package: pkg, Activity: act}, nil
}

// Validate checks a target that did not come from ParseAppTarget.
func (a AppTarget) Validate() error {
	if !validAppPackage(a.Package) || a.Activity != "" && !validAppActivity(a.Activity) {
		s := a.Package
		if a.Activity != "" {
			s += "/" + a.Activity
		}
		_, err := ParseAppTarget(s)
		if err == nil {
			err = errs.Newf(errs.InvalidArgument, "应用包名不对：「%s」", s)
		}
		return err
	}
	return nil
}

func (a AppTarget) String() string {
	if a.Activity == "" {
		return a.Package
	}
	return a.Package + "/" + a.Activity
}

// validAppPackage: at least two dot-separated segments, each a letter followed by letters, digits and underscores.
func validAppPackage(s string) bool {
	if s == "" || len(s) > appMaxNameLen {
		return false
	}
	segs := strings.Split(s, ".")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		if !javaName(seg, false) {
			return false
		}
	}
	return true
}

// validAppActivity: a class name, as a manifest writes it: ".Main" (relative to the package) or "com.example.Main".
func validAppActivity(s string) bool {
	if s == "" || len(s) > appMaxNameLen {
		return false
	}
	s = strings.TrimPrefix(s, ".")
	for _, seg := range strings.Split(s, ".") {
		if !javaName(seg, true) {
			return false
		}
	}
	return true
}

// javaName: ASCII letters, digits and '_' (a class name may start with '_'; a package segment starts with a letter).
func javaName(s string, classStart bool) bool {
	if s == "" {
		return false
	}
	for k, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '_' && (k > 0 || classStart):
		case r >= '0' && r <= '9' && k > 0:
		default:
			return false
		}
	}
	return true
}

// AppLaunch is what start --launch did (StartResult.App). OK means the app is the resumed activity; otherwise Code
// (errs.AppNotInstalled, errs.AppLaunchFailed, errs.Timeout, errs.Canceled), Message and Advice say why. The instance
// itself is up either way.
type AppLaunch struct {
	Package string `json:"package"`
	// Component is the activity that was started, "<package>/<class>"; empty when none was found.
	Component string  `json:"component,omitempty"`
	OK        bool    `json:"ok"`
	Seconds   float64 `json:"seconds"`
	Code      string  `json:"code,omitempty"`
	Message   string  `json:"message,omitempty"`
	Advice    string  `json:"advice,omitempty"`
}

// Err is the failure as an error (nil when OK).
func (a *AppLaunch) Err() error {
	if a == nil || a.OK {
		return nil
	}
	return errs.New(a.Code, a.Message).WithAdvice(a.Advice)
}

// appGuest is how launchApp reaches the guest; tests use a fake with a clock.
type appGuest struct {
	shell   func(command string, timeout time.Duration) adb.Result
	sleep   func(ctx context.Context, d time.Duration)
	now     func() time.Time
	timeout time.Duration
}

// appGuestOf is how openApp reaches the guest; a test replaces it with a fake guest.
var appGuestOf = (*Manager).appGuest

func (m *Manager) appGuest(ctx context.Context, i int) appGuest {
	return appGuest{
		shell: func(command string, timeout time.Duration) adb.Result { return m.shell(ctx, i, command, timeout) },
		sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		},
		now:     time.Now,
		timeout: AppLaunchTimeout,
	}
}

// foregroundTopScript prints "top: " and the first topResumedActivity / mResumedActivity line of dumpsys (see
// foregroundProbeScript).
const foregroundTopScript = `echo "top: $(dumpsys activity activities | grep -m1 -E 'topResumedActivity|mResumedActivity')"`

// launchApp opens t in the guest and waits for it to be in front. It never returns an error: a failure is the result.
func launchApp(ctx context.Context, g appGuest, t AppTarget) *AppLaunch {
	start := g.now()
	res := &AppLaunch{Package: t.Package}
	fail := func(code, msg, advice string) *AppLaunch {
		res.OK, res.Code, res.Message, res.Advice = false, code, msg, advice
		res.Seconds = g.now().Sub(start).Seconds()
		if ctx.Err() != nil {
			res.Code, res.Message, res.Advice = errs.Canceled, "已取消", ""
		}
		return res
	}

	// pm path is exact (pm list packages matches by prefix) and prints nothing for a package that is not there.
	out := g.shell("pm path "+t.Package, 20*time.Second)
	switch installed, ok := parsePmPath(out); {
	case installed:
	case ok:
		return fail(errs.AppNotInstalled, fmt.Sprintf("实例里没有安装应用 %s", t.Package),
			"先装上这个应用（modavd install <i> <apk>，或在实例里自己装），再用 --launch 打开")
	default:
		return fail(errs.AppLaunchFailed, fmt.Sprintf("查不出应用 %s 装没装：%s", t.Package, tail(out.Out, 200)), "")
	}

	component := ""
	if t.Activity != "" {
		component = t.Package + "/" + t.Activity
	} else {
		out := g.shell("cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.LAUNCHER "+
			t.Package, 20*time.Second)
		c, ok := parseLauncherComponent(t.Package, out.Out)
		if !ok {
			return fail(errs.AppLaunchFailed, fmt.Sprintf("应用 %s 没有桌面启动入口，或查不出来：%s", t.Package, tail(out.Out, 200)),
				"知道 Activity 的话用 --launch 包名/Activity 直接指定")
		}
		component = c
	}
	res.Component = component

	if failed, why := amStartFailed(g.shell("am start -n "+component, 30*time.Second)); failed {
		return fail(errs.AppLaunchFailed, fmt.Sprintf("打开 %s 失败：%s", component, why), "")
	}

	deadline := g.now().Add(g.timeout)
	front := ""
	for {
		probe := g.shell(foregroundTopScript, 10*time.Second)
		if top, _, topOK, _ := parseForegroundProbe(probe.Out); topOK {
			if pkg, none, ok := resumedPackage(top); ok && !none {
				front = pkg
				if pkg == t.Package {
					res.OK = true
					res.Seconds = g.now().Sub(start).Seconds()
					return res
				}
			}
		}
		if ctx.Err() != nil || !g.now().Before(deadline) {
			break
		}
		g.sleep(ctx, appPollEvery)
	}
	if front == "" {
		front = "读不出来"
	}
	return fail(errs.Timeout, fmt.Sprintf("已发出打开 %s 的请求，但 %.0f 秒内它没有到前台（前台是：%s）",
		component, g.timeout.Seconds(), front),
		"应用可能还在弹权限或更新窗口，到实例里看一眼；也可能没起来，用 modavd shell 看 logcat")
}

// parsePmPath reads `pm path <pkg>`: installed when a "package:" line is there; ok=false when the output is neither
// that nor empty (adb or pm itself failed).
func parsePmPath(r adb.Result) (installed, ok bool) {
	text := strings.TrimSpace(r.Out)
	for _, line := range outputLines(text) {
		if strings.HasPrefix(strings.TrimSpace(line), "package:") {
			return true, true
		}
	}
	return false, text == ""
}

// parseLauncherComponent reads the last line of `cmd package resolve-activity --brief`: "<pkg>/<class>". The guest
// printed it, so it is checked like a name from the command line before it is put in a command.
func parseLauncherComponent(pkg, out string) (string, bool) {
	var last string
	for _, line := range outputLines(out) {
		if l := strings.TrimSpace(line); l != "" {
			last = l
		}
	}
	p, class, found := strings.Cut(last, "/")
	if !found || p != pkg || !validAppActivity(class) {
		return "", false
	}
	return last, true
}

// amStartFailed: `am start` printed an error (it does not always exit non-zero). The "Warning: Activity not started,
// its current task has been brought to the front" of an app that was running already is no failure.
func amStartFailed(r adb.Result) (bool, string) {
	text := strings.TrimSpace(r.Out)
	for _, line := range outputLines(text) {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "Error") || strings.Contains(l, "Exception") || strings.Contains(l, "Permission Denial") {
			return true, tail(text, 300)
		}
	}
	if r.Code != 0 {
		if text == "" {
			text = fmt.Sprintf("am start 退出码 %d", r.Code)
		}
		return true, tail(text, 300)
	}
	return false, ""
}

// openApp is the end of Start with StartOptions.Launch: the instance is up, so a failure here is the result's, not
// the call's.
func (m *Manager) openApp(ctx context.Context, i int, t AppTarget) *AppLaunch {
	m.Log.Infof("实例 %d 正在打开应用 %s", i, t)
	r := launchApp(ctx, appGuestOf(m, ctx, i), t)
	if r.OK {
		m.Log.Infof("实例 %d 已打开应用 %s（%.1f 秒）", i, r.Component, r.Seconds)
	} else {
		m.Log.Warnf("实例 %d 没能打开应用 %s：%s", i, t, r.Message)
	}
	return r
}
