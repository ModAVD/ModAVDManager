//go:build windows

package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"modavd/internal/appproxy"
	"modavd/internal/emu"
)

func TestProxyCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		call string
	}{
		{[]string{"proxy", "3"}, "proxy 3"},
		{[]string{"proxy", "3", "test"}, "proxy-test 3"},
		{[]string{"proxy", "3", "on"}, "proxy-set 3 on=true server=- apps=- add= remove="},
		{[]string{"proxy", "mas_3", "OFF"}, "proxy-set 3 on=false server=- apps=- add= remove="},
		{[]string{"proxy", "3", "on", "--server", "auto", "--add", "a.b, c.d", "--add", "e.f"},
			"proxy-set 3 on=true server=auto apps=- add=a.b,c.d,e.f remove="},
		{[]string{"proxy", "3", "--apps", ""}, "proxy-set 3 on=- server=- apps= add= remove="},
		{[]string{"proxy", "3", "--remove", "a.b", "--server", "socks5://127.0.0.1:7890"},
			"proxy-set 3 on=- server=socks5://127.0.0.1:7890 apps=- add= remove=a.b"},
	} {
		h := newHarness()
		r := h.run(t, nil, append(c.args, "--json")...)
		if r.code != 0 {
			t.Errorf("%q: exit %d %s %s", c.args, r.code, r.stdout, r.stderr)
			continue
		}
		envelope(t, r.stdout)
		if calls := h.fake.callList(); !slices.Contains(calls, c.call) {
			t.Errorf("%q: calls %q, want %q", c.args, calls, c.call)
		}
	}
	for _, args := range [][]string{
		{"proxy"},
		{"proxy", "3", "maybe"},
		{"proxy", "3", "test", "--server", "auto"},
		{"proxy", "3", "on", "off"},
		{"proxy", "abc", "on"},
	} {
		h := newHarness()
		r := h.run(t, nil, append(args, "--json")...)
		if r.code != 2 || h.newCalls != 0 {
			t.Errorf("%q: exit %d (want 2), manager made %d times", args, r.code, h.newCalls)
		}
	}
}

func TestFormatAppProxy(t *testing.T) {
	var b bytes.Buffer
	formatAppProxy(&b, &emu.AppProxyResult{Index: 3, Enabled: true, Server: "socks5://10.0.2.2:7890",
		Apps: []string{"com.example.game", "com.google.android.gms"}, Applied: "now",
		Notes: []string{"代理地址取自Windows 代理设置：127.0.0.1:7890"},
		Guest: &appproxy.Status{Running: true, PID: 4321, Rules: appproxy.RulesOK,
			Server: appproxy.ServerStatus{TCP: "ok", UDP: "unsupported"},
			Apps:   []appproxy.AppStatus{{Package: "com.example.game"}, {Package: "com.google.android.gms", UID: 10120}}}})
	out := b.String()
	for _, want := range []string{"实例 3 按应用走代理：开", "名单：com.example.game、com.google.android.gms", "提示：代理地址取自",
		"已生效", "规则就位", "TCP 能连，UDP 不通", "还没装的（装上就生效）：com.example.game"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	b.Reset()
	formatAppProxy(&b, &emu.AppProxyResult{Index: 2, Apps: []string{}, Applied: "failed", Error: "没有 su",
		Guest: &appproxy.Status{Rules: appproxy.RulesPartial}})
	for _, want := range []string{"：关", "名单：空", "没生效：没有 su", "规则还在（名单里的应用现在断网）"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}
