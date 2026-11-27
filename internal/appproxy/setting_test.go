package appproxy

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"modavd/internal/appproxy/testproxy"
)

func ptr[T any](v T) *T { return &v }

func TestSettingApply(t *testing.T) {
	auto := func() (string, string, error) {
		return "socks5://127.0.0.1:7897", "从 Windows 代理设置读到", nil
	}
	var s Setting

	// first time on, no address: found as for auto, loopback rewritten for the guest
	s, notes, err := s.Apply(Change{Enabled: ptr(true), Add: []string{"com.example.game"}}, auto)
	if err != nil || s.Server != "socks5://10.0.2.2:7897" || !s.Active() || len(notes) != 2 {
		t.Fatalf("%+v %q %v", s, notes, err)
	}
	s, _, err = s.Apply(Change{Add: SplitApps([]string{"com.google.android.gms, com.example.game", "a.b"})}, nil)
	if err != nil || !slices.Equal(s.Apps, []string{"com.example.game", "com.google.android.gms", "a.b"}) {
		t.Fatalf("add: %v %v", s.Apps, err)
	}
	s, notes, err = s.Apply(Change{Remove: []string{"a.b", "not.there"}}, nil)
	if err != nil || len(s.Apps) != 2 || len(notes) != 1 || !strings.Contains(notes[0], "not.there") {
		t.Fatalf("remove: %v %q %v", s.Apps, notes, err)
	}
	s, _, err = s.Apply(Change{Server: ptr("http://192.168.1.5:3128"), Apps: &[]string{"x.y"}}, nil)
	if err != nil || s.Server != "http://192.168.1.5:3128" || !reflect.DeepEqual(s.Apps, []string{"x.y"}) {
		t.Fatalf("server, apps: %+v %v", s, err)
	}
	// off keeps the rest
	off, _, err := s.Apply(Change{Enabled: ptr(false)}, nil)
	if err != nil || off.Active() || off.Server != s.Server || len(off.Apps) != 1 {
		t.Fatalf("off: %+v %v", off, err)
	}
	// errors leave the setting as it was
	for name, c := range map[string]Change{
		"bad package": {Add: []string{"nope"}},
		"bad server":  {Server: ptr("10.0.2.2:7890")},
		"name server": {Server: ptr("socks5://proxy.lan:1080")},
		"auto failed": {Server: ptr("auto")},
	} {
		failing := func() (string, string, error) { return "", "", errors.New("没有") }
		if got, _, err := s.Apply(c, failing); err == nil || !reflect.DeepEqual(got, s) {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	many := make([]string, MaxApps+1)
	for i := range many {
		many[i] = "com.example.a" + strings.Repeat("x", i)
	}
	if _, _, err := s.Apply(Change{Apps: &many}, nil); err == nil {
		t.Error("too many apps")
	}
	if (Change{}).Empty() == false || (Change{Add: []string{"a.b"}}).Empty() {
		t.Error("Empty")
	}
	if err := (Setting{Apps: []string{"a.b"}}).Validate(); err != nil {
		t.Errorf("off without an address: %v", err)
	}
	if c := s.GuestConfig(); c.Validate() != nil || c.Server != s.Server {
		t.Errorf("guest config %+v", c)
	}
}

func TestProbe(t *testing.T) {
	resolverAddr := udpEcho(t) // an echo answers the query with itself: same ID, good enough for a DNS answer
	for _, c := range []struct {
		mode      testproxy.Mode
		kind, udp string
	}{
		{testproxy.ModeSOCKS5, "socks5", "ok"},
		{testproxy.ModeSOCKS5NoUDP, "socks5", "unsupported"},
		{testproxy.ModeHTTP, "http", "unsupported"},
	} {
		p := startProxy(t, c.mode, "", "", func(netip.AddrPort) netip.AddrPort { return resolverAddr })
		r, err := Probe(context.Background(), p.Addr(), true, netip.MustParseAddrPort("1.1.1.1:53"))
		if err != nil || r.Kind != c.kind || r.UDP != c.udp {
			t.Errorf("%s: %+v %v", c.mode, r, err)
		}
	}
	if _, err := Probe(context.Background(), deadServer(t).Addr, false, netip.AddrPort{}); err == nil ||
		!strings.Contains(err.Error(), "连不上") {
		t.Errorf("nobody there: %v", err)
	}
	s := oneShot(t, "SSH-2.0-x\r\n")
	if _, err := Probe(context.Background(), s.Addr, false, netip.AddrPort{}); err == nil {
		t.Error("not a proxy taken for one")
	}
}
