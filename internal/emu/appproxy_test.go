//go:build windows

package emu

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"modavd/internal/appproxy"
	"modavd/internal/appproxy/testproxy"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

func startTestProxy(t *testing.T, mode testproxy.Mode) *testproxy.Proxy {
	t.Helper()
	p, err := testproxy.Start(testproxy.Options{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// deadPort is a loopback port nobody listens on.
func deadPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// noHostProxy clears the proxy variables, so auto finds what the test sets (and the Windows settings, which a test
// cannot fake from here, are only read when none is set).
func noHostProxy(t *testing.T) {
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
}

func TestAppProxySetting(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	i := 3
	_, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &i})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	ptr := func(b bool) *bool { return &b }
	str := func(s string) *string { return &s }

	// never set
	r, err := m.AppProxy(ctx, i)
	if err != nil || r.Enabled || r.Server != "" || r.Apps == nil || len(r.Apps) != 0 || r.Guest != nil {
		t.Fatalf("unset: %+v %v", r, err)
	}
	if in, _ := m.Info(ctx, i); in.AppProxy != nil {
		t.Fatalf("summary of an unset proxy: %+v", in.AppProxy)
	}

	// on, an address without its kind: probed (a SOCKS5 proxy), loopback stored as the guest's 10.0.2.2
	p := startTestProxy(t, testproxy.ModeSOCKS5)
	port := p.Addr().Port()
	r, err = m.SetAppProxy(ctx, i, appproxy.Change{Enabled: ptr(true), Server: str(fmt.Sprintf("localhost:%d", port)),
		Add: []string{"com.example.game"}})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("socks5://10.0.2.2:%d", port)
	if !r.Enabled || r.Server != want || !slices.Equal(r.Apps, []string{"com.example.game"}) || r.Applied != "next_boot" ||
		len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "10.0.2.2") {
		t.Fatalf("on: %+v", r)
	}
	// stored next to the other keys, in order
	meta := m.Root.InstanceMeta(i)
	if !meta.Has("memoryMb") || !slices.Equal(meta.Get(appproxy.MetaKey).Obj().Keys(), []string{"enabled", "server", "apps"}) {
		t.Fatalf("meta keys %v / %v", meta.Keys(), meta.Get(appproxy.MetaKey).Obj().Keys())
	}
	if in, _ := m.Info(ctx, i); in.AppProxy == nil || !in.AppProxy.Enabled || in.AppProxy.Apps != 1 || in.AppProxy.Server != want {
		t.Fatalf("summary %+v", in.AppProxy)
	}

	// off keeps the address and the list
	r, err = m.SetAppProxy(ctx, i, appproxy.Change{Enabled: ptr(false)})
	if err != nil || r.Enabled || r.Server != want || len(r.Apps) != 1 || r.Applied != "off" {
		t.Fatalf("off: %+v %v", r, err)
	}

	// refused changes leave it as it was
	dead := fmt.Sprintf("127.0.0.1:%d", deadPort(t))
	for name, c := range map[string]struct {
		ch   appproxy.Change
		code string
	}{
		"bad package":       {appproxy.Change{Add: []string{"not a package"}}, errs.InvalidArgument},
		"nobody there":      {appproxy.Change{Server: str(dead)}, errs.ProxyUnavailable},
		"not an address":    {appproxy.Change{Server: str("proxy.lan")}, errs.InvalidArgument},
		"name with a kind":  {appproxy.Change{Server: str("socks5://proxy.lan:1080")}, errs.InvalidArgument},
		"nothing to change": {appproxy.Change{}, errs.Usage},
	} {
		if _, err := m.SetAppProxy(ctx, i, c.ch); errs.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if r, _ := m.AppProxy(ctx, i); r.Enabled || r.Server != want || len(r.Apps) != 1 {
		t.Fatalf("after refused changes: %+v", r)
	}

	// on with an address that does not answer: stored, with a warning
	r, err = m.SetAppProxy(ctx, i, appproxy.Change{Enabled: ptr(true), Server: str("http://" + dead)})
	if err != nil || r.Server != "http://10.0.2.2:"+strings.Split(dead, ":")[1] ||
		!slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "会断网") }) {
		t.Fatalf("dead proxy: %+v %v", r, err)
	}

	// auto: HTTPS_PROXY names an HTTP proxy, but the port speaks SOCKS5 too (a mixed port): SOCKS5 it is
	noHostProxy(t)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://127.0.0.1:%d", port))
	r, err = m.SetAppProxy(ctx, i, appproxy.Change{Server: str("auto"), Apps: &[]string{"a.b", "c.d"}})
	if err != nil || r.Server != want || !slices.Equal(r.Apps, []string{"a.b", "c.d"}) ||
		!slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "HTTPS_PROXY") }) {
		t.Fatalf("auto: %+v %v", r, err)
	}
	// auto with nothing set on the host: an error, nothing changed
	noHostProxy(t)
	t.Setenv("HTTPS_PROXY", "socks5://proxy.lan:1080") // a name: not usable in the guest, skipped
	if _, err := m.SetAppProxy(ctx, i, appproxy.Change{Server: str("auto")}); err != nil && errs.CodeOf(err) != errs.ProxyUnavailable {
		t.Fatalf("auto, nothing usable: %v", err)
	}

	// test: what speaks there, from the host
	if _, err := m.SetAppProxy(ctx, i, appproxy.Change{Server: str(fmt.Sprintf("socks5://127.0.0.1:%d", port))}); err != nil {
		t.Fatal(err)
	}
	noUDP := startTestProxy(t, testproxy.ModeSOCKS5NoUDP)
	if _, err := m.SetAppProxy(ctx, i, appproxy.Change{Server: str(fmt.Sprintf("socks5://127.0.0.1:%d", noUDP.Addr().Port()))}); err != nil {
		t.Fatal(err)
	}
	tr, err := m.TestAppProxy(ctx, i)
	if err != nil || tr.Kind != "socks5" || tr.UDP != "unsupported" || tr.HostAddr != noUDP.Addr().String() || tr.Guest != nil {
		t.Fatalf("test: %+v %v", tr, err)
	}

	// a value that is not a setting (not an object; an object with an address that is no proxy's) is not overwritten
	bad, err := jsonord.Parse([]byte(`{"enabled": true, "server": "socks5://proxy.lan:1080", "apps": ["a.b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []*jsonord.Value{jsonord.NewString("x"), bad} {
		if err := m.Root.UpdateInstanceMeta(i, map[string]*jsonord.Value{appproxy.MetaKey: v}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AppProxy(ctx, i); errs.CodeOf(err) != errs.MetadataUnreadable {
			t.Fatalf("bad value read: %v", err)
		}
		if _, err := m.SetAppProxy(ctx, i, appproxy.Change{Enabled: ptr(false)}); errs.CodeOf(err) != errs.MetadataUnreadable {
			t.Fatalf("bad value overwritten: %v", err)
		}
		if in, _ := m.Info(ctx, i); in.AppProxy == nil {
			t.Error("a bad value: no summary at all")
		}
	}
}

func TestSettingValueKeepsOtherKeys(t *testing.T) {
	old, err := jsonord.Parse([]byte(`{"server": "socks5://10.0.2.2:1", "future": 1, "dns": "8.8.8.8:53"}`))
	if err != nil {
		t.Fatal(err)
	}
	v := settingValue(old, appproxy.Setting{Enabled: true, Server: "http://10.0.2.2:2", Apps: []string{"a.b"}})
	if got := v.Obj().Keys(); !slices.Equal(got, []string{"server", "future", "enabled", "apps"}) {
		t.Fatalf("keys %v", got)
	}
	s, err := settingOf(func() *jsonord.Obj { o := jsonord.NewObject(); o.Obj().Set(appproxy.MetaKey, v); return o.Obj() }())
	if err != nil || !s.Enabled || s.Server != "http://10.0.2.2:2" || s.DNS != "" || !slices.Equal(s.Apps, []string{"a.b"}) {
		t.Fatalf("round trip %+v %v", s, err)
	}
}

func TestSumOf(t *testing.T) {
	h := strings.Repeat("0123456789abcdef", 4)
	for out, want := range map[string]string{
		h + "  /data/local/tmp/modavd-proxy/modavd-proxyd\n": h,
		"": "",
		"sha256sum: /data/local/tmp/modavd-proxy/modavd-proxyd: No such file or directory": "",
		strings.ToUpper(h) + "  f": "",
		h[:63] + "  f":             "",
		"error: device offline":    "",
	} {
		if got := sumOf(out); got != want {
			t.Errorf("%q: %q", out, got)
		}
	}
}

func TestGuestSummary(t *testing.T) {
	s := appproxy.Setting{Enabled: true, Server: "socks5://10.0.2.2:7890", Apps: []string{"a.b", "c.d"}}
	st := &appproxy.Status{Rules: appproxy.RulesOK, Server: appproxy.ServerStatus{UDP: "ok"},
		Apps: []appproxy.AppStatus{{Package: "a.b", UID: 10100}, {Package: "c.d"}}}
	if got := guestSummary(s, st); got != "2 个应用（1 个已装）→ socks5://10.0.2.2:7890，UDP 可用，规则就位" {
		t.Errorf("%q", got)
	}
}
