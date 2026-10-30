//go:build windows

package emu

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/logx"
	"modavd/internal/root"
)

const (
	gboard   = "com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME"
	aospIME  = "com.android.inputmethod.latin/.LatinIME"
	sogouIME = "com.sohu.inputmethod.sogou/.SogouIME"
)

// TestHostImeScripts: the guest scripts and constants are byte-identical to AUTO-MAS (printed by its own code) and
// the launcher; the marker especially, or the two would take the default back and forth. The probe's base.apk hash
// (the last part, 10-30) is in the launcher too, not in AUTO-MAS's own step, which modavd start replaces.
func TestHostImeScripts(t *testing.T) {
	checks := map[string][2]string{
		"read_default": {hostImeReadDefault, `if d=$(settings get secure default_input_method 2>/dev/null); then [ "$d" = null ] && d=; echo "def=$d"; else echo def_err; fi`},
		"probe":        {hostImeProbeScript, `if p=$(pm list packages --show-versioncode org.modavd.ime 2>/dev/null); then echo "$p"; echo pm_ok; else echo pm_err; fi; if d=$(settings get secure default_input_method 2>/dev/null); then [ "$d" = null ] && d=; echo "def=$d"; else echo def_err; fi; if [ -e /data/local/tmp/modavd-ime.autoselected ]; then echo mark=1; else echo mark=0; fi; s=; for a in $(pm path org.modavd.ime 2>/dev/null); do case $a in package:*/base.apk) s=$(sha256sum "${a#package:}" 2>/dev/null) ;; esac; done; echo "sha=${s%% *}"`},
		"wait":         {hostImeWaitScript, `i=0; while [ $i -lt 20 ]; do ime list -a -s | grep -qx org.modavd.ime/.HostImeService && { echo ok; exit 0; }; sleep 0.25; i=$((i+1)); done; echo timeout`},
		"marker":       {hostImeMarker, "/data/local/tmp/modavd-ime.autoselected"},
		"id":           {hostImeID, "org.modavd.ime/.HostImeService"},
		"allow":        {allowInstallSourcesScript, `set -o pipefail; list=$(dumpsys package packages 2>/dev/null | awk '/^  Package \[/ { n++; p = substr($2, 2, length($2) - 2) } /android.permission.REQUEST_INSTALL_PACKAGES/ { if (p != "" && p != "android") print p } END { if (n == 0) exit 3 }' | sort -u) || { echo "dumpsys package failed"; exit 2; }; fail=0; for p in $list; do m=$(appops get "$p" REQUEST_INSTALL_PACKAGES 2>&1) || { echo "failed: $p"; fail=1; continue; }; case "$m" in *"REQUEST_INSTALL_PACKAGES: default"*) ;; *"REQUEST_INSTALL_PACKAGES: "*) continue ;; esac; if appops set "$p" REQUEST_INSTALL_PACKAGES allow; then echo "$p"; else echo "failed: $p"; fail=1; fi; done; exit $fail`},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s differs from AUTO-MAS:\n got %s\nwant %s", name, c[0], c[1])
		}
	}
	if len(hostImeStockDefaults) != 3 || !hostImeStockDefaults[""] || !hostImeStockDefaults[gboard] || !hostImeStockDefaults[aospIME] {
		t.Errorf("stock defaults: %v", hostImeStockDefaults)
	}
}

func TestDefaultImeFrom(t *testing.T) {
	// same inputs as given to AUTO-MAS _default_ime_from: "", "a/b", None, None, "b"
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"def=", "", true},
		{"x\ndef=a/b\n", "a/b", true},
		{"def_err", "", false},
		{"", "", false},
		{"def=a\ndef=b", "b", true},
		{"  def=" + gboard + "  \r\n", gboard, true},
	}
	for _, c := range cases {
		got, ok := defaultImeFrom(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("defaultImeFrom(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseHostImeProbe(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want hostImeProbe
		ok   bool
	}{
		{"not installed, unset", "pm_ok\r\ndef=\r\nmark=0\r\nsha=", hostImeProbe{}, true},
		{"installed, ours, marked", "package:org.modavd.ime versionCode:3\npm_ok\ndef=" + hostImeID + "\nmark=1\nsha=" + fakeApkSHA,
			hostImeProbe{Installed: 3, Default: hostImeID, Marked: true, SHA: fakeApkSHA}, true},
		// the hash only matters for the same versionCode: unknown is not a probe failure
		{"installed, hash unknown", "package:org.modavd.ime versionCode:3\npm_ok\ndef=\nmark=0\nsha=",
			hostImeProbe{Installed: 3}, true},
		{"installed, no sha line", "package:org.modavd.ime versionCode:3\npm_ok\ndef=\nmark=0",
			hostImeProbe{Installed: 3}, true},
		{"installed, not a hash", "package:org.modavd.ime versionCode:3\npm_ok\ndef=\nmark=0\nsha=" + strings.ToUpper(fakeApkSHA[:40]),
			hostImeProbe{Installed: 3}, true},
		// pm filters by substring: the test app must not count
		{"substring package", "package:org.modavd.ime.testapp versionCode:9\npm_ok\ndef=" + gboard + "\nmark=0",
			hostImeProbe{Default: gboard}, true},
		{"pm failed", "pm_err\ndef=\nmark=0", hostImeProbe{}, false},
		{"settings failed", "pm_ok\ndef_err\nmark=0", hostImeProbe{}, false},
		{"no marker line", "pm_ok\ndef=", hostImeProbe{}, false},
		{"adb error", "error: device offline", hostImeProbe{}, false},
	}
	for _, c := range cases {
		got, ok := parseHostImeProbe(c.out)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestHostImeInstallAction(t *testing.T) {
	apk, dev := fakeApkSHA, guestSHA(devSigner, 3)
	cases := []struct {
		name                 string
		installed, apkVer    int64
		installedSHA, apkSHA string
		want                 imeInstallAction
	}{
		{"not installed", 0, 3, "", apk, imeInstall},
		{"older", 2, 3, dev, apk, imeInstall},
		{"older, hash unknown", 2, 3, "", "", imeInstall},
		{"same version, same bytes", 3, 3, apk, apk, imeUpToDate},
		// a dev build and the release build share the versionCode
		{"same version, other bytes", 3, 3, dev, apk, imeReplace},
		{"same version, guest hash unknown", 3, 3, "", apk, imeHashUnknown},
		{"same version, APK hash unknown", 3, 3, dev, "", imeHashUnknown},
		{"newer, same bytes", 4, 3, apk, apk, imeKeepNewer},
		{"newer, other bytes: never downgrade", 4, 3, dev, apk, imeKeepNewer},
	}
	for _, c := range cases {
		if got := hostImeInstallAction(c.installed, c.apkVer, c.installedSHA, c.apkSHA); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFileSHA256(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.apk")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := fileSHA256(f), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("fileSHA256 = %s, want %s", got, want)
	}
	if got := fileSHA256(f + ".missing"); got != "" {
		t.Errorf("missing file: %q", got)
	}
}

// TestPlanHostIme is the decision table of the launcher / AUTO-MAS rules.
func TestPlanHostIme(t *testing.T) {
	enable := "ime enable " + hostImeID + " >/dev/null"
	set := "ime set " + hostImeID + " >/dev/null"
	confirm := `[ "$(settings get secure default_input_method)" = ` + hostImeID + ` ] && touch ` + hostImeMarker
	touch := "touch " + hostImeMarker
	cases := []struct {
		name                       string
		prior                      string
		marked, fresh, reinstalled bool
		installedBefore            int64
		want                       hostImePlan
		cmds                       []string
	}{
		{"new install over Gboard", gboard, false, true, false, 0,
			hostImePlan{SetDefault: true, Enable: true}, []string{enable, set, confirm}},
		{"new install over AOSP keyboard", aospIME, false, true, false, 0,
			hostImePlan{SetDefault: true, Enable: true}, []string{enable, set, confirm}},
		{"up to date, default unset, undecided", "", false, false, false, 3,
			hostImePlan{SetDefault: true, Enable: true}, []string{enable, set, confirm}},
		{"new install, user has another IME", sogouIME, false, true, false, 0,
			hostImePlan{Enable: true, TouchMarker: true}, []string{enable, touch}},
		{"up to date, user has another IME", sogouIME, false, false, false, 3,
			hostImePlan{TouchMarker: true}, []string{touch}},
		{"already ours, undecided (marker lost)", hostImeID, false, false, false, 3,
			hostImePlan{TouchMarker: true}, []string{touch}},
		{"decided, back on Gboard: user's choice", gboard, true, false, false, 3,
			hostImePlan{}, nil},
		{"upgrade keeps a user's disabled state", hostImeID, true, true, false, 2,
			hostImePlan{}, nil},
		{"upgrade, undecided, stock default", gboard, false, true, false, 2,
			hostImePlan{SetDefault: true, Enable: true}, []string{enable, set, confirm}},
		// the marker was dropped before the uninstall when the default was ours
		{"signer change over our default", hostImeID, false, true, true, 2,
			hostImePlan{SetDefault: true, Enable: true}, []string{enable, set, confirm}},
		{"signer change, user's IME, decided", sogouIME, true, true, true, 2,
			hostImePlan{Enable: true}, []string{enable}},
	}
	for _, c := range cases {
		got := planHostIme(c.prior, c.marked, c.fresh, c.reinstalled, c.installedBefore)
		if got != c.want {
			t.Errorf("%s: plan %+v, want %+v", c.name, got, c.want)
		}
		if cmds := got.commands(); !reflect.DeepEqual(cmds, c.cmds) {
			t.Errorf("%s: commands %q, want %q", c.name, cmds, c.cmds)
		}
	}
}

// fakeImeGuest is the guest side of the IME step: package manager, input method service and settings, driven by
// exactly the commands runHostIme / recheckHostIme send. Unknown commands are recorded and fail the test.
type fakeImeGuest struct {
	imeState
	imeFaults
	installs int
	shells   int // adb shell commands
	adbCalls []string
	unknown  []string
}

type imeState struct {
	installed int64  // versionCode, 0 = not installed
	build     string // the installed build: its signer, or "<signer>-<n>" for other bytes under that signer
	enabled   bool
	def       string // secure default_input_method
	marker    bool
}

type imeFaults struct {
	probeFails     bool
	hashFails      bool // the probe cannot hash base.apk (prints "sha=")
	noApkSHA       bool // the host could not hash the APK
	uninstallFails bool
	installFails   int // the n-th install attempt fails (1-based), 0 = none
	setFails       bool
}

const (
	fakeApkPath   = `D:\root\components\modavd-ime.apk`
	releaseSigner = "release"
	devSigner     = "dev"
	releaseOther  = "release-2" // another build under the release key, same versionCode
)

// fakeApkSHA is the SHA-256 of the APK the fake installs (versionCode 3, the release build).
var fakeApkSHA = guestSHA(releaseSigner, 3)

// guestSHA is the fake base.apk hash of a build at a versionCode.
func guestSHA(build string, version int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", build, version))
	return hex.EncodeToString(sum[:])
}

func signerOf(build string) string {
	signer, _, _ := strings.Cut(build, "-")
	return signer
}

func (g *fakeImeGuest) io() guestIO {
	return guestIO{
		shell: func(cmd string, _ time.Duration) adb.Result { return g.shell(cmd) },
		adb: func(_ time.Duration, args ...string) adb.Result {
			g.adbCalls = append(g.adbCalls, strings.Join(args, " "))
			return g.adb(args)
		},
	}
}

func flag01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (g *fakeImeGuest) shell(cmd string) adb.Result {
	g.shells++
	switch cmd {
	case hostImeProbeScript:
		if g.probeFails {
			return adb.Result{Out: "pm_err\ndef=\nmark=0\nsha="}
		}
		var lines []string
		sha := ""
		if g.installed > 0 {
			lines = append(lines, fmt.Sprintf("package:%s versionCode:%d", hostImePackage, g.installed))
			if !g.hashFails {
				sha = guestSHA(g.build, g.installed)
			}
		}
		lines = append(lines, "package:org.modavd.ime.testapp versionCode:99", "pm_ok", "def="+g.def, "mark="+flag01(g.marker),
			"sha="+sha)
		return adb.Result{Out: strings.Join(lines, "\r\n")}
	case hostImeWaitScript:
		if g.installed > 0 {
			return adb.Result{Out: "ok"}
		}
		return adb.Result{Out: "timeout"}
	case "rm -f " + hostImeMarker:
		g.marker = false
		return adb.Result{}
	}
	rest, readBack := strings.CutSuffix(cmd, hostImeReadDefault)
	for _, piece := range strings.Split(strings.TrimSuffix(rest, "; "), "; ") {
		switch piece {
		case "", "sync":
		case "ime enable " + hostImeID + " >/dev/null":
			if g.installed > 0 {
				g.enabled = true
			}
		case "ime set " + hostImeID + " >/dev/null":
			if g.installed > 0 && g.enabled && !g.setFails {
				g.def = hostImeID
			}
		case `[ "$(settings get secure default_input_method)" = ` + hostImeID + ` ] && touch ` + hostImeMarker:
			if g.def == hostImeID {
				g.marker = true
			}
		case "touch " + hostImeMarker:
			g.marker = true
		default:
			g.unknown = append(g.unknown, piece)
			return adb.Result{Code: 127, Out: "sh: unknown command"}
		}
	}
	if readBack {
		return adb.Result{Out: "def=" + g.def}
	}
	return adb.Result{}
}

func (g *fakeImeGuest) adb(args []string) adb.Result {
	const apkVersion, signer = 3, releaseSigner
	switch {
	case len(args) == 2 && args[0] == "uninstall" && args[1] == hostImePackage:
		if g.uninstallFails || g.installed == 0 {
			return adb.Result{Code: 1, Out: "Failure [DELETE_FAILED_INTERNAL_ERROR]"}
		}
		g.installed, g.build, g.enabled = 0, "", false
		if g.def == hostImeID {
			g.def = gboard // the system picks another keyboard
		}
		return adb.Result{Out: "Success"}
	case len(args) >= 2 && args[0] == "install" && args[len(args)-1] == fakeApkPath:
		g.installs++
		replace := len(args) == 3 && args[1] == "-r"
		if g.installs == g.installFails {
			return adb.Result{Code: 1, Out: "Performing Streamed Install\nadb: failed to install " + fakeApkPath +
				": Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]"}
		}
		if g.installed > 0 {
			if !replace {
				return adb.Result{Code: 1, Out: "Failure [INSTALL_FAILED_ALREADY_EXISTS: Attempt to re-install org.modavd.ime without first uninstalling.]"}
			}
			if signerOf(g.build) != signer {
				return adb.Result{Code: 1, Out: "Performing Streamed Install\nadb: failed to install " + fakeApkPath +
					": Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Existing package org.modavd.ime signatures do not match newer version; ignoring!]"}
			}
		} else {
			g.enabled = false
		}
		g.installed, g.build = apkVersion, signer
		return adb.Result{Out: "Performing Streamed Install\r\nSuccess"}
	}
	g.unknown = append(g.unknown, "adb "+strings.Join(args, " "))
	return adb.Result{Code: 1, Out: "adb: unknown command"}
}

type logLine struct {
	level logx.Level
	msg   string
}

func recordingManager() (*Manager, *[]logLine) {
	var lines []logLine
	m := &Manager{Log: logx.New(func(level logx.Level, msg string) { lines = append(lines, logLine{level, msg}) })}
	return m, &lines
}

var imeSeconds = regexp.MustCompile(`（\d+\.\d 秒）$`)

// TestRunHostIme walks the whole IME step against a fake guest: installs, signer changes, the default and the
// marker, and the one log line AUTO-MAS writes for each case.
func TestRunHostIme(t *testing.T) {
	const prefix = "实例 0 宿主输入法: org.modavd.ime "
	type S = imeState
	type F = imeFaults
	const rel, dev, rel2 = releaseSigner, devSigner, releaseOther
	install := "install -r " + fakeApkPath
	reinstall := []string{install, "uninstall " + hostImePackage, "install " + fakeApkPath}
	cases := []struct {
		name   string
		before S
		faults F
		after  S
		ours   bool
		warn   bool
		msg    string // without the timing
		adb    []string
	}{
		{"new install over Gboard", S{def: gboard}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 新装，默认 " + hostImeID + "（原来是 " + gboard + "）", []string{install}},
		{"new install, default unset", S{}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 新装，默认 " + hostImeID + "（原来是 没设）", []string{install}},
		{"up to date, decided, ours", S{3, rel, true, hostImeID, true}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 已是最新，默认 " + hostImeID, nil},
		{"user went back to Gboard", S{3, rel, true, gboard, true}, F{},
			S{3, rel, true, gboard, true}, false, false,
			prefix + "3 已是最新，默认 " + gboard + "（保持用户选的）", nil},
		{"new install, user has Sogou", S{def: sogouIME}, F{},
			S{3, rel, true, sogouIME, true}, false, false,
			prefix + "3 新装，默认 " + sogouIME + "（保持用户选的）", []string{install}},
		{"upgrade keeps the user's disabled state", S{2, rel, false, gboard, true}, F{},
			S{3, rel, false, gboard, true}, false, false,
			prefix + "3 从 2 升级，默认 " + gboard + "（保持用户选的）", []string{install}},
		{"marker lost, already ours", S{3, rel, true, hostImeID, false}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 已是最新，默认 " + hostImeID, nil},
		{"installed is newer: never downgrade", S{4, rel, true, gboard, false}, F{},
			S{4, rel, true, hostImeID, true}, true, false,
			prefix + "保留已装的 4（安装包是 3），默认 " + hostImeID + "（原来是 " + gboard + "）", nil},
		{"signer change over our default", S{2, dev, true, hostImeID, true}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 换签名重装（原来是 2），默认 " + hostImeID, reinstall},
		{"signer change, user's IME stays", S{2, dev, true, sogouIME, true}, F{},
			S{3, rel, true, sogouIME, true}, false, false,
			prefix + "3 换签名重装（原来是 2），默认 " + sogouIME + "（保持用户选的）", reinstall},
		// AUTO-MAS WIP: an uninstall that fails leaves the old one and installs nothing
		{"signer change, uninstall fails", S{2, dev, true, hostImeID, true}, F{uninstallFails: true},
			S{2, dev, true, hostImeID, true}, true, true,
			prefix + "签名不同，卸载旧版 2 失败（DELETE_FAILED_INTERNAL_ERROR），默认 " + hostImeID, reinstall[:2]},
		{"signer change, reinstall fails", S{2, dev, true, hostImeID, true}, F{installFails: 2},
			S{def: gboard}, false, true,
			prefix + "安装失败（INSTALL_FAILED_INSUFFICIENT_STORAGE），旧签名的已卸掉，默认 " + gboard + "（原来是 " + hostImeID + "）",
			reinstall},
		{"new install fails", S{def: gboard}, F{installFails: 1},
			S{def: gboard}, false, true,
			prefix + "安装失败（INSTALL_FAILED_INSUFFICIENT_STORAGE），默认 " + gboard, []string{install}},
		{"ime set does not take: no marker, retried next boot", S{def: gboard}, F{setFails: true},
			S{3, rel, true, gboard, false}, false, true,
			prefix + "3 新装，没能设成默认（下次开机再试），默认 " + gboard, []string{install}},

		// same versionCode (a dev build and the release build are both 1): the bytes decide
		{"same version, other bytes, same signer: installed again, disabled stays", S{3, rel2, false, gboard, true}, F{},
			S{3, rel, false, gboard, true}, false, false,
			prefix + "3 内容不同，重装，默认 " + gboard + "（保持用户选的）", []string{install}},
		{"same version, other bytes, same signer, ours", S{3, rel2, true, hostImeID, true}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 内容不同，重装，默认 " + hostImeID, []string{install}},
		// ime-pkg step c: the dev build set up by hand, the marker there
		{"same version, other signer over our default", S{3, dev, true, hostImeID, true}, F{},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 签名不同，卸了重装，默认 " + hostImeID, reinstall},
		{"same version, other signer, user's IME, disabled: enabled again", S{3, dev, false, sogouIME, true}, F{},
			S{3, rel, true, sogouIME, true}, false, false,
			prefix + "3 签名不同，卸了重装，默认 " + sogouIME + "（保持用户选的）", reinstall},
		{"same version, other signer, uninstall fails", S{3, dev, true, hostImeID, true}, F{uninstallFails: true},
			S{3, dev, true, hostImeID, true}, true, true,
			prefix + "签名不同，卸载旧版 3 失败（DELETE_FAILED_INTERNAL_ERROR），默认 " + hostImeID, reinstall[:2]},
		{"same version, other bytes, install fails", S{3, rel2, true, hostImeID, true}, F{installFails: 1},
			S{3, rel2, true, hostImeID, true}, true, true,
			prefix + "安装失败（INSTALL_FAILED_INSUFFICIENT_STORAGE），默认 " + hostImeID, []string{install}},
		{"same version, guest hash unknown: up to date", S{3, dev, true, hostImeID, true}, F{hashFails: true},
			S{3, dev, true, hostImeID, true}, true, false,
			prefix + "3 已是最新（哈希读不出，没比内容），默认 " + hostImeID, nil},
		{"same version, APK hash unknown: up to date", S{3, dev, true, hostImeID, true}, F{noApkSHA: true},
			S{3, dev, true, hostImeID, true}, true, false,
			prefix + "3 已是最新（哈希读不出，没比内容），默认 " + hostImeID, nil},
		{"older, hash unknown: upgraded", S{2, rel, true, hostImeID, true}, F{hashFails: true},
			S{3, rel, true, hostImeID, true}, true, false,
			prefix + "3 从 2 升级，默认 " + hostImeID, []string{install}},
		{"newer, other signer: kept", S{4, dev, true, hostImeID, true}, F{},
			S{4, dev, true, hostImeID, true}, true, false,
			prefix + "保留已装的 4（安装包是 3），默认 " + hostImeID, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := fakeImeGuest{imeState: c.before, imeFaults: c.faults}
			m, lines := recordingManager()
			apkSHA := fakeApkSHA
			if c.faults.noApkSHA {
				apkSHA = ""
			}
			ours := m.runHostIme(0, fakeApkPath, 3, apkSHA, g.io())
			if len(g.unknown) > 0 {
				t.Fatalf("unexpected guest commands: %q", g.unknown)
			}
			if ours != c.ours {
				t.Errorf("returned %v, want %v", ours, c.ours)
			}
			if got := g.imeState; got != c.after {
				t.Errorf("guest after:\n got %+v\nwant %+v", got, c.after)
			}
			if !reflect.DeepEqual(g.adbCalls, c.adb) {
				t.Errorf("adb calls %q, want %q", g.adbCalls, c.adb)
			}
			if len(*lines) != 1 {
				t.Fatalf("log lines %v, want one", *lines)
			}
			l := (*lines)[0]
			if (l.level == logx.Warn) != c.warn {
				t.Errorf("level %v, warn wanted %v", l.level, c.warn)
			}
			if !imeSeconds.MatchString(l.msg) {
				t.Errorf("no timing at the end: %q", l.msg)
			}
			if msg := imeSeconds.ReplaceAllString(l.msg, ""); msg != c.msg {
				t.Errorf("message\n got %s\nwant %s", msg, c.msg)
			}
		})
	}
}

// TestRunHostImeSteadyState: an up-to-date, decided guest costs one adb shell (the probe) and nothing else, hash
// included; so does a guest that cannot hash.
func TestRunHostImeSteadyState(t *testing.T) {
	for _, hashFails := range []bool{false, true} {
		g := fakeImeGuest{imeState: imeState{3, releaseSigner, true, hostImeID, true}, imeFaults: imeFaults{hashFails: hashFails}}
		m, _ := recordingManager()
		if !m.runHostIme(0, fakeApkPath, 3, fakeApkSHA, g.io()) {
			t.Errorf("hash fails %v: returned false", hashFails)
		}
		if g.shells != 1 || len(g.adbCalls) != 0 || len(g.unknown) != 0 {
			t.Errorf("hash fails %v: %d shell commands, adb %q, unknown %q; want only the probe", hashFails, g.shells,
				g.adbCalls, g.unknown)
		}
	}
}

func TestRunHostImeProbeFails(t *testing.T) {
	g := fakeImeGuest{imeState: imeState{def: gboard}, imeFaults: imeFaults{probeFails: true}}
	m, lines := recordingManager()
	if m.runHostIme(0, fakeApkPath, 3, fakeApkSHA, g.io()) {
		t.Error("returned true")
	}
	if len(g.adbCalls) != 0 || g.installed != 0 || g.marker {
		t.Errorf("guest touched: %+v", g)
	}
	want := "实例 0 宿主输入法这次跳过，读不出客体现状: pm_err | def= | mark=0 | sha="
	if len(*lines) != 1 || (*lines)[0].level != logx.Warn || (*lines)[0].msg != want {
		t.Errorf("log %v, want warning %q", *lines, want)
	}
}

func TestRecheckHostIme(t *testing.T) {
	cases := []struct {
		name  string
		def   string // before
		fails bool   // ime set does not take
		after string
		level logx.Level
		msg   string
	}{
		{"still ours", hostImeID, false, hostImeID, -1, ""},
		{"lost: set back", gboard, false, hostImeID, logx.Info,
			"实例 4 宿主输入法: 重启 zygote 后默认变成了 " + gboard + "，已设回"},
		{"lost: cannot set back", "", true, "", logx.Warn,
			"实例 4 宿主输入法: 重启 zygote 后默认变成了 没设，没能设回（现在 没设）"},
	}
	for _, c := range cases {
		g := fakeImeGuest{imeState: imeState{installed: 3, build: releaseSigner, enabled: true, def: c.def, marker: true},
			imeFaults: imeFaults{setFails: c.fails}}
		m, lines := recordingManager()
		m.recheckHostIme(4, g.io())
		if g.def != c.after || len(g.unknown) > 0 {
			t.Errorf("%s: default %q (unknown %q), want %q", c.name, g.def, g.unknown, c.after)
		}
		switch {
		case c.msg == "" && len(*lines) != 0:
			t.Errorf("%s: logged %v", c.name, *lines)
		case c.msg != "" && (len(*lines) != 1 || (*lines)[0].level != c.level || (*lines)[0].msg != c.msg):
			t.Errorf("%s: log %v, want %v %q", c.name, *lines, c.level, c.msg)
		}
	}
	// settings unreadable: no ime set, a warning
	m, lines := recordingManager()
	m.recheckHostIme(4, guestIO{shell: func(string, time.Duration) adb.Result {
		return adb.Result{Code: -1, Out: "adb shell 超时（30 秒）"}
	}})
	if len(*lines) != 1 || (*lines)[0].msg != "实例 4 宿主输入法: 重启 zygote 后读不出默认输入法" {
		t.Errorf("unreadable: %v", *lines)
	}
}

// TestSetupHostImeSkips: the quiet and the warned skips before the guest is touched (no adb here at all).
func TestSetupHostImeSkips(t *testing.T) {
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	props := filepath.Join(dir, "sdk", "emulator", "source.properties")
	writeBuild := func(id string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(props), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(props, []byte("Pkg.Revision=37.2.10\nPkg.BuildId="+id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var lines []logLine
	m := &Manager{Root: r, Log: logx.New(func(level logx.Level, msg string) { lines = append(lines, logLine{level, msg}) })}

	// no APK in the package: nothing, not even a debug line
	writeBuild("mas-35")
	if m.setupHostIme(t.Context(), 0, true, "", nil) || len(lines) != 0 {
		t.Errorf("no APK: %v", lines)
	}

	if err := os.MkdirAll(filepath.Dir(r.ImeAPK()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.ImeAPK(), []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	// an emulator without the channel: debug only
	writeBuild("mas-34")
	lines = nil
	if m.setupHostIme(t.Context(), 0, false, "", nil) || len(lines) != 1 || lines[0].level != logx.Debug ||
		lines[0].msg != "实例 0 宿主输入法跳过: 内测包 mas-34 低于 mas-35" {
		t.Errorf("old emulator: %v", lines)
	}
	// channel known (build): an unreadable APK is a warning
	writeBuild("mas-35")
	lines = nil
	if m.setupHostIme(t.Context(), 0, false, "", nil) || len(lines) != 1 || lines[0].level != logx.Warn ||
		lines[0].msg != "实例 0 宿主输入法没装: 读不出 "+r.ImeAPK()+" 的包名和版本号" {
		t.Errorf("bad APK: %v", lines)
	}
}

func TestHostImeChannel(t *testing.T) {
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: r, Log: logx.Nop()}
	logWith := filepath.Join(dir, "with.log")
	logWithout := filepath.Join(dir, "without.log")
	os.WriteFile(logWith, []byte("INFO    | "+hostImeChannelLine+"\n"), 0o644)
	os.WriteFile(logWithout, []byte("INFO    | boot completed\n"), 0o644)
	props := filepath.Join(dir, "sdk", "emulator", "source.properties")
	os.MkdirAll(filepath.Dir(props), 0o755)

	cases := []struct {
		build    string // "" = no source.properties
		launched bool
		log      string
		ok       bool
	}{
		{"mas-35", false, "", true},
		{"mas-36", true, logWithout, true},
		{"mas-34", false, logWith, false}, // adopted: the log is not ours to judge
		{"mas-34", true, logWith, true},   // a dev SDK whose build id lags behind
		{"mas-34", true, logWithout, false},
		{"", true, logWith, true},
		{"", false, "", false},
	}
	for _, c := range cases {
		os.Remove(props)
		if c.build != "" {
			os.WriteFile(props, []byte("Pkg.BuildId="+c.build+"\n"), 0o644)
		}
		ok, why := m.hostImeChannel(c.launched, c.log)
		if ok != c.ok || (ok && why != "") || (!ok && why == "") {
			t.Errorf("build %q launched %v log %s: %v %q", c.build, c.launched, filepath.Base(c.log), ok, why)
		}
	}
}

func TestInstallFailure(t *testing.T) {
	cases := []struct {
		res  adb.Result
		want string
	}{
		{adb.Result{Code: 1, Out: "Performing Streamed Install\nadb: failed to install x.apk: Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Package org.modavd.ime signatures do not match]"}, "INSTALL_FAILED_UPDATE_INCOMPATIBLE"},
		{adb.Result{Code: 1, Out: "Performing Streamed Install\nerror: device offline\n"}, "error: device offline"},
		{adb.Result{Code: 7, Out: "  "}, "退出码 7"},
	}
	for _, c := range cases {
		if got := installFailure(c.res); got != c.want {
			t.Errorf("installFailure(%q) = %q, want %q", c.res.Out, got, c.want)
		}
	}
}
