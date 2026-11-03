package appproxy

import (
	"strings"
	"testing"
)

func TestParseServer(t *testing.T) {
	for in, want := range map[string]string{
		"socks5://10.0.2.2:7890":   "socks5://10.0.2.2:7890",
		"SOCKS5h://10.0.2.2:7890/": "socks5://10.0.2.2:7890",
		"http://192.168.1.5:3128":  "http://192.168.1.5:3128",
		" socks5://10.0.2.2:1080 ": "socks5://10.0.2.2:1080",
	} {
		s, err := ParseServer(in)
		if err != nil || s.String() != want {
			t.Errorf("%q: %v %v, want %s", in, s, err, want)
		}
	}
	for _, in := range []string{
		"10.0.2.2:7890",                // no kind
		"https://10.0.2.2:7890",        // not supported
		"socks4://10.0.2.2:1080",       // not supported
		"socks5://u:p@10.0.2.2:7890",   // credentials
		"socks5://10.0.2.2",            // no port
		"socks5://10.0.2.2:0",          // bad port
		"socks5://10.0.2.2:70000",      // bad port
		"socks5://proxy.lan:7890",      // a name: the daemon cannot resolve
		"socks5://[::1]:7890",          // IPv6
		"socks5://0.0.0.0:7890",        // unspecified
		"socks5://10.0.2.2:7890/x",     // path
		"socks5://10.0.2.2:7890?a=b",   // query
		"socks5://localhost:7890",      // only GuestServer maps it
		"socks5://127.0.0.1:7890 junk", // not a URL
	} {
		if s, err := ParseServer(in); err == nil {
			t.Errorf("%q accepted: %v", in, s)
		}
	}
	if _, err := ParseServer("10.0.2.2:7890"); err == nil || !strings.Contains(err.Error(), "socks5://") {
		t.Errorf("no kind: %v", err)
	}
}

func TestGuestServer(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"socks5://127.0.0.1:7890", "socks5://10.0.2.2:7890", true},
		{"socks5://localhost:7890", "socks5://10.0.2.2:7890", true},
		{"http://127.8.9.1:3128", "http://10.0.2.2:3128", true},
		{"socks5://192.168.1.5:1080", "socks5://192.168.1.5:1080", false},
		{"socks5://10.0.2.2:7890", "socks5://10.0.2.2:7890", false},
	}
	for _, c := range cases {
		s, changed, err := GuestServer(c.in)
		if err != nil || s.String() != c.want || changed != c.changed {
			t.Errorf("%q: %v %v %v, want %s %v", c.in, s, changed, err, c.want, c.changed)
		}
	}
	if _, _, err := GuestServer("socks5://proxy.lan:7890"); err == nil {
		t.Error("a host name accepted")
	}
}

func TestValidPackage(t *testing.T) {
	for _, p := range []string{"com.example.game", "com.google.android.gms", "a.b", "com.hypergryph.arknights", "x.y_z.A9"} {
		if !ValidPackage(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range []string{"", "com", "com.", ".com", "com..x", "1com.x", "com.1x", "com.ex-ample", "com.exa mple",
		"com/x", strings.Repeat("a.", 128) + "b"} {
		if ValidPackage(p) {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{Version: 1, Server: "socks5://10.0.2.2:7890", Apps: []string{"com.example.game"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if r, _ := ok.Resolver(); r.String() != DefaultDNS {
		t.Errorf("default resolver %v", r)
	}
	apps := make([]string, MaxApps+1)
	for i := range apps {
		apps[i] = "com.example.a" + strings.Repeat("x", i)
	}
	for name, c := range map[string]Config{
		"version":   {Version: 2, Server: ok.Server},
		"server":    {Version: 1, Server: "10.0.2.2:7890"},
		"package":   {Version: 1, Server: ok.Server, Apps: []string{"nope"}},
		"twice":     {Version: 1, Server: ok.Server, Apps: []string{"a.b", "a.b"}},
		"too many":  {Version: 1, Server: ok.Server, Apps: apps},
		"dns name":  {Version: 1, Server: ok.Server, DNS: "dns.google:53"},
		"dns port":  {Version: 1, Server: ok.Server, DNS: "8.8.8.8"},
		"dns local": {Version: 1, Server: ok.Server, DNS: "127.0.0.1:53"},
		"dns v6":    {Version: 1, Server: ok.Server, DNS: "[2606:4700::1111]:53"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c, err := ParseConfig(ok.Marshal())
	if err != nil || c.Server != ok.Server || len(c.Apps) != 1 {
		t.Errorf("round trip: %+v %v", c, err)
	}
	if _, err := ParseConfig([]byte("{")); err == nil {
		t.Error("broken JSON accepted")
	}
	if ConfigSum([]byte("x")) != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Error("ConfigSum")
	}
}
