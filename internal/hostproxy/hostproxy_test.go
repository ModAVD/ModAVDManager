package hostproxy

import (
	"net/http"
	"net/url"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	p, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noEnv clears the proxy variables for the test and stubs the Internet settings.
func noEnv(t *testing.T, s settings) {
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	saved := system
	system = func() settings { return s }
	t.Cleanup(func() { system = saved })
}

func TestProxyFor(t *testing.T) {
	cases := []struct {
		target, server, override, want string
	}{
		{"https://dl.google.com/x", "127.0.0.1:7890", "", "http://127.0.0.1:7890"},
		{"https://dl.google.com/x", "http=10.0.0.1:80;https=10.0.0.2:443", "", "http://10.0.0.2:443"},
		{"http://dl.google.com/x", "http=10.0.0.1:80;https=10.0.0.2:443", "", "http://10.0.0.1:80"},
		{"https://dl.google.com/x", "http=10.0.0.1:80", "", ""},
		{"https://dl.google.com/x", "http://proxy.example:3128", "", "http://proxy.example:3128"},
		{"https://dl.google.com/x", "127.0.0.1:7890", "*.google.com;<local>", ""},
		{"https://mirrors.cloud.tencent.com/x", "127.0.0.1:7890", "*.google.com;<local>", "http://127.0.0.1:7890"},
		{"https://intranet/x", "127.0.0.1:7890", "<local>", ""},
		{"https://DL.Google.COM/x", "127.0.0.1:7890", "dl.google.*", ""},
	}
	for _, c := range cases {
		got := proxyFor(mustURL(t, c.target), c.server, c.override)
		if (got == nil && c.want != "") || (got != nil && got.String() != c.want) {
			t.Errorf("%s %q %q: %v, want %q", c.target, c.server, c.override, got, c.want)
		}
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopback(h) {
			t.Errorf("%s not loopback", h)
		}
	}
	if isLoopback("dl.google.com") || !wildcard("a*c", "abbbc") || wildcard("a*c", "abd") || !wildcard("*", "") {
		t.Error("wildcard")
	}
}

// TestForRequestSystem: without the variables the Internet settings decide; loopback targets go direct.
func TestForRequestSystem(t *testing.T) {
	noEnv(t, settings{enabled: true, server: "127.0.0.1:7890", override: "<local>"})
	for target, want := range map[string]string{
		"https://api.github.com/repos": "http://127.0.0.1:7890",
		"https://127.0.0.1:8443/x":     "",
		"http://localhost/x":           "",
		"https://intranet/x":           "",
	} {
		got, err := ForRequest(&http.Request{URL: mustURL(t, target)})
		if err != nil || (got == nil) != (want == "") || (got != nil && got.String() != want) {
			t.Errorf("%s: %v %v, want %q", target, got, err, want)
		}
	}
	for _, s := range []settings{{autoConfig: true}, {server: "127.0.0.1:7890"}} { // PAC only; turned off, address left
		noEnv(t, s)
		if got, _ := ForRequest(&http.Request{URL: mustURL(t, "https://api.github.com/")}); got != nil {
			t.Errorf("%+v: %v", s, got)
		}
	}
}

// TestForRequestEnv: any of the variables set, the environment alone decides (NO_PROXY included) and the Internet
// settings are not read. The only test here that calls http.ProxyFromEnvironment, which reads the environment once
// per process.
func TestForRequestEnv(t *testing.T) {
	noEnv(t, settings{enabled: true, server: "10.9.9.9:1"})
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	t.Setenv("NO_PROXY", "mirrors.example")
	got, err := ForRequest(&http.Request{URL: mustURL(t, "https://api.github.com/")})
	if err != nil || got == nil || got.String() != "http://127.0.0.1:3128" {
		t.Errorf("HTTPS_PROXY: %v %v", got, err)
	}
	if got, _ := ForRequest(&http.Request{URL: mustURL(t, "https://mirrors.example/x")}); got != nil {
		t.Errorf("NO_PROXY: %v", got)
	}
	if Advice() != "" {
		t.Error("advice with the variables set")
	}
}

// TestAdvice: only a PAC without a static proxy, and no variables, gets the HTTPS_PROXY hint.
func TestAdvice(t *testing.T) {
	for _, c := range []struct {
		s    settings
		want bool
	}{
		{settings{}, false},
		{settings{autoConfig: true}, true},
		{settings{autoConfig: true, enabled: true, server: "127.0.0.1:7890"}, false},
		{settings{enabled: true, server: "127.0.0.1:7890"}, false},
	} {
		noEnv(t, c.s)
		if got := Advice(); (got != "") != c.want {
			t.Errorf("%+v: %q", c.s, got)
		}
	}
	noEnv(t, settings{autoConfig: true})
	t.Setenv("no_proxy", "mirrors.example") // bypasses alone do not disable the system proxy or PAC advice
	if envSet() || Advice() == "" {
		t.Error("NO_PROXY alone")
	}
}

func TestOnlyNoProxyKeepsSystemProxy(t *testing.T) {
	noEnv(t, settings{enabled: true, server: "127.0.0.1:7890"})
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(key, "example.com,.internal.test,10.0.0.0/8,[2001:db8::1]:443,port.test:8443")
		for target, direct := range map[string]bool{
			"https://api.github.com/x": false, "https://example.com": true, "https://a.example.com": true,
			"https://notexample.com": false, "https://internal.test": false, "https://a.internal.test": true,
			"https://10.2.3.4": true, "https://[2001:db8::1]": true, "http://[2001:db8::1]": false,
			"https://port.test:8443": true, "https://port.test": false,
		} {
			got, err := ForRequest(&http.Request{URL: mustURL(t, target)})
			if err != nil || (got == nil) != direct || got != nil && got.String() != "http://127.0.0.1:7890" {
				t.Errorf("%s %s: %v %v", key, target, got, err)
			}
		}
		t.Setenv(key, "*")
		if got, _ := ForRequest(&http.Request{URL: mustURL(t, "https://api.github.com")}); got != nil {
			t.Fatal(got)
		}
		t.Setenv(key, "")
	}
}
