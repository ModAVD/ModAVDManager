//go:build windows

package update

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/win"
)

// Sources of a ModAVD update.
const (
	SourceGitHub      = "github"
	SourceMirrorChyan = "mirrorchyan"
)

// Settings are the user's update choices, set in the manager (设置 → 更新) and used by `modavd update` (AUTO-MAS
// calls it): which source to download from, and the MirrorChyan CDK. One file per Windows user,
// %LOCALAPPDATA%\ModAVD\update-settings.json; the CDK is encrypted for that user with DPAPI, never written, logged or
// returned in clear.
type Settings struct {
	Source string // SourceGitHub or SourceMirrorChyan
	cdk    string
}

// CDK is the MirrorChyan CDK ("" when none).
func (s Settings) CDK() string { return s.cdk }

// HasCDK: a CDK is set.
func (s Settings) HasCDK() bool { return s.cdk != "" }

type settingsFile struct {
	Source string `json:"source"`
	CDK    string `json:"cdk,omitempty"` // base64 of the DPAPI blob
}

// cdkEntropy is DPAPI's extra secret for the CDK: another program of the same user cannot decrypt the blob with a
// plain CryptUnprotectData call without it.
var cdkEntropy = []byte("ModAVD update: MirrorChyan CDK")

// settingsPath is %LOCALAPPDATA%\ModAVD\update-settings.json; a variable for the tests.
var settingsPath = func() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
	}
	return filepath.Join(base, "ModAVD", "update-settings.json")
}

// LoadSettings reads the settings; missing or unreadable means GitHub without CDK. A CDK that does not decrypt
// (another user's file, a copied profile) counts as none.
func LoadSettings() Settings {
	s := Settings{Source: SourceGitHub}
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	var f settingsFile
	if json.Unmarshal(b, &f) != nil {
		return s
	}
	if f.Source == SourceMirrorChyan {
		s.Source = SourceMirrorChyan
	}
	if blob, err := base64.StdEncoding.DecodeString(f.CDK); err == nil && len(blob) > 0 {
		if plain, err := win.Unprotect(blob, cdkEntropy); err == nil {
			s.cdk = strings.TrimSpace(string(plain))
		}
	}
	return s
}

// SaveSettings changes the source (when not "") and the CDK (when cdk is not nil; "" clears it).
func SaveSettings(source string, cdk *string) (Settings, error) {
	s := LoadSettings()
	switch source {
	case "":
	case SourceGitHub, SourceMirrorChyan:
		s.Source = source
	default:
		return s, errs.Newf(errs.InvalidArgument, "更新源只能是 %s 或 %s，收到「%s」", SourceGitHub, SourceMirrorChyan, source)
	}
	if cdk != nil {
		v := strings.TrimSpace(*cdk)
		if len(v) > 256 || strings.ContainsAny(v, "\r\n\t &?#=") {
			return s, errs.New(errs.InvalidArgument, "CDK 的格式不对")
		}
		s.cdk = v
	}
	f := settingsFile{Source: s.Source}
	if s.cdk != "" {
		blob, err := win.Protect([]byte(s.cdk), cdkEntropy)
		if err != nil {
			return s, errs.Wrap(errs.Internal, err, "加密 CDK 失败: "+err.Error())
		}
		f.CDK = base64.StdEncoding.EncodeToString(blob)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return s, errs.Wrap(errs.Internal, err, "保存更新设置失败")
	}
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return s, errs.Wrap(errs.Internal, err, "保存更新设置失败: "+err.Error())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return s, errs.Wrap(errs.Internal, err, "保存更新设置失败: "+err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return s, errs.Wrap(errs.Internal, err, "保存更新设置失败: "+err.Error())
	}
	return s, nil
}
