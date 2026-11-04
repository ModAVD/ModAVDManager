// Package hostproxy is the proxy the manager's own downloads use (components fetch, update): HTTPS_PROXY /
// HTTP_PROXY when a proxy address is set, else the static proxy of the Windows Internet settings (the proxy
// set by hand under "Internet Options" or Settings > Network > Proxy, which is also where proxy programs put theirs).
// A proxy auto-config script (PAC, WPAD) is not evaluated: set HTTPS_PROXY for such a network (Advice says so).
// Loopback targets (local test mirrors) always go direct.
package hostproxy

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var envKeys = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// envSet: a proxy address is set, so the environment decides alone. NO_PROXY only adds bypasses.
func envSet() bool {
	for _, k := range envKeys[:4] {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// ForRequest is an http.Transport Proxy.
func ForRequest(req *http.Request) (*url.URL, error) {
	if envSet() {
		return http.ProxyFromEnvironment(req)
	}
	if isLoopback(req.URL.Hostname()) || bypassEnv(req.URL) {
		return nil, nil
	}
	s := system()
	if !s.enabled {
		return nil, nil
	}
	return proxyFor(req.URL, s.server, s.override), nil
}

// bypassEnv applies NO_PROXY to the system proxy too. Match hosts, optional ports, IPs and CIDRs using the
// environment convention: example.com includes subdomains, .example.com matches subdomains only.
func bypassEnv(u *url.URL) bool {
	list := os.Getenv("NO_PROXY")
	if list == "" {
		list = os.Getenv("no_proxy")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ip := net.ParseIP(host)
	for _, entry := range strings.Split(list, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "*" {
			return true
		}
		if entry == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if h, p, err := net.SplitHostPort(entry); err == nil {
			if p != port {
				continue
			}
			entry = h
		}
		entry = strings.Trim(entry, "[]")
		if address := net.ParseIP(entry); address != nil {
			if ip != nil && address.Equal(ip) {
				return true
			}
			continue
		}
		entry = strings.TrimPrefix(entry, "*")
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
		} else if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// Advice is the hint for a failed download when the proxy may be the cause: "" unless no proxy was used because the
// Internet settings use an auto-config script.
func Advice() string {
	if envSet() {
		return ""
	}
	if s := system(); !s.enabled && s.autoConfig {
		return "这台电脑的代理是自动配置脚本（PAC），本程序不读它；请把 HTTPS_PROXY 环境变量设成代理的地址（比如 http://127.0.0.1:7890）再试"
	}
	return ""
}

// settings are the Internet settings that matter here.
type settings struct {
	enabled          bool   // ProxyEnable and a ProxyServer
	server, override string // ProxyServer, ProxyOverride
	autoConfig       bool   // AutoConfigURL is set
}

// system reads them (a variable for the tests).
var system = readSystem

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
