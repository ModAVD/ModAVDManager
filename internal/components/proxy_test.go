package components

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProxyFor: ProxyServer and ProxyOverride of the Internet settings, as proxyFor reads them.
func TestProxyFor(t *testing.T) {
	for _, c := range []struct{ url, server, override, want string }{
		{"https://dl.google.com/x.zip", "proxy:8080", "", "http://proxy:8080"},
		{"http://dl.google.com/x.zip", "proxy:8080", "", "http://proxy:8080"},
		{"https://dl.google.com/x.zip", "http=hp:1;https=sp:2", "", "http://sp:2"},
		{"http://dl.google.com/x.zip", "http=hp:1;https=sp:2", "", "http://hp:1"},
		{"https://dl.google.com/x.zip", "HTTPS=sp:2", "", "http://sp:2"},
		{"https://dl.google.com/x.zip", "socks=s:1 https=sp:2", "", "http://sp:2"},
		// a scheme the per-scheme list does not name goes direct
		{"https://dl.google.com/x.zip", "http=hp:1", "", ""},
		// one for every scheme, then a per-scheme one: the per-scheme one wins for its scheme only
		{"https://dl.google.com/x.zip", "all:1;https=sp:2", "", "http://sp:2"},
		{"http://dl.google.com/x.zip", "all:1;https=sp:2", "", "http://all:1"},
		{"https://dl.google.com/x.zip", "https://sp:2", "", "https://sp:2"},
		{"https://dl.google.com/x.zip", "", "", ""},
		{"https://dl.google.com/x.zip", "http://", "", ""},
		{"https://dl.google.com/x.zip", "proxy:8080", "*.google.com", ""},
		{"https://dl.google.com/x.zip", "proxy:8080", "other; *.GOOGLE.com", ""},
		{"https://mirrors.cloud.tencent.com/x.zip", "proxy:8080", "*.google.com", "http://proxy:8080"},
		{"http://intranet/x.zip", "proxy:8080", "<local>", ""},
		{"https://dl.google.com/x.zip", "proxy:8080", "<LOCAL>", "http://proxy:8080"},
		{"http://10.1.2.3/x.zip", "proxy:8080", "10.*", ""},
	} {
		req, err := http.NewRequest(http.MethodGet, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if u := proxyFor(req.URL, c.server, c.override); u != nil {
			got = u.String()
		}
		if got != c.want {
			t.Errorf("proxyFor(%s, %q, %q) = %q, want %q", c.url, c.server, c.override, got, c.want)
		}
	}
}

func TestWildcard(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "x", true}, {"a*c", "abc", true}, {"a*c", "ac", true}, {"a*c", "ab", false},
		{"*.x.com", "a.x.com", true}, {"*.x.com", "x.com", false}, {"a**b", "ab", true}, {"abc", "abd", false},
	} {
		if got := wildcard(c.pattern, c.s); got != c.want {
			t.Errorf("wildcard(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}

// TestRequestProxySystem: with none of proxyEnv set, the Windows settings decide; loopback hosts go direct.
func TestRequestProxySystem(t *testing.T) {
	for _, k := range proxyEnv {
		t.Setenv(k, "")
	}
	old := readSystemProxy
	t.Cleanup(func() { readSystemProxy = old })
	readSystemProxy = func() (string, string, bool) { return "proxy:8080", "*.corp.test", true }
	proxyOf := func(raw string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		u, err := requestProxy(req)
		if err != nil {
			t.Fatal(err)
		}
		if u == nil {
			return ""
		}
		return u.String()
	}
	if got := proxyOf("https://dl.google.com/x.zip"); got != "http://proxy:8080" {
		t.Errorf("system proxy: %q", got)
	}
	if got := proxyOf("https://mirror.corp.test/x.zip"); got != "" {
		t.Errorf("override: %q", got)
	}
	for _, raw := range []string{"http://127.0.0.1:8080/x", "http://localhost/x", "http://LOCALHOST:1/x", "http://[::1]:1/x"} {
		if got := proxyOf(raw); got != "" {
			t.Errorf("%s: %q, want direct", raw, got)
		}
	}
	readSystemProxy = func() (string, string, bool) { return "", "", false }
	if got := proxyOf("https://dl.google.com/x.zip"); got != "" {
		t.Errorf("no system proxy: %q", got)
	}
}

// TestRequestProxyEnv: any of proxyEnv set hands the choice to the environment, and the Windows settings are not
// read. net/http reads the environment once per process, so each case runs in a child (TestRequestProxyEnvChild).
func TestRequestProxyEnv(t *testing.T) {
	var clean []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range proxyEnv {
			drop = drop || strings.EqualFold(k, p)
		}
		if !drop {
			clean = append(clean, kv)
		}
	}
	for _, c := range []struct {
		env       []string
		url, want string
	}{
		{[]string{"HTTPS_PROXY=http://envproxy:3128"}, "https://dl.google.com/x.zip", "http://envproxy:3128"},
		{[]string{"HTTP_PROXY=http://envproxy:3128"}, "http://dl.google.com/x.zip", "http://envproxy:3128"},
		{[]string{"HTTPS_PROXY=http://envproxy:3128", "NO_PROXY=dl.google.com"}, "https://dl.google.com/x.zip", ""},
		// only NO_PROXY: the environment still decides (direct), the system proxy is not asked
		{[]string{"NO_PROXY=example.com"}, "https://dl.google.com/x.zip", ""},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRequestProxyEnvChild$")
		cmd.Env = append(append(append([]string(nil), clean...), c.env...), "MODAVD_PROXY_CHILD="+c.url+"|"+c.want)
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "PASS") {
			t.Errorf("%v %s: %v\n%s", c.env, c.url, err, out)
		}
	}
}

func TestRequestProxyEnvChild(t *testing.T) {
	spec := os.Getenv("MODAVD_PROXY_CHILD")
	if spec == "" {
		t.Skip("run by TestRequestProxyEnv")
	}
	raw, want, _ := strings.Cut(spec, "|")
	readSystemProxy = func() (string, string, bool) {
		t.Error("the Windows proxy settings were read although the environment names a proxy")
		return "system:1", "", true
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := requestProxy(req)
	got := ""
	if u != nil {
		got = u.String()
	}
	if err != nil || got != want {
		t.Fatalf("%s: proxy %q, %v; want %q", raw, got, err, want)
	}
}
