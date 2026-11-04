//go:build windows

package ui

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestMarkOf(t *testing.T) {
	ok := &appproxy.Status{Running: true, Rules: appproxy.RulesOK, Server: appproxy.ServerStatus{TCP: "ok", UDP: "ok"},
		Apps: []appproxy.AppStatus{{Package: "a.b", UID: 10100}, {Package: "c.d"}}}
	with := func(f func(*appproxy.Status)) *appproxy.Status {
		c := *ok
		f(&c)
		return &c
	}
	on := func(g *appproxy.Status, f ...func(*emu.AppProxyResult)) *emu.AppProxyResult {
		r := &emu.AppProxyResult{Index: 3, Enabled: true, Server: "socks5://10.0.2.2:7890", Apps: []string{"a.b", "c.d"},
			Notes: []string{}, Guest: g}
		for _, fn := range f {
			fn(r)
		}
		return r
	}
	down := errs.New(errs.ProxyUnavailable, "连不上 127.0.0.1:7890")
	for _, c := range []struct {
		name   string
		res    *emu.AppProxyResult
		online bool
		probe  error
		tone   string // "" = no mark
		text   string
		probed bool
	}{
		{"working", on(ok), true, nil, "ok", "已生效：2 个应用（1 个还没装）→ socks5://10.0.2.2:7890", false},
		{"no UDP", on(with(func(s *appproxy.Status) { s.Server.UDP = "unsupported" })), true, nil, "ok", "（UDP 不通）", false},
		{"not running", on(nil), false, nil, "", "", false},
		{"off", on(ok, func(r *emu.AppProxyResult) { r.Enabled = false }), true, nil, "", "", false},
		{"empty list", on(nil, func(r *emu.AppProxyResult) { r.Apps = []string{} }), true, nil, "", "", false},
		{"booting", on(nil, func(r *emu.AppProxyResult) { r.Applied = "booting" }), true, nil, "", "", false},
		{"apply failed", on(nil, func(r *emu.AppProxyResult) { r.Applied, r.Error = "failed", "没有 su" }), true, nil, "err",
			"没生效：没有 su", false},
		{"no daemon", on(nil, func(r *emu.AppProxyResult) { r.Notes = []string{"实例 3 里的程序不一样"} }), true, nil, "err",
			"名单里的应用现在直连（实例 3 里的程序不一样）", false},
		{"guest not answering", on(nil, func(r *emu.AppProxyResult) { r.GuestError = "客体程序没有正常回答" }), true, nil, "warn",
			"问不到客体里的状态：客体程序没有正常回答", false},
		{"stopped, rules left", on(with(func(s *appproxy.Status) { s.Running, s.Rules = false, appproxy.RulesPartial })), true,
			nil, "err", "规则还在：名单里的应用现在断网", false},
		{"stopped", on(with(func(s *appproxy.Status) { s.Running, s.Rules, s.Error = false, appproxy.RulesOff, "端口被占" })),
			true, nil, "err", "没在运行：名单里的应用现在直连（端口被占）", false},
		{"rules failed", on(with(func(s *appproxy.Status) { s.Rules, s.RulesError = appproxy.RulesFailed, "没有 iptables" })),
			true, nil, "err", "规则挂不上：没有 iptables", false},
		{"rules partial", on(with(func(s *appproxy.Status) { s.Rules = appproxy.RulesPartial })), true, nil, "warn", "正在补上",
			false},
		{"last connection failed, the proxy answers",
			on(with(func(s *appproxy.Status) { s.Server.TCP, s.Server.LastError = "failed", "超时" })), true, nil, "warn",
			"最近一次经代理的连接没成功：超时", true},
		{"the proxy is down", on(with(func(s *appproxy.Status) { s.Server.TCP = "failed" })), true, down, "err",
			"代理连不上（名单里的应用现在断网）：连不上 127.0.0.1:7890", true},
	} {
		probed := false
		m := markOf(c.res, c.online, func() error { probed = true; return c.probe })
		switch {
		case c.tone == "" && m != nil:
			t.Errorf("%s: mark %+v, want none", c.name, m)
		case c.tone == "":
		case m == nil:
			t.Errorf("%s: no mark", c.name)
		case m.Tone != c.tone || !strings.Contains(m.Text, c.text) || m.Index != 3:
			t.Errorf("%s: %+v, want %s %q", c.name, m, c.tone, c.text)
		}
		if probed != c.probed {
			t.Errorf("%s: probed %v", c.name, probed)
		}
	}
}

func TestParsePackages(t *testing.T) {
	out := "package:com.example.tool uid:10150\r\n" +
		"package:com.hypergryph.arknights uid:10123\n" +
		"package:com.android.zzz uid:10160,1010160\n" +
		systemMarker + "\n" +
		"package:com.google.android.gms uid:10120\n" +
		"package:com.google.android.gms.policy_sidecar_aps uid:10121\n"
	got, ok := parsePackages(out)
	want := []appPackage{{"com.hypergryph.arknights", "明日方舟（官服）", 10123, false}, {"com.android.zzz", "", 10160, false},
		{"com.example.tool", "", 10150, false}, {gmsPackage, "Google Play 服务", 10120, true}}
	if !ok || !slices.Equal(got, want) {
		t.Errorf("got %v %v\nwant %v", got, ok, want)
	}
	for name, c := range map[string]struct {
		out  string
		want []appPackage
		ok   bool
	}{
		"nothing installed":   {systemMarker + "\n", []appPackage{}, true},
		"pm not up":           {"cmd: Can't find service: package\n" + systemMarker + "\n", nil, false},
		"cut short":           {"package:a.b uid:1\n", nil, false},
		"odd lines":           {"package:nodots uid:1\npackage:a.b\n" + systemMarker, []appPackage{{"a.b", "", 0, false}}, true},
		"system app only":     {"Error: x\n" + systemMarker + "\npackage:" + gmsPackage + " uid:1", nil, false},
		"warning and package": {"WARNING: linker\npackage:a.b uid:7\n" + systemMarker, []appPackage{{"a.b", "", 7, false}}, true},
	} {
		got, ok := parsePackages(c.out)
		if ok != c.ok || (ok && !slices.Equal(got, c.want)) {
			t.Errorf("%s: %v %v, want %v %v", name, got, ok, c.want, c.ok)
		}
	}
}

// setProxy gives instance i a per-app proxy: in the polled list (its summary) and in what AppProxy answers.
func (f *fake) setProxy(i int, st emu.Status, busy string, apps []string, g *appproxy.Status) {
	f.put(i, st)
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.instances[i]
	in.Busy = busy
	in.AppProxy = &emu.AppProxySummary{Enabled: true, Server: "socks5://10.0.2.2:7890", Apps: len(apps)}
	f.instances[i] = in
	f.proxy[i] = &emu.AppProxyResult{Index: i, Enabled: true, Server: "socks5://10.0.2.2:7890", Apps: apps,
		Notes: []string{}, Guest: g}
}

type marksEvent struct {
	Marks []proxyMark `json:"marks"`
}

func TestProxyEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	h.f.setProxy(3, emu.StatusOffline, "", []string{"a.b"}, nil)

	// GET: the setting, as the CLI's --json data, and no mark while it does not run
	resp := h.do("GET", "/api/instances/3/proxy", nil)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var v proxyView
	if err := json.Unmarshal(data, &v); resp.StatusCode != 200 || err != nil || !v.Enabled || v.Server != "socks5://10.0.2.2:7890" ||
		!slices.Equal(v.Apps, []string{"a.b"}) || v.Mark != nil || !strings.Contains(string(data), `"mark":null`) {
		t.Fatalf("GET: %d %s", resp.StatusCode, data)
	}
	expectError(t, h.do("GET", "/api/instances/5/proxy", nil), 400, errs.InvalidArgument)
	expectError(t, h.do("GET", "/api/instances/7/proxy", nil), 404, errs.NotFound)

	// PUT: the fields given become the change; the others stay out of it (nil, not empty)
	expectError(t, h.do("PUT", "/api/instances/3/proxy", "{}"), 400, errs.Usage)
	expectError(t, h.do("PUT", "/api/instances/3/proxy", `{"enable":true}`), 400, errs.Usage)
	expectError(t, h.do("PUT", "/api/instances/3/proxy", `{"apps":"a.b"}`), 400, errs.InvalidArgument)
	expectError(t, h.do("PUT", "/api/instances/3/proxy", `{"enabled":true}`, withHeader("Origin", "http://evil.example")),
		403, "forbidden")
	expect(t, h.do("PUT", "/api/instances/3/proxy", `{"enabled":false,"server":"auto","apps":[]}`), 200, &v)
	h.f.mu.Lock()
	c := h.f.proxySets[len(h.f.proxySets)-1]
	h.f.mu.Unlock()
	if len(h.f.proxySets) != 1 || c.Enabled == nil || *c.Enabled || c.Server == nil || *c.Server != "auto" || c.Apps == nil ||
		len(*c.Apps) != 0 || c.Add != nil || c.Remove != nil {
		t.Fatalf("change %+v (%d sets)", c, len(h.f.proxySets))
	}
	if v.Enabled || v.Applied != "next_boot" || v.Mark != nil {
		t.Fatalf("PUT answer %+v", v)
	}
	expect(t, h.do("PUT", "/api/instances/3/proxy", `{"enabled":true}`), 200, nil)
	h.f.mu.Lock()
	c = h.f.proxySets[len(h.f.proxySets)-1]
	h.f.proxyErr = errs.New(errs.ProxyUnavailable, "这台电脑没设代理")
	h.f.mu.Unlock()
	if c.Enabled == nil || !*c.Enabled || c.Server != nil || c.Apps != nil {
		t.Fatalf("change of enabled alone %+v", c)
	}
	expectError(t, h.do("PUT", "/api/instances/3/proxy", `{"server":"auto"}`), 502, errs.ProxyUnavailable)

	// applied in the running guest: the answer carries the mark
	h.f.mu.Lock()
	h.f.proxyErr, h.f.proxyApplied = nil, "now"
	h.f.proxy[3].Guest = &appproxy.Status{Running: true, Rules: appproxy.RulesFailed, RulesError: "没有 iptables"}
	h.f.mu.Unlock()
	expect(t, h.do("PUT", "/api/instances/3/proxy", `{"apps":["a.b"]}`), 200, &v)
	if v.Mark == nil || v.Mark.Tone != "err" || !strings.Contains(v.Mark.Text, "没有 iptables") {
		t.Fatalf("mark of an applied change %+v", v.Mark)
	}

	// suggest
	h.f.mu.Lock()
	h.f.suggest = &emu.AppProxySuggestion{Server: "socks5://10.0.2.2:7890", Notes: []string{"代理地址取自HTTPS_PROXY：127.0.0.1:7890"}}
	h.f.mu.Unlock()
	var sg emu.AppProxySuggestion
	expect(t, h.do("GET", "/api/proxy/suggest", nil), 200, &sg)
	if sg.Server != "socks5://10.0.2.2:7890" || len(sg.Notes) != 1 {
		t.Fatalf("suggest %+v", sg)
	}
	h.f.mu.Lock()
	h.f.suggest, h.f.suggestErr = nil, errs.New(errs.ProxyUnavailable, "这台电脑没设代理")
	h.f.mu.Unlock()
	expectError(t, h.do("GET", "/api/proxy/suggest", nil), 502, errs.ProxyUnavailable)

	// probe: with UDP, the address as typed
	var pr emu.AppProxyProbe
	expect(t, h.do("POST", "/api/proxy/probe", `{"server":"127.0.0.1:7890"}`), 200, &pr)
	if pr.Kind != "socks5" || pr.UDP != "ok" || pr.Server != "127.0.0.1:7890" {
		t.Fatalf("probe %+v", pr)
	}
	expectError(t, h.do("POST", "/api/proxy/probe", `{"server":7890}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/proxy/probe", `{"server":"x"}`, withHeader("Origin", "")), 403, "forbidden")
	h.f.mu.Lock()
	h.f.probeErr = errs.New(errs.ProxyUnavailable, "127.0.0.1:7890 上没有程序在听")
	h.f.mu.Unlock()
	expectError(t, h.do("POST", "/api/proxy/probe", `{"server":"127.0.0.1:7890"}`), 502, errs.ProxyUnavailable)
	h.f.mu.Lock()
	probes := slices.Clone(h.f.probes)
	h.f.mu.Unlock()
	if !slices.Equal(probes, []string{"127.0.0.1:7890 udp=true", "127.0.0.1:7890 udp=true"}) {
		t.Fatalf("probes %q", probes)
	}
}

func TestProxyPackages(t *testing.T) {
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)
	expect(t, h.do("GET", "/api/instances", nil), 200, nil) // the list a request may use

	// not booted per that list: 409, no adb
	e := expectError(t, h.do("GET", "/api/instances/4/packages", nil), 409, errs.Busy)
	if !strings.Contains(e.Advice, "手填包名") || len(h.f.shells) != 0 {
		t.Fatalf("offline: %+v, shells %q", e, h.f.shells)
	}

	h.f.mu.Lock()
	h.f.shellOut = "package:com.example.tool uid:10150\npackage:com.hypergryph.arknights uid:10123\n" + systemMarker +
		"\npackage:com.google.android.gms uid:10120\n"
	h.f.mu.Unlock()
	var got struct {
		Packages []appPackage `json:"packages"`
	}
	expect(t, h.do("GET", "/api/instances/3/packages", nil), 200, &got)
	if len(got.Packages) != 3 || got.Packages[0].Name != "明日方舟（官服）" || !got.Packages[2].System ||
		h.f.shells[len(h.f.shells)-1] != packagesCmd {
		t.Fatalf("packages %+v, shell %q", got.Packages, h.f.shells)
	}

	// pm not answering: 500 with what it said
	h.f.mu.Lock()
	h.f.shellOut = "cmd: Can't find service: package\n" + systemMarker + "\n"
	h.f.mu.Unlock()
	if e := expectError(t, h.do("GET", "/api/instances/3/packages", nil), 500, errs.Internal); !strings.Contains(e.Message, "Can't find service") {
		t.Fatalf("pm failed: %+v", e)
	}
	// emu says it does not run (no recent list): the same 409
	h.f.mu.Lock()
	h.f.shellErr = errs.New(errs.Unsupported, "魔改 AVD 实例 3 没有在运行")
	h.f.mu.Unlock()
	h.s.hub.dropCached()
	expectError(t, h.do("GET", "/api/instances/3/packages", nil), 409, errs.Busy)
}

// TestProxyMarks: the cards' marks follow the guests of the running instances that have it on, and go with them.
func TestProxyMarks(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.proxyEvery = 80 * time.Millisecond })
	working := &appproxy.Status{Running: true, Rules: appproxy.RulesOK, Server: appproxy.ServerStatus{TCP: "ok", UDP: "ok"},
		Apps: []appproxy.AppStatus{{Package: "a.b", UID: 10100}}}
	h.f.setProxy(3, emu.StatusOnline, "", []string{"a.b"}, working)
	h.f.setProxy(4, emu.StatusOnline, "starting", []string{"a.b"}, nil) // after-boot steps under way: not asked
	st := h.events()
	waitFor(st, "proxy", func(v marksEvent) bool { return len(v.Marks) == 1 && v.Marks[0].Index == 3 && v.Marks[0].Tone == "ok" })

	// the guest's rules go: red at the next look
	h.f.mu.Lock()
	h.f.proxy[3].Guest = &appproxy.Status{Running: true, Rules: appproxy.RulesFailed, RulesError: "没有 iptables"}
	h.f.mu.Unlock()
	waitFor(st, "proxy", func(v marksEvent) bool { return len(v.Marks) == 1 && v.Marks[0].Tone == "err" })

	// a stream that connects now gets the marks at once
	st2 := h.events()
	waitFor(st2, "proxy", func(v marksEvent) bool { return len(v.Marks) == 1 && v.Marks[0].Tone == "err" })

	// the instance shuts down: its mark goes
	h.f.put(3, emu.StatusOffline)
	waitFor(st, "proxy", func(v marksEvent) bool { return len(v.Marks) == 0 })
	h.f.mu.Lock()
	asked := slices.Clone(h.f.proxyAsked)
	h.f.mu.Unlock()
	if slices.Contains(asked, 4) {
		t.Errorf("a starting instance's guest was asked: %v", asked)
	}
}

// TestProxyMarksPace: a guest is asked once per proxyEvery however often the list is polled, and again at once when
// its instance's state or setting changes.
func TestProxyMarksPace(t *testing.T) {
	h := newHarness(t, nil) // proxyEvery stays 30 s
	h.f.setProxy(3, emu.StatusOnline, "", []string{"a.b"}, &appproxy.Status{Running: true, Rules: appproxy.RulesOK,
		Server: appproxy.ServerStatus{TCP: "ok"}})
	st := h.events()
	waitFor(st, "proxy", func(v marksEvent) bool { return len(v.Marks) == 1 })
	asked := h.f.proxyCalls.Load()
	for range 6 { // polls every 50 ms
		waitFor[json.RawMessage](st, "instances", nil)
	}
	if n := h.f.proxyCalls.Load(); n != asked {
		t.Fatalf("asked %d more times within proxyEvery", n-asked)
	}
	h.f.setProxy(3, emu.StatusOnline, "", []string{"a.b", "c.d"}, nil)
	waitFor(st, "proxy", func(v marksEvent) bool { return len(v.Marks) == 1 && v.Marks[0].Tone == "err" })
	if h.f.proxyCalls.Load() != asked+1 {
		t.Fatalf("asked %d times after the change, want 1", h.f.proxyCalls.Load()-asked)
	}
}
