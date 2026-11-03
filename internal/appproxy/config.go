// Package appproxy is the per-app proxy of an instance (ModAVDManager#27): the listed apps' TCP, UDP and DNS go
// through the user's proxy on the host, every other app goes direct.
//
// In the guest, modavd-proxyd (cmd/modavd-proxyd, run as root) gets the listed apps' packets from the kernel: uid
// policy routing and iptables marks send them to loopback, where TPROXY hands them to the daemon's sockets with the
// original destination intact (rules.go). The daemon relays TCP with SOCKS5 CONNECT (or HTTP CONNECT), UDP with
// SOCKS5 UDP ASSOCIATE, and answers DNS itself, asking the resolver over TCP through the proxy (dns.go). With the
// rules in place and the daemon or the proxy gone, those apps have no network: nothing falls back to direct.
//
// modavd writes config.json, pushes the daemon (embedded in modavd.exe, see internal/guest) and runs
// `modavd-proxyd start` on every boot. This package is both sides' shared part (config, status, protocols, rules);
// what only the guest runs is in the *_linux.go files. User-facing text is Chinese, like the rest of modavd.
package appproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	// GuestDir holds the daemon, its config, state and log in the guest.
	GuestDir   = "/data/local/tmp/modavd-proxy"
	DaemonName = "modavd-proxyd"
	ConfigName = "config.json"
	StateName  = "state.json"
	LogName    = "proxyd.log"
	LockName   = "proxyd.lock" // holds the running daemon's pid

	// ConfigVersion is Config.Version.
	ConfigVersion = 1
	// MaxApps is the longest app list.
	MaxApps = 64
	// DefaultDNS is the resolver the listed apps' DNS goes to (over TCP, through the proxy) when the app did not pick
	// a public one itself.
	DefaultDNS = "1.1.1.1:53"
	// HostLoopback is the host's 127.0.0.1 as the guest reaches it (the emulator's user-mode network).
	HostLoopback = "10.0.2.2"
)

// Config is the guest's config.json, written by modavd before `modavd-proxyd start`.
type Config struct {
	Version int      `json:"version"`
	Server  string   `json:"server"`        // socks5://ip:port or http://ip:port, as the guest reaches it
	Apps    []string `json:"apps"`          // package names
	DNS     string   `json:"dns,omitempty"` // ipv4:port; "" is DefaultDNS
}

// Validate checks everything the daemon relies on.
func (c *Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("配置版本是 %d，这个版本的 modavd-proxyd 只认 %d", c.Version, ConfigVersion)
	}
	if _, err := ParseServer(c.Server); err != nil {
		return err
	}
	if len(c.Apps) > MaxApps {
		return fmt.Errorf("名单里有 %d 个应用，最多 %d 个", len(c.Apps), MaxApps)
	}
	seen := map[string]bool{}
	for _, p := range c.Apps {
		if !ValidPackage(p) {
			return fmt.Errorf("不是安卓包名：%q", p)
		}
		if seen[p] {
			return fmt.Errorf("%s 在名单里出现了两次", p)
		}
		seen[p] = true
	}
	_, err := c.Resolver()
	return err
}

// Resolver is the DNS address to use.
func (c *Config) Resolver() (netip.AddrPort, error) {
	s := c.DNS
	if s == "" {
		s = DefaultDNS
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 || ap.Addr().IsLoopback() || ap.Addr().IsUnspecified() {
		return netip.AddrPort{}, fmt.Errorf("DNS 要写成 IPv4 地址加端口（比如 %s）：%q", DefaultDNS, c.DNS)
	}
	return ap, nil
}

// Marshal is the config as written to the guest.
func (c *Config) Marshal() []byte {
	b, _ := json.Marshal(c)
	return append(b, '\n')
}

// ParseConfig reads and validates config.json.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config.json 读不懂：%v", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ConfigSum is the SHA-256 of a config file's bytes, as Status.ConfigSHA256 reports it.
func ConfigSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Server is the user's proxy.
type Server struct {
	Scheme string // "socks5" or "http"
	Addr   netip.AddrPort
}

func (s Server) String() string { return s.Scheme + "://" + s.Addr.String() }

// SOCKS5 tells the two kinds apart (only SOCKS5 relays UDP).
func (s Server) SOCKS5() bool { return s.Scheme == "socks5" }

// ParseServer reads "socks5://ip:port" or "http://ip:port" ("socks5h" is the same as "socks5": names are always sent
// to the proxy when known). The host must be an IPv4 address: the daemon has no resolver of its own.
func ParseServer(s string) (Server, error) {
	scheme, host, port, err := splitServer(s)
	if err != nil {
		return Server{}, err
	}
	a, ok := ipv4(host)
	if !ok {
		return Server{}, fmt.Errorf("代理地址要写 IPv4 地址：%q", s)
	}
	return Server{Scheme: scheme, Addr: netip.AddrPortFrom(a, port)}, nil
}

// GuestServer is ParseServer for what a user typed on the host: localhost and 127.x become HostLoopback, the guest's
// way to the host, and changed says so.
func GuestServer(s string) (srv Server, changed bool, err error) {
	scheme, host, port, err := splitServer(s)
	if err != nil {
		return Server{}, false, err
	}
	if a, err := netip.ParseAddr(host); strings.EqualFold(host, "localhost") || (err == nil && a.IsLoopback()) {
		host, changed = HostLoopback, true
	}
	a, ok := ipv4(host)
	if !ok {
		return Server{}, false, fmt.Errorf("代理地址要写 IPv4 地址或 localhost：%q", s)
	}
	return Server{Scheme: scheme, Addr: netip.AddrPortFrom(a, port)}, changed, nil
}

func ipv4(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(host)
	return a, err == nil && a.Is4() && !a.IsUnspecified()
}

var errNoScheme = errors.New("代理地址前面要写类型：socks5://… 或 http://…")

func splitServer(s string) (scheme, host string, port uint16, err error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		return "", "", 0, errNoScheme
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", 0, fmt.Errorf("不是代理地址：%q", s)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		scheme = "socks5"
	case "http":
		scheme = "http"
	default:
		return "", "", 0, fmt.Errorf("不支持 %s 代理，只支持 socks5:// 和 http://", u.Scheme)
	}
	if u.User != nil {
		return "", "", 0, errors.New("不支持要账号密码的代理")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", "", 0, fmt.Errorf("代理地址只写「类型://地址:端口」：%q", s)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil || n < 1 || n > 65535 {
		return "", "", 0, fmt.Errorf("代理地址要带端口（1–65535）：%q", s)
	}
	return scheme, u.Hostname(), uint16(n), nil
}

// ValidPackage: an Android package name, two or more dot-separated parts, each a letter then letters, digits or _.
func ValidPackage(p string) bool {
	if len(p) > 255 {
		return false
	}
	parts := strings.Split(p, ".")
	if len(parts) < 2 {
		return false
	}
	for _, s := range parts {
		if s == "" || !isLetter(s[0]) {
			return false
		}
		for i := 1; i < len(s); i++ {
			if c := s[i]; !isLetter(c) && (c < '0' || c > '9') && c != '_' {
				return false
			}
		}
	}
	return true
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
