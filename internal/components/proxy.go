package components

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// proxyEnv are the variables that hand the choice to the environment.
var proxyEnv = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// readSystemProxy is systemProxy; tests put a stand-in here.
var readSystemProxy = systemProxy

// requestProxy is the proxy of a request. The script's HttpWebRequest uses the Windows proxy settings; Go reads only
// the environment. So: HTTPS_PROXY / HTTP_PROXY / NO_PROXY when any of them is set (like update), else the static
// proxy of the Windows Internet settings (systemProxy). A proxy auto-config script (PAC, WPAD) is not evaluated: set
// HTTPS_PROXY for such a network. Loopback addresses (local test mirrors) always go direct.
func requestProxy(req *http.Request) (*url.URL, error) {
	for _, k := range proxyEnv {
		if os.Getenv(k) != "" {
			return http.ProxyFromEnvironment(req)
		}
	}
	if isLoopback(req.URL.Hostname()) {
		return nil, nil
	}
	server, override, ok := readSystemProxy()
	if !ok {
		return nil, nil
	}
	return proxyFor(req.URL, server, override), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// proxyFor picks the proxy for u from the Internet settings' ProxyServer and ProxyOverride: ProxyServer is
// "host:port" for every scheme or "http=host:port;https=host:port" per scheme (a scheme it does not list goes
// direct); ProxyOverride lists hosts reached directly, ";"-separated, with "*" wildcards and "<local>" for names
// without a dot. nil: direct.
func proxyFor(u *url.URL, server, override string) *url.URL {
	host := u.Hostname()
	for _, p := range strings.Split(override, ";") {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.EqualFold(p, "<local>"):
			if !strings.Contains(host, ".") {
				return nil
			}
		case wildcard(strings.ToLower(p), strings.ToLower(host)):
			return nil
		}
	}
	chosen := ""
	for _, entry := range strings.FieldsFunc(server, func(r rune) bool { return r == ';' || r == ' ' }) {
		scheme, addr, perScheme := strings.Cut(entry, "=")
		if !perScheme {
			if chosen == "" {
				chosen = entry
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(scheme), u.Scheme) {
			chosen = strings.TrimSpace(addr)
			break
		}
	}
	if chosen == "" {
		return nil
	}
	if !strings.Contains(chosen, "://") {
		chosen = "http://" + chosen
	}
	p, err := url.Parse(chosen)
	if err != nil || p.Host == "" {
		return nil
	}
	return p
}

// wildcard matches s against pattern, where "*" stands for any run of characters.
func wildcard(pattern, s string) bool {
	star, mark := -1, 0
	i, j := 0, 0
	for j < len(s) {
		switch {
		case i < len(pattern) && pattern[i] == '*':
			star, mark = i, j
			i++
		case i < len(pattern) && pattern[i] == s[j]:
			i++
			j++
		case star >= 0:
			i, mark = star+1, mark+1
			j = mark
		default:
			return false
		}
	}
	for i < len(pattern) && pattern[i] == '*' {
		i++
	}
	return i == len(pattern)
}
