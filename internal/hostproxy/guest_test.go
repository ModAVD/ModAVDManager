package hostproxy

import "testing"

func TestFromProxyServer(t *testing.T) {
	cases := []struct {
		server, addr, kind string
	}{
		{"127.0.0.1:7890", "127.0.0.1:7890", ""},
		{"http=127.0.0.1:7890;https=127.0.0.1:7890;socks=127.0.0.1:7891", "127.0.0.1:7891", "socks5"},
		{"http=127.0.0.1:8080;https=127.0.0.1:8443", "127.0.0.1:8443", "http"},
		{"http=127.0.0.1:8080", "127.0.0.1:8080", "http"},
		{"localhost:10809", "127.0.0.1:10809", ""},
		{"socks=localhost:1080;192.168.1.5:3128", "127.0.0.1:1080", "socks5"},
		{"http://127.0.0.1:7890", "127.0.0.1:7890", "http"},
	}
	for _, c := range cases {
		s, ok := fromProxyServer(c.server)
		if !ok || s.Addr.String() != c.addr || s.Kind != c.kind || s.From == "" {
			t.Errorf("%q: %+v %v, want %s %q", c.server, s, ok, c.addr, c.kind)
		}
	}
	for _, server := range []string{"", "proxy.corp:8080", "[::1]:7890", "ftp=127.0.0.1:21", "127.0.0.1"} {
		if s, ok := fromProxyServer(server); ok {
			t.Errorf("%q: %+v", server, s)
		}
	}
}

func TestGuestSuggestion(t *testing.T) {
	noEnv(t, settings{enabled: true, server: "127.0.0.1:7890"})
	for _, k := range []string{"ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	if s, ok := GuestSuggestion(); !ok || s.Addr.String() != "127.0.0.1:7890" || s.From != "Windows 代理设置" {
		t.Errorf("Windows: %+v %v", s, ok)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	if s, ok := GuestSuggestion(); !ok || s.Kind != "http" || s.From != "HTTPS_PROXY" || s.AuthDropped {
		t.Errorf("HTTPS_PROXY: %+v", s)
	}
	t.Setenv("HTTPS_PROXY", "http://user:secret@127.0.0.1:3128") // the address without them, and it says so
	if s, ok := GuestSuggestion(); !ok || s.Addr.String() != "127.0.0.1:3128" || !s.AuthDropped {
		t.Errorf("HTTPS_PROXY with a password: %+v", s)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	t.Setenv("all_proxy", "socks5h://localhost:1080")
	if s, ok := GuestSuggestion(); !ok || s.Kind != "socks5" || s.Addr.String() != "127.0.0.1:1080" || s.From != "ALL_PROXY" {
		t.Errorf("all_proxy: %+v", s)
	}
	t.Setenv("all_proxy", "socks5://proxy.lan:1080") // a name: skipped
	if s, _ := GuestSuggestion(); s.From != "HTTPS_PROXY" {
		t.Errorf("name skipped: %+v", s)
	}
	noEnv(t, settings{})
	for _, k := range []string{"ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	if s, ok := GuestSuggestion(); ok {
		t.Errorf("nothing set: %+v", s)
	}
}
