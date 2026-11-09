package components

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/host"
)

// SourceSettings is the component download source selected by the user. An
// empty Mirror means automatic order; MirrorURLs, when non-empty, are custom
// HTTP(S) bases and take precedence over Mirror.
type SourceSettings struct {
	Mirror     string
	MirrorURLs []string
	// Set distinguishes a saved automatic choice from a missing settings file.
	// It is intentionally not serialized.
	Set bool `json:"-"`
}

type sourceSettingsFile struct {
	Mirror     string   `json:"mirror,omitempty"`
	MirrorURLs []string `json:"mirror_urls,omitempty"`
}

// sourceSettingsPath is kept as a variable so tests can point it at a
// temporary file. Production uses the same per-user directory as update
// settings, but a separate file so the two settings cannot overwrite one
// another.
var sourceSettingsPath = func() string {
	base := host.UserSettingsDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "components-settings.json")
}

// LoadSourceSettings reads the per-user component source. Missing, malformed,
// or unsupported files are treated as no saved choice (automatic order).
func LoadSourceSettings() SourceSettings {
	s := SourceSettings{}
	path := sourceSettingsPath()
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f sourceSettingsFile
	if json.Unmarshal(b, &f) != nil {
		return s
	}
	if err := validateSource(f.Mirror, f.MirrorURLs); err != nil {
		return s
	}
	s.Mirror, s.MirrorURLs, s.Set = strings.TrimSpace(f.Mirror), append([]string(nil), f.MirrorURLs...), true
	return s
}

// SaveSourceSettings validates and atomically writes a source choice. An empty
// Mirror with no URLs is a saved automatic choice; this is different from a
// missing file because interactive fetch should not ask again.
func SaveSourceSettings(mirror string, urls []string) (SourceSettings, error) {
	mirror = strings.TrimSpace(mirror)
	urls = normalizeURLs(urls)
	if err := validateSource(mirror, urls); err != nil {
		return SourceSettings{}, err
	}
	s := SourceSettings{Mirror: mirror, MirrorURLs: urls, Set: true}
	f := sourceSettingsFile{Mirror: mirror, MirrorURLs: urls}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return SourceSettings{}, errs.Wrap(errs.Internal, err, "保存组件下载源失败")
	}
	path := sourceSettingsPath()
	if path == "" {
		return SourceSettings{}, errs.New(errs.Internal, "找不到当前用户的配置目录，无法保存组件下载源")
	}
	dirMode := os.FileMode(0o755)
	if runtime.GOOS == "darwin" {
		dirMode = 0o700
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return SourceSettings{}, errs.Wrap(errs.Internal, err, "保存组件下载源失败: "+err.Error())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return SourceSettings{}, errs.Wrap(errs.Internal, err, "保存组件下载源失败: "+err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return SourceSettings{}, errs.Wrap(errs.Internal, err, "保存组件下载源失败: "+err.Error())
	}
	return s, nil
}

func normalizeURLs(urls []string) []string {
	var out []string
	for _, raw := range urls {
		for _, u := range strings.Split(raw, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				if !strings.HasSuffix(u, "/") {
					u += "/"
				}
				out = append(out, u)
			}
		}
	}
	return out
}

func validateSource(mirror string, urls []string) error {
	if _, err := ChooseMirrors(mirror, urls); err != nil {
		return err
	}
	return nil
}

// ImportLegacySource migrates the old per-root downloads\source.txt choice
// into the per-user settings. Existing settings always win. The file is left
// in place so an interrupted migration cannot destroy user data; subsequent
// runs use the saved setting and no longer consult it.
func ImportLegacySource(rootDir string) (SourceSettings, bool, error) {
	current := LoadSourceSettings()
	if current.Set || strings.TrimSpace(rootDir) == "" {
		return current, false, nil
	}
	p := filepath.Join(rootDir, "downloads", "source.txt")
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return current, false, nil
		}
		return current, false, errs.Wrap(errs.Internal, err, "读取旧的组件下载源失败")
	}
	choice := strings.TrimSpace(string(b))
	if strings.EqualFold(choice, "auto") {
		choice = ""
	}
	if _, err := SaveSourceSettings(choice, nil); err != nil {
		// Old, unpublished builds could write arbitrary text here. Treat it as
		// no saved choice instead of turning a fetch into a usage error.
		if errs.CodeOf(err) == errs.InvalidArgument {
			return current, false, nil
		}
		return current, false, err
	}
	return LoadSourceSettings(), true, nil
}
