package hostproxy

import (
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// Suggestion is the host's proxy, for an instance's per-app proxy to use. Addr is as the host reaches it (the guest
// reaches a loopback one at 10.0.2.2); Kind is "socks5" or "http" when the setting says, "" when it does not (a
// Windows entry for every protocol, a variable without a scheme): probe the port then.
type Suggestion struct {
	Addr netip.AddrPort
	Kind string
	From string // where it was found: the variable's name, or "Windows 代理设置"
	// AuthDropped: the setting had a user name or password, which Addr leaves out (the per-app proxy has no
	// authentication).
	AuthDropped bool
}

// GuestSuggestion looks where ForRequest does, with the guest's needs first: ALL_PROXY (often socks5://, which the
// downloads do not use), HTTPS_PROXY, HTTP_PROXY; then the Windows manual proxy, its socks= entry before the one for
// every protocol, then https=, then http=. Only IPv4 addresses and localhost are taken: the guest has no resolver
// for the host's names.
func GuestSuggestion() (Suggestion, bool) {
	for _, k := range []string{"ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY"} {
		v := os.Getenv(k)
		if v == "" {
			v = os.Getenv(strings.ToLower(k))
		}
		if v == "" {
			continue
		}
		if s, ok := parseSuggestion(v, ""); ok {
			s.From = k
			return s, true
		}
	}
	st := system()
	if !st.enabled {
		return Suggestion{}, false
	}
	return fromProxyServer(st.server)
}

// fromProxyServer picks from the Internet settings' ProxyServer.
func fromProxyServer(server string) (Suggestion, bool) {
	entries := map[string]string{}
	for _, e := range strings.FieldsFunc(server, func(r rune) bool { return r == ';' || r == ' ' }) {
		scheme, addr, perScheme := strings.Cut(e, "=")
		if !perScheme {
			addr, scheme = e, ""
		}
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if _, seen := entries[scheme]; !seen {
			entries[scheme] = strings.TrimSpace(addr)
		}
	}
	for _, p := range []struct{ scheme, kind string }{{"socks", "socks5"}, {"", ""}, {"https", "http"}, {"http", "http"}} {
		if v, ok := entries[p.scheme]; ok {
			if s, ok := parseSuggestion(v, p.kind); ok {
				s.From = "Windows 代理设置"
				return s, true
			}
		}
	}
	return Suggestion{}, false
}

// parseSuggestion reads "[scheme://][user:password@]host:port"; kind is used when there is no scheme.
func parseSuggestion(v, kind string) (Suggestion, bool) {
	v = strings.TrimSpace(v)
	auth := false
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return Suggestion{}, false
		}
		switch strings.ToLower(u.Scheme) {
		case "socks5", "socks5h", "socks":
			kind = "socks5"
		case "http", "https": // https:// for a proxy usually still means a plain HTTP proxy on that port
			kind = "http"
		default:
			return Suggestion{}, false
		}
		v, auth = u.Host, u.User != nil
	}
	host, port, ok := strings.Cut(v, ":")
	if !ok || strings.Contains(port, ":") {
		return Suggestion{}, false
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ap, err := netip.ParseAddrPort(host + ":" + port)
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 || ap.Addr().IsUnspecified() {
		return Suggestion{}, false
	}
	return Suggestion{Addr: ap, Kind: kind, AuthDropped: auth}, true
}
