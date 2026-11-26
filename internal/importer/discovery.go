//go:build windows

// Package importer discovers third-party Android emulator installations and provides the
// host-side primitives used by the game import workflow. Discovery is deliberately kept
// independent from registry and filesystem access so tests can use a complete fake snapshot.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Kind string

const (
	KindLDPlayer Kind = "ldplayer"
	KindMuMu     Kind = "mumu"
)

type Instance struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ConfigPath string `json:"config_path"`
	ADBPort    int    `json:"adb_port"`
}

type Provider struct {
	Kind        Kind       `json:"kind"`
	InstallRoot string     `json:"install_root"`
	Version     string     `json:"version,omitempty"`
	Instances   []Instance `json:"instances"`
}

// Snapshot is the platform-neutral input to discovery. RegistryValues contains values
// from the provider uninstall key and Roots contains likely install roots. Callers can
// pass both sources, which makes discovery work when an emulator was installed per-user.
type Snapshot struct {
	RegistryValues map[string]string
	Roots          []string
}

func Discover(s Snapshot) []Provider {
	roots := uniqueRoots(s.Roots)
	if r := firstNonEmpty(s.RegistryValues["InstallLocation"], s.RegistryValues["DisplayIcon"]); r != "" {
		if strings.Contains(strings.ToLower(r), "ldplayer") || strings.Contains(strings.ToLower(r), "leidian") {
			roots = append(roots, trimExe(r))
		}
	}
	if r := s.RegistryValues["MuMuInstallLocation"]; r != "" {
		roots = append(roots, trimExe(r))
	}
	var out []Provider
	for _, root := range uniqueRoots(roots) {
		if p, ok := discoverLDPlayer(root); ok {
			out = append(out, p)
		}
		if p, ok := discoverMuMu(root); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i].Kind)+out[i].InstallRoot < string(out[j].Kind)+out[j].InstallRoot
	})
	return out
}

func discoverLDPlayer(root string) (Provider, bool) {
	configDir := filepath.Join(root, "vms", "config")
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return Provider{}, false
	}
	p := Provider{Kind: KindLDPlayer, InstallRoot: root, Instances: []Instance{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(strings.ToLower(e.Name()), "leidian") || !strings.HasSuffix(strings.ToLower(e.Name()), ".config") {
			continue
		}
		path := filepath.Join(configDir, e.Name())
		if !trailingDigits.MatchString(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))) {
			continue
		}
		var cfg map[string]json.RawMessage
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cfg) == nil {
			id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			name := rawString(cfg["statusSettings.playerName"])
			p.Instances = append(p.Instances, Instance{ID: id, Name: name, ConfigPath: path, ADBPort: ldPort(id)})
		}
	}
	if len(p.Instances) == 0 {
		return Provider{}, false
	}
	sort.Slice(p.Instances, func(i, j int) bool { return p.Instances[i].ID < p.Instances[j].ID })
	return p, true
}

func discoverMuMu(root string) (Provider, bool) {
	base := filepath.Join(root, "vms")
	entries, err := os.ReadDir(base)
	if err != nil {
		return Provider{}, false
	}
	p := Provider{Kind: KindMuMu, InstallRoot: root, Instances: []Instance{}}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(strings.ToLower(e.Name()), "mumuplayer-") {
			continue
		}
		config := filepath.Join(base, e.Name(), "configs", "vm_config.json")
		b, err := os.ReadFile(config)
		if err != nil {
			config = filepath.Join(base, e.Name(), "configs", "main", "vm_config.json")
			b, err = os.ReadFile(config)
		}
		if err != nil {
			continue
		}
		port := mumuPort(b)
		id := e.Name()
		p.Instances = append(p.Instances, Instance{ID: id, Name: id, ConfigPath: config, ADBPort: port})
	}
	if len(p.Instances) == 0 {
		return Provider{}, false
	}
	sort.Slice(p.Instances, func(i, j int) bool { return p.Instances[i].ID < p.Instances[j].ID })
	return p, true
}

var trailingDigits = regexp.MustCompile(`(\d+)$`)

func ldPort(id string) int {
	m := trailingDigits.FindStringSubmatch(id)
	if len(m) == 0 {
		return 5555
	}
	n, _ := strconv.Atoi(m[1])
	return 5555 + n*2
}

func mumuPort(b []byte) int {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return 0
	}
	var find func(any) int
	find = func(x any) int {
		switch x := x.(type) {
		case map[string]any:
			for k, v := range x {
				if strings.EqualFold(k, "host_port") || strings.EqualFold(k, "adb_port") {
					switch n := v.(type) {
					case float64:
						if n > 0 {
							return int(n)
						}
					case string:
						if port, err := strconv.Atoi(n); err == nil && port > 0 {
							return port
						}
					}
				}
				if n := find(v); n > 0 {
					return n
				}
			}
		case []any:
			for _, v := range x {
				if n := find(v); n > 0 {
					return n
				}
			}
		}
		return 0
	}
	return find(v)
}

func rawString(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return ""
}

func trimExe(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	if strings.HasSuffix(strings.ToLower(s), ".exe") {
		return filepath.Dir(s)
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func uniqueRoots(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		key := strings.ToLower(p)
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// ParseRegistryOutput parses the stable `reg query ... /v ...` output used by
// DiscoverInstalled. It intentionally ignores localized labels and only consumes
// the tab-separated value at the end of each line.
func ParseRegistryOutput(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && (fields[1] == "REG_SZ" || fields[1] == "REG_EXPAND_SZ") {
			values[fields[0]] = strings.Join(fields[2:], " ")
		}
	}
	return values
}

var errUnsupportedRegistry = errors.New("registry discovery is only available on Windows")

func registryError() error { return fmt.Errorf("%w", errUnsupportedRegistry) }

// DiscoverInstalled reads only uninstall metadata and known configuration folders.
// It never starts an emulator and never connects to an ADB port.
func DiscoverInstalled() ([]Provider, error) {
	if _, err := exec.LookPath("reg.exe"); err != nil {
		return nil, registryError()
	}
	var roots []string
	for _, key := range []string{
		`HKLM\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\leidian14`,
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\MuMuPlayer`,
	} {
		out, err := exec.Command("reg.exe", "query", key).Output()
		if err == nil {
			values := ParseRegistryOutput(string(out))
			if r := firstNonEmpty(values["InstallLocation"], values["DisplayIcon"]); r != "" {
				roots = append(roots, trimExe(r))
			}
		}
	}
	// The registry can omit a per-user install. These are only candidates and
	// are accepted when their instance configuration exists.
	roots = append(roots,
		`D:\leidian\LDPlayer14`,
		`C:\Program Files\Netease\MuMu`,
		`C:\Program Files\Netease\MuMuPlayer`,
	)
	return Discover(Snapshot{Roots: roots}), nil
}
