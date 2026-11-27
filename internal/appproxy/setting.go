package appproxy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MetaKey is the key of an instance's setting in mas-avd.json (instances.<i>.appProxy).
const MetaKey = "appProxy"

// Setting is an instance's per-app proxy as modavd keeps it. Server is as the guest reaches the proxy, with its kind
// (socks5://10.0.2.2:7890). Off keeps the address and the list for next time.
type Setting struct {
	Enabled bool     `json:"enabled"`
	Server  string   `json:"server"`
	Apps    []string `json:"apps"`
	DNS     string   `json:"dns,omitempty"`
}

// Validate is Config.Validate for what is stored; an empty server is fine while off.
func (s Setting) Validate() error {
	if !s.Enabled && s.Server == "" {
		c := Config{Version: ConfigVersion, Server: "socks5://" + HostLoopback + ":1", Apps: s.Apps, DNS: s.DNS}
		return c.Validate()
	}
	return s.GuestConfig().Validate()
}

// GuestConfig is the config.json for the guest.
func (s Setting) GuestConfig() *Config {
	return &Config{Version: ConfigVersion, Server: s.Server, Apps: slices.Clone(s.Apps), DNS: s.DNS}
}

// Active: the guest should run the daemon (on, and something listed).
func (s Setting) Active() bool { return s.Enabled && len(s.Apps) > 0 }

// Auto is the Server value of a Change that asks for the host's own proxy setting (Suggest).
const Auto = "auto"

// Change is an edit of a Setting, from the command line, MCP or the manager window. Nil fields stay as they are.
type Change struct {
	Enabled *bool
	Server  *string   // a proxy address as typed on the host, or Auto
	Apps    *[]string // the whole list
	Add     []string
	Remove  []string
}

// Empty: nothing to change.
func (c Change) Empty() bool {
	return c.Enabled == nil && c.Server == nil && c.Apps == nil && len(c.Add) == 0 && len(c.Remove) == 0
}

// Resolver finds the proxy for Auto: the guest's address of the host's proxy and how it was found.
type Resolver func() (server string, note string, err error)

// Apply returns s with c applied, and notes for the user (an address that was rewritten for the guest, say). The
// first time it is turned on without an address, the address is found as for Auto.
func (s Setting) Apply(c Change, auto Resolver) (Setting, []string, error) {
	var notes []string
	out := s
	out.Apps = slices.Clone(s.Apps)
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	server := c.Server
	if server == nil && out.Enabled && out.Server == "" {
		a := Auto
		server = &a
	}
	if server != nil {
		v := strings.TrimSpace(*server)
		if strings.EqualFold(v, Auto) {
			if auto == nil {
				return s, nil, errors.New("找不到本机的代理设置")
			}
			found, note, err := auto()
			if err != nil {
				return s, nil, err
			}
			v = found
			if note != "" {
				notes = append(notes, note)
			}
		}
		srv, changed, err := GuestServer(v)
		if err != nil {
			return s, nil, err
		}
		if changed {
			notes = append(notes, fmt.Sprintf("实例里连宿主要用 %s，代理地址记成了 %s", HostLoopback, srv))
		}
		out.Server = srv.String()
	}
	if c.Apps != nil {
		out.Apps = nil
		for _, p := range *c.Apps {
			out.Apps = addApp(out.Apps, p)
		}
	}
	for _, p := range c.Add {
		out.Apps = addApp(out.Apps, p)
	}
	for _, p := range c.Remove {
		p = strings.TrimSpace(p)
		if i := slices.Index(out.Apps, p); i >= 0 {
			out.Apps = slices.Delete(out.Apps, i, i+1)
		} else {
			notes = append(notes, p+" 不在名单里")
		}
	}
	for _, p := range out.Apps {
		if !ValidPackage(p) {
			return s, nil, fmt.Errorf("不是安卓包名：%q", p)
		}
	}
	if len(out.Apps) > MaxApps {
		return s, nil, fmt.Errorf("名单里最多 %d 个应用", MaxApps)
	}
	if out.Enabled && out.Server == "" {
		return s, nil, errors.New("要先给代理地址")
	}
	if err := out.Validate(); err != nil {
		return s, nil, err
	}
	return out, notes, nil
}

func addApp(apps []string, p string) []string {
	p = strings.TrimSpace(p)
	if p == "" || slices.Contains(apps, p) {
		return apps
	}
	return append(apps, p)
}

// SplitApps splits "a.b,c.d" lists (repeatable command line values).
func SplitApps(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
