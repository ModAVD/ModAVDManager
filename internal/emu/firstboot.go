//go:build windows

package emu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// The light launcher shipped in <root>\components (AUTO-MAS constants LIGHT_LAUNCHER*): µLauncher 0.2.12, F-Droid
// developer-signed, MIT. It shows in the display's natural orientation (ours is landscape). Pinned to 0.2.12: another
// version needs the keys, files and version below checked against its source again.
const (
	lightLauncherPackage = "de.jrpie.android.launcher"
	lightLauncherHome    = "de.jrpie.android.launcher/.ui.HomeActivity"
	// its PreferenceManager.getDefaultSharedPreferences file
	lightLauncherPrefsFile = "/data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml"
	// the preference version it writes with its defaults on first start (Preferences.kt PREFERENCE_VERSION, not
	// versionCode 57)
	lightLauncherPrefsVersion = 101
	// only to start its process in the background so it writes its default preferences: an empty broadcast to this
	// receiver (which does not know it and does nothing)
	lightLauncherWakeReceiver = "de.jrpie.android.launcher/.actions.lock.LauncherDeviceAdmin"
	// the stock launcher the light one replaces
	pixelLauncherPackage = "com.google.android.apps.nexuslauncher"
)

// sharedPref is one preset preference: Value is a bool (<boolean>) or a string (<string>).
type sharedPref struct {
	Key   string
	Value any
}

// lightLauncherPresetPrefs (AUTO-MAS LIGHT_LAUNCHER_PRESET_PREFS): key names are the string values of
// res/values/donottranslate.xml. internal.started_before = onboarding done; functionality.search_auto_keyboard =
// keyboard on opening the app list (in landscape the IME goes full screen and hides the whole list); a double tap on
// the home screen locks by default, which needs device admin and pops "add device admin" without it, so it becomes
// the built-in "do nothing" (deleting the key shows a "cannot open" toast instead).
var lightLauncherPresetPrefs = []sharedPref{
	{"internal.started_before", true},
	{"functionality.search_auto_keyboard", false},
	{"action.double_click", `{"type":"action:launcher","value":"nop"}`},
}

// debloatPackages are disabled after the first boot (`pm disable-user --user 0`, `pm enable` undoes it): about
// 480 MB of guest RAM. Google services, WebView, Chrome, keyboards, the file picker, the installer, network and
// telephony stay (AUTO-MAS DEBLOAT_PACKAGES).
var debloatPackages = []string{
	"com.google.android.googlequicksearchbox",
	"com.google.android.youtube",
	"com.google.android.apps.youtube.music",
	"com.google.android.gm",
	"com.google.android.apps.messaging",
	"com.google.android.dialer",
	"com.google.android.contacts",
	"com.google.android.apps.photos",
	"com.google.android.apps.maps",
	"com.google.android.apps.docs",
	"com.google.android.calendar",
	"com.google.android.deskclock",
	"com.google.android.apps.wellbeing",
	"com.google.android.as",
	"com.google.android.as.oss",
	"com.google.android.projection.gearhead",
	"com.google.android.marvin.talkback",
	"com.google.android.apps.restore",
	"com.google.android.feedback",
	"com.google.android.printservice.recommendation",
	"com.google.android.markup",
}

// firstBootInit is the persistent setup after the first boot (AUTO-MAS _first_boot_init), recorded in mas-avd.json:
// Bluetooth off, no immersive-mode hint, phone apps disabled, the light launcher, the renderer. A failed item leaves
// initialized=false and the next boot tries again.
func (m *Manager) firstBootInit(ctx context.Context, i int) {
	m.Log.Infof("魔改 AVD 实例 %d 首次开机，正在初始化", i)
	var failures []string

	// Wi-Fi is not turned off here: every boot turns it on with the fastpath (setupNetwork)
	if res := m.shell(ctx, i, "svc bluetooth disable ; settings put secure immersive_mode_confirmations confirmed", 0); res.Code != 0 {
		failures = append(failures, "基础设置: "+res.Out)
	}
	disable := make([]string, len(debloatPackages))
	for k, p := range debloatPackages {
		disable[k] = "pm disable-user --user 0 " + p
	}
	if res := m.shell(ctx, i, strings.Join(disable, " ; "), 120*time.Second); res.Code != 0 {
		failures = append(failures, "禁用预装应用: "+tail(res.Out, 200))
	}

	launcher := m.installLauncher(ctx, i)

	// Without a usable GPU driver the guest falls back to SwiftShader: the picture is right, but the title screen
	// alone takes 6.6 cores. Record it and say so instead of putting up with it silently.
	res := m.shell(ctx, i, "dumpsys SurfaceFlinger | grep GLES", 0)
	renderer := ""
	if res.Code == 0 {
		renderer = glesRenderer(res.Out)
	}
	software := strings.Contains(strings.ToLower(renderer), "swiftshader")
	if software {
		m.Log.Warnf("魔改 AVD 实例 %d 正在用软件渲染（%s）：没有可用的显卡驱动，游戏会占满 CPU，请更新显卡驱动", i, renderer)
	}
	if ctx.Err() != nil {
		// the commands above were cut short: do not record a half run, the next boot does it again
		return
	}

	if err := m.writeInitMeta(i, len(failures) == 0, launcher, renderer, software, failures); err != nil {
		m.Log.Warnf("魔改 AVD 实例 %d 初始化结果没写进 mas-avd.json，下次开机重做: %v", i, err)
	}
	if len(failures) > 0 {
		m.Log.Warnf("魔改 AVD 实例 %d 初始化有失败项，下次开机重试: %s", i, pyList(failures))
	} else {
		m.Log.Infof("魔改 AVD 实例 %d 初始化完成（桌面: %s）", i, launcher)
	}
}

// glesRenderer picks the "GLES: vendor, renderer, version" line (the first line of the output is the
// "------------RE GLES------------" header).
func glesRenderer(out string) string {
	for _, line := range outputLines(out) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "GLES:") {
			return line
		}
	}
	return ""
}

// writeInitMeta merges the first-boot result into instances.<i> like AUTO-MAS update_instance_meta: existing keys
// keep their place, new ones are appended in its keyword order.
func (m *Manager) writeInitMeta(i int, ok bool, launcher, renderer string, software bool, failures []string) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		errsList := make([]*jsonord.Value, len(failures))
		for k, f := range failures {
			errsList[k] = jsonord.NewString(f)
		}
		meta.Set("initialized", jsonord.NewBool(ok))
		meta.Set("initializedAt", jsonord.NewString(root.NowISO()))
		meta.Set("launcher", jsonord.NewString(launcher))
		meta.Set("renderer", jsonord.NewString(renderer))
		meta.Set("softwareRenderer", jsonord.NewBool(software))
		meta.Set("initErrors", jsonord.NewArray(errsList...))
		return nil
	})
}

// installLauncher installs the light launcher, presets it, makes it home and removes the stock launcher for user 0
// (AUTO-MAS _install_launcher). Returns the launcher in use: its package, or "pixel" when the package has none or a
// step failed.
func (m *Manager) installLauncher(ctx context.Context, i int) string {
	apkPath := m.Root.LauncherAPK()
	if !regularFile(apkPath) {
		return "pixel"
	}
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: installLauncherTimeout, Reregister: true},
		"install", "-r", apkPath)
	if res.Code != 0 || !strings.Contains(res.Out, "Success") {
		m.Log.Warnf("实例 %d 安装轻量桌面失败: %s", i, res.Out)
		return "pixel"
	}
	m.presetLightLauncher(ctx, i)
	// The stock launcher is uninstalled for user 0 (the system partition keeps it; `cmd package install-existing`
	// restores it), not disabled: disabling sends PACKAGE_CHANGED, SystemUI's OverviewProxyService then looks for
	// the stock launcher's recents service and crashes on null, which breaks the home switch transition and leaves
	// only the wallpaper on the first boot. Uninstalling sends PACKAGE_REMOVED, which it ignores (10-08).
	cmds := []string{
		"cmd package set-home-activity " + lightLauncherHome,
		"pm uninstall --user 0 " + pixelLauncherPackage,
		"input keyevent KEYCODE_HOME",
	}
	if res := m.shell(ctx, i, strings.Join(cmds, " ; "), 0); res.Code != 0 {
		m.Log.Warnf("实例 %d 设置轻量桌面失败: %s", i, res.Out)
		return "pixel"
	}
	return lightLauncherPackage
}

// presetLightLauncher takes the onboarding pages and the notification permission prompt out of µLauncher's first
// start (AUTO-MAS _preset_light_launcher):
//  1. notifications denied and fixed (it asks on the last onboarding page);
//  2. an empty broadcast as root only to start its process in the background: seeing no finished onboarding it
//     writes its default preferences (clock, swipe up for the app list ...). `am broadcast` returns once the
//     receiver ran, by then the applied preferences are on disk. A file with only our keys would not do: seeing
//     "onboarding done" it would never write its defaults;
//  3. stop it, add our keys, restore owner, mode and SELinux label.
//
// Failures are only warnings: at worst the onboarding shows once.
func (m *Manager) presetLightLauncher(ctx context.Context, i int) {
	const notification = "android.permission.POST_NOTIFICATIONS"
	res := m.shell(ctx, i, "pm revoke "+lightLauncherPackage+" "+notification+" ; "+
		"pm set-permission-flags "+lightLauncherPackage+" "+notification+" user-set user-fixed", 0)
	if res.Code != 0 {
		m.Log.Warnf("实例 %d 轻量桌面通知权限没预设上: %s", i, tail(res.Out, 200))
	}

	prefs := lightLauncherPrefsFile
	// 0x20 = FLAG_INCLUDE_STOPPED_PACKAGES: a freshly installed app is stopped, the broadcast would not reach it
	res = m.su(ctx, i, "am broadcast -f 0x20 -n "+lightLauncherWakeReceiver+" >/dev/null ; "+
		"am force-stop "+lightLauncherPackage+" ; cat "+prefs, 0)
	version := fmt.Sprintf(`name="internal.version_code" value="%d"`, lightLauncherPrefsVersion)
	if !strings.Contains(res.Out, version) {
		m.Log.Warnf("实例 %d 轻量桌面没写出默认偏好（或偏好版本不是 %d），跳过预置: %s", i, lightLauncherPrefsVersion,
			tail(res.Out, 200))
		return
	}

	script, err := presetPrefsScript(prefs, lightLauncherPresetPrefs)
	if err != nil {
		m.Log.Warnf("实例 %d 轻量桌面偏好没预置上: %v", i, err)
		return
	}
	res = m.su(ctx, i, script, 0)
	var missing []string
	for _, p := range lightLauncherPresetPrefs {
		if entry, err := sharedPrefsEntry(p.Key, p.Value); err != nil || !strings.Contains(res.Out, entry) {
			missing = append(missing, p.Key)
		}
	}
	if res.Code != 0 || len(missing) > 0 {
		what := strconv.Itoa(res.Code)
		if len(missing) > 0 {
			what = pyList(missing)
		}
		m.Log.Warnf("实例 %d 轻量桌面偏好没预置上（%s）: %s", i, what, tail(res.Out, 200))
	} else {
		m.Log.Infof("实例 %d 轻量桌面已预置: 跳过引导、应用列表不弹键盘、双击桌面不锁屏、不申请通知权限", i)
	}
}

// presetPrefsScript is the root script that replaces the preset keys in the preferences file and fixes owner, mode
// and label. The script goes inside single quotes (su), the sed arguments inside double quotes: double quotes are
// escaped, and so is '&' (the matched text in a sed replacement).
func presetPrefsScript(prefs string, presets []sharedPref) (string, error) {
	deletes := make([]string, len(presets))
	var inserts strings.Builder
	for k, p := range presets {
		deletes[k] = `-e "/name=\"` + p.Key + `\"/d"`
		entry, err := sharedPrefsEntry(p.Key, p.Value)
		if err != nil {
			return "", err
		}
		inserts.WriteString(entry)
	}
	ins := strings.ReplaceAll(inserts.String(), `"`, `\"`)
	ins = strings.ReplaceAll(ins, "&", `\&`)
	dir := "" // Python rpartition("/")[0]
	if k := strings.LastIndex(prefs, "/"); k >= 0 {
		dir = prefs[:k]
	}
	return fmt.Sprintf(`sed -i %s -e "s#</map>#%s</map>#" %s && `+
		`chown $(stat -c %%u:%%g %s) %s && chmod 660 %s && `+
		`restorecon %s && cat %s`,
		strings.Join(deletes, " "), ins, prefs, dir, prefs, prefs, prefs, prefs), nil
}

// sharedPrefsEntry is one SharedPreferences XML element as Android writes it ('"' in strings as &quot;). It goes
// verbatim into the double quotes of a guest sed command, so characters the shell or sed would interpret are refused.
func sharedPrefsEntry(key string, value any) (string, error) {
	var text string
	switch v := value.(type) {
	case bool:
		text = fmt.Sprintf(`<boolean name="%s" value="%t" />`, key, v)
	case string:
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(v)
		text = `<string name="` + key + `">` + esc + `</string>`
	default:
		return "", errs.Newf(errs.Internal, "偏好 %s 的值类型不支持", key)
	}
	if strings.ContainsAny(text, "'#\\$`\n") {
		return "", errs.Newf(errs.Internal, "偏好 %s 里有客体命令放不下的字符", key)
	}
	return text, nil
}
