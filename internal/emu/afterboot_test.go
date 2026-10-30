//go:build windows

package emu

import (
	"strings"
	"testing"
)

// `su 0 sh mas-wifi-fastpath.sh status` with the rule in place (adb output has CRLF on the host).
const fastpathStatusOn = "rule4: 15000:\tfrom all iif lo lookup eth0\r\n" +
	"rule6: 15000:\tfrom all iif lo lookup eth0\r\n" +
	"eth0 default: default via 10.0.2.2 dev eth0 proto static\r\n" +
	"route 1.1.1.1: 1.1.1.1 via 10.0.2.2 dev eth0 table eth0 src 10.0.2.15 uid 0\r\n" +
	"tcp_rmem: 2097152\t6291456\t16777216\r\n" +
	"eth0 quota: none\r\n" +
	"data plan: none\r\n" +
	"watchers: 1234 \r\n" +
	"default network: Active default network: 101\r\n" +
	"ni{WIFI CONNECTED\r\n"

// right after boot: no eth0 table yet, the watcher runs
const fastpathStatusWaiting = "rule4: \nrule6: \neth0 default: \nroute 1.1.1.1: 1.1.1.1 via 10.0.2.2 dev wlan0\n" +
	"watchers: 2345 \ndefault network: Active default network: 100\n"

func TestParseStatusLines(t *testing.T) {
	lines := parseStatusLines(fastpathStatusOn)
	if lines["rule4"] != "15000:\tfrom all iif lo lookup eth0" {
		t.Errorf("rule4 = %q", lines["rule4"])
	}
	if lines["watchers"] != "1234" {
		t.Errorf("watchers = %q", lines["watchers"])
	}
	// the key is everything before the first ':', not trimmed; the value is the rest, trimmed
	if lines["route 1.1.1.1"] != "1.1.1.1 via 10.0.2.2 dev eth0 table eth0 src 10.0.2.15 uid 0" {
		t.Errorf("route = %q", lines["route 1.1.1.1"])
	}
	if v, ok := lines["ni{WIFI CONNECTED"]; !ok || v != "" {
		t.Errorf("line without ':' = %q, %v", v, ok)
	}
	// later lines win (Python dict comprehension)
	if got := parseStatusLines("a: 1\na: 2")["a"]; got != "2" {
		t.Errorf("duplicate key = %q", got)
	}
}

func TestFastpathReport(t *testing.T) {
	warn, msg := fastpathReport(2, fastpathStatusOn)
	if warn || msg != "实例 2 网络: WiFi 开，fastpath 生效（15000:\tfrom all iif lo lookup eth0）" {
		t.Errorf("on: %v %q", warn, msg)
	}
	warn, msg = fastpathReport(2, fastpathStatusWaiting)
	if warn || msg != "实例 2 网络: WiFi 开，fastpath 守护已起（pid 2345），规则在 eth0 就绪后由守护补上" {
		t.Errorf("waiting: %v %q", warn, msg)
	}
	warn, msg = fastpathReport(2, "rule4: \nwatchers: \n")
	if !warn || !strings.HasPrefix(msg, "实例 2 fastpath 没起来: ") {
		t.Errorf("down: %v %q", warn, msg)
	}
}

func TestIdlepollReport(t *testing.T) {
	status := "disk: vdc\r\nwatchers: 3456 \r\nloops: \r\n"
	warn, msg := idlepollReport(1, "", status)
	if warn || msg != "实例 1 密集读写加速: 守护已起（pid 3456，数据盘 vdc）" {
		t.Errorf("running: %v %q", warn, msg)
	}
	warn, msg = idlepollReport(1, "", "disk: \nwatchers: 3456\n")
	if warn || msg != "实例 1 密集读写加速: 守护已起（pid 3456，数据盘 ?）" {
		t.Errorf("no disk: %v %q", warn, msg)
	}
	started := "mas-idlepoll: no stat for the data disk 'x', not watching"
	warn, msg = idlepollReport(1, started+"\n", "disk: x\nwatchers: \nloops: \n")
	if !warn || msg != "实例 1 密集读写加速没起来: "+started+" / disk: x\nwatchers: \nloops:" {
		t.Errorf("down: %v %q", warn, msg)
	}
}

func TestParseWriteback(t *testing.T) {
	if e, w, ok := parseWriteback("200\r\n100\r\n"); !ok || e != 200 || w != 100 {
		t.Errorf("good: %d %d %v", e, w, ok)
	}
	if _, _, ok := parseWriteback("sh: /proc/sys/vm/dirty_expire_centisecs: Permission denied"); ok {
		t.Error("error text parsed as values")
	}
	if _, _, ok := parseWriteback("3000"); ok {
		t.Error("one value parsed as two")
	}
}

func TestBalloonState(t *testing.T) {
	cases := []struct {
		out            string
		bound, current string
		ok             bool
	}{
		{"1\r\n9\r\n", "1", "9", true},
		{"0\n10\n", "0", "10", false},
		{"1\n10\n", "1", "10", false},
		{"", "?", "?", false},
		{"1", "1", "?", false},
	}
	for _, c := range cases {
		b, cur, ok := balloonState(c.out, 9)
		if b != c.bound || cur != c.current || ok != c.ok {
			t.Errorf("balloonState(%q) = %q %q %v", c.out, b, cur, ok)
		}
	}
}

func TestParseRotation(t *testing.T) {
	if got := parseRotation("mDisplayRotation=ROTATION_0\r\nBroken pipe\r\n"); got != "ROTATION_0" {
		t.Errorf("rotation = %q", got)
	}
	if got := parseRotation("Broken pipe\n  mDisplayRotation=ROTATION_90\n"); got != "ROTATION_90" {
		t.Errorf("rotation = %q", got)
	}
	if got := parseRotation(""); got != "读不到" {
		t.Errorf("empty rotation = %q", got)
	}
}

func TestLineHelpers(t *testing.T) {
	if got := lastNonEmptyLine("NEED_UPLOAD\r\n\r\n  "); got != "NEED_UPLOAD" {
		t.Errorf("lastNonEmptyLine = %q", got)
	}
	if got := lastNonEmptyLine(""); got != "" {
		t.Errorf("lastNonEmptyLine empty = %q", got)
	}
	if got := joinNonEmptyLines(" 0\r\n\r\n1 \n", " / "); got != "0 / 1" {
		t.Errorf("joinNonEmptyLines = %q", got)
	}
	if orText("", "x") != "x" || orText("y", "x") != "y" {
		t.Error("orText")
	}
}
