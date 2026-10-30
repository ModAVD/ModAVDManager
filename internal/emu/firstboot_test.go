//go:build windows

package emu

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// The reference strings below were printed by AUTO-MAS's own code (manager._shared_prefs_entry over
// LIGHT_LAUNCHER_PRESET_PREFS, and the script _preset_light_launcher builds).
var masPresetEntries = []string{
	`<boolean name="internal.started_before" value="true" />`,
	`<boolean name="functionality.search_auto_keyboard" value="false" />`,
	`<string name="action.double_click">{&quot;type&quot;:&quot;action:launcher&quot;,&quot;value&quot;:&quot;nop&quot;}</string>`,
}

const masPresetScript = `sed -i -e "/name=\"internal.started_before\"/d" -e "/name=\"functionality.search_auto_keyboard\"/d" -e "/name=\"action.double_click\"/d" -e "s#</map>#<boolean name=\"internal.started_before\" value=\"true\" /><boolean name=\"functionality.search_auto_keyboard\" value=\"false\" /><string name=\"action.double_click\">{\&quot;type\&quot;:\&quot;action:launcher\&quot;,\&quot;value\&quot;:\&quot;nop\&quot;}</string></map>#" /data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml && chown $(stat -c %u:%g /data/data/de.jrpie.android.launcher/shared_prefs) /data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml && chmod 660 /data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml && restorecon /data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml && cat /data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml`

func TestSharedPrefsEntry(t *testing.T) {
	for k, p := range lightLauncherPresetPrefs {
		got, err := sharedPrefsEntry(p.Key, p.Value)
		if err != nil || got != masPresetEntries[k] {
			t.Errorf("%s: got %q, %v; want %q", p.Key, got, err, masPresetEntries[k])
		}
	}
	got, err := sharedPrefsEntry("k", `a&b<c>"d"`)
	if err != nil || got != `<string name="k">a&amp;b&lt;c&gt;&quot;d&quot;</string>` {
		t.Errorf("escaping: %q, %v", got, err)
	}
	// characters the single-quoted su script / the sed command would interpret are refused
	for _, bad := range []string{"it's", "a#b", `back\slash`, "$HOME", "`id`", "two\nlines"} {
		if _, err := sharedPrefsEntry("k", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := sharedPrefsEntry("k", 3); err == nil {
		t.Error("an int value was accepted")
	}
}

func TestPresetPrefsScript(t *testing.T) {
	got, err := presetPrefsScript(lightLauncherPrefsFile, lightLauncherPresetPrefs)
	if err != nil {
		t.Fatal(err)
	}
	if got != masPresetScript {
		t.Errorf("preset script differs from AUTO-MAS:\n got %s\nwant %s", got, masPresetScript)
	}
	// it goes through su 0 sh -c '<script>', which cannot hold single quotes
	if strings.Contains(got, "'") {
		t.Error("preset script contains a single quote")
	}
	if _, err := presetPrefsScript(lightLauncherPrefsFile, []sharedPref{{"k", "a'b"}}); err == nil {
		t.Error("bad value accepted")
	}
}

func TestFirstBootConstants(t *testing.T) {
	masDebloat := []string{
		"com.google.android.googlequicksearchbox", "com.google.android.youtube", "com.google.android.apps.youtube.music",
		"com.google.android.gm", "com.google.android.apps.messaging", "com.google.android.dialer",
		"com.google.android.contacts", "com.google.android.apps.photos", "com.google.android.apps.maps",
		"com.google.android.apps.docs", "com.google.android.calendar", "com.google.android.deskclock",
		"com.google.android.apps.wellbeing", "com.google.android.as", "com.google.android.as.oss",
		"com.google.android.projection.gearhead", "com.google.android.marvin.talkback",
		"com.google.android.apps.restore", "com.google.android.feedback",
		"com.google.android.printservice.recommendation", "com.google.android.markup",
	}
	if !reflect.DeepEqual(debloatPackages, masDebloat) {
		t.Errorf("debloat list differs from AUTO-MAS DEBLOAT_PACKAGES: %q", debloatPackages)
	}
	if lightLauncherPrefsVersion != 101 || lightLauncherWakeReceiver != "de.jrpie.android.launcher/.actions.lock.LauncherDeviceAdmin" ||
		lightLauncherHome != "de.jrpie.android.launcher/.ui.HomeActivity" || pixelLauncherPackage != "com.google.android.apps.nexuslauncher" ||
		lightLauncherPrefsFile != "/data/data/de.jrpie.android.launcher/shared_prefs/de.jrpie.android.launcher_preferences.xml" {
		t.Error("light launcher constants differ from AUTO-MAS constants.py")
	}
}

func TestGlesRenderer(t *testing.T) {
	out := "------------RE GLES------------\r\nGLES: Google (NVIDIA Corporation), Android Emulator OpenGL ES Translator (NVIDIA GeForce RTX 4080/PCIe/SSE2), OpenGL ES 3.0\r\n"
	want := "GLES: Google (NVIDIA Corporation), Android Emulator OpenGL ES Translator (NVIDIA GeForce RTX 4080/PCIe/SSE2), OpenGL ES 3.0"
	if got := glesRenderer(out); got != want {
		t.Errorf("glesRenderer = %q", got)
	}
	if got := glesRenderer("------------RE GLES------------"); got != "" {
		t.Errorf("header only: %q", got)
	}
	sw := glesRenderer("  GLES: Google (Google Inc.), Android Emulator OpenGL ES Translator (Google SwiftShader), OpenGL ES 3.0")
	if !strings.Contains(strings.ToLower(sw), "swiftshader") {
		t.Errorf("swiftshader line: %q", sw)
	}
}

func TestPyList(t *testing.T) {
	if got := pyList([]string{"基础设置: x", "禁用预装应用: y"}); got != "['基础设置: x', '禁用预装应用: y']" {
		t.Errorf("pyList = %q", got)
	}
	if got := pyList(nil); got != "[]" {
		t.Errorf("empty pyList = %q", got)
	}
	// what Python prints for these strings
	for in, want := range map[string]string{
		"plain":             `'plain'`,
		"it's":              `"it's"`,
		`say "hi"`:          `'say "hi"'`,
		`both ' and "`:      `'both \' and "'`,
		"two\r\nlines\ttab": `'two\r\nlines\ttab'`,
		`back\slash`:        `'back\\slash'`,
		"bell\x07 del\x7f":  `'bell\x07 del\x7f'`,
		"失败: Failure [X]":   `'失败: Failure [X]'`,
	} {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestWriteInitMeta: the first-boot result lands in instances.<i> in AUTO-MAS's key order, other keys and instances
// untouched, and initialized reads it back with Python truthiness.
func TestWriteInitMeta(t *testing.T) {
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := `{"adbServerPort": 20089, "instances": {"1": {"memoryMb": 4096}, "2": {"memoryMb": 6144, "initialized": false, "createdAt": "2026-10-28T10:00:00"}}}`
	if err := os.WriteFile(r.MetadataPath(), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: r, Log: logx.Nop()}
	if m.initialized(2) {
		t.Fatal("initialized before the first boot")
	}
	renderer := "GLES: Google (NVIDIA Corporation), Android Emulator OpenGL ES Translator (NVIDIA GeForce RTX 4080/PCIe/SSE2), OpenGL ES 3.0"
	if err := m.writeInitMeta(2, true, lightLauncherPackage, renderer, false, nil); err != nil {
		t.Fatal(err)
	}
	if !m.initialized(2) {
		t.Error("not initialized after writeInitMeta")
	}
	meta := r.InstanceMeta(2)
	wantKeys := []string{"memoryMb", "initialized", "createdAt", "initializedAt", "launcher", "renderer",
		"softwareRenderer", "initErrors"}
	if got := meta.Keys(); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("keys %q, want %q", got, wantKeys)
	}
	if s, _ := meta.Get("launcher").Str(); s != lightLauncherPackage {
		t.Errorf("launcher = %q", s)
	}
	if at, _ := meta.Get("initializedAt").Str(); len(at) != len("2026-10-28T15:04:05") || strings.ContainsAny(at, "Z+") {
		t.Errorf("initializedAt = %q", at)
	}
	if items := meta.Get("initErrors").Items(); items == nil || len(items) != 0 {
		t.Errorf("initErrors = %v", items)
	}
	if mem, _ := r.InstanceMeta(1).Get("memoryMb").Int(); mem != 4096 {
		t.Error("another instance changed")
	}

	// failures: initialized stays false, the errors are kept for the next boot's retry
	if err := m.writeInitMeta(2, false, "pixel", "", false, []string{"禁用预装应用: x"}); err != nil {
		t.Fatal(err)
	}
	if m.initialized(2) {
		t.Error("initialized despite failures")
	}
	if items := r.InstanceMeta(2).Get("initErrors").Items(); len(items) != 1 {
		t.Errorf("initErrors = %v", items)
	}

	// an instance that has no entry yet gets one
	if err := m.writeInitMeta(3, true, "pixel", "", false, nil); err != nil || !m.initialized(3) {
		t.Errorf("new entry: %v", err)
	}
}

func TestJSONTruthy(t *testing.T) {
	for in, want := range map[string]bool{
		"true": true, "false": false, "null": false, "1": true, "0": false, "0.0": false, "-0.5": true, "2e3": true,
		`""`: false, `"no"`: true, "[]": false, "[0]": true, "{}": false, `{"a": 1}`: true,
	} {
		v, err := jsonord.Parse([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := jsonTruthy(v); got != want {
			t.Errorf("jsonTruthy(%s) = %v, want %v", in, got, want)
		}
	}
	if jsonTruthy(nil) {
		t.Error("a missing key is truthy")
	}
}
