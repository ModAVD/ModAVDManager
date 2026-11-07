//go:build darwin && cgo

package hostproxy

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"modavd/internal/host"
)

// readSystem uses macOS's active manual HTTP/HTTPS/SOCKS settings. PAC and
// auto-discovery are reported for the existing HTTPS_PROXY advice, not executed.
func readSystem() settings {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, exceptions, err := host.SystemProxy(ctx)
	if err != nil {
		return settings{}
	}
	return darwinSettings(v, exceptions)
}

func darwinSettings(v map[string]string, exceptions []string) settings {
	s := settings{bypassNetworks: true, autoConfig: v["ProxyAutoConfigEnable"] == "1" || v["ProxyAutoDiscoveryEnable"] == "1"}
	var entries []string
	for _, p := range []struct{ key, scheme string }{{"HTTP", "http"}, {"HTTPS", "https"}, {"SOCKS", "socks"}} {
		n, err := strconv.Atoi(v[p.key+"Port"])
		addr := v[p.key+"Proxy"]
		if v[p.key+"Enable"] != "1" || addr == "" || n < 1 || n > 65535 || err != nil {
			continue
		}
		endpoint := net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(n))
		entries = append(entries, p.scheme+"="+endpoint)
		if p.scheme == "socks" {
			entries = append(entries, "socks5://"+endpoint)
		}
	}
	s.enabled, s.server = len(entries) > 0, strings.Join(entries, ";")
	s.bypassSimpleHosts = v["ExcludeSimpleHostnames"] == "1"
	s.override = strings.Join(exceptions, ";")
	s.from = "macOS 代理设置"
	return s
}
