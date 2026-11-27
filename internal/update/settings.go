package update

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

// Sources of a ModAVD update.
const (
	SourceGitHub      = "github"
	SourceMirrorChyan = "mirrorchyan"
)

// Settings are the user's update choices, set in the manager (设置 → 更新) and used by `modavd update` (AUTO-MAS
// calls it): which source to download from, and the MirrorChyan CDK. Settings
// use the per-user ModAVD configuration directory. The CDK is encrypted with
// Windows DPAPI or a macOS Keychain wrapping key, never written or logged clear.
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
	CDK    string `json:"cdk,omitempty"` // base64 of the platform-protected blob
}

// cdkEntropy binds the protected blob to this use. It is DPAPI's optional
// entropy on Windows and the Keychain key purpose / authenticated data on macOS.
var cdkEntropy = []byte("ModAVD update: MirrorChyan CDK")

func settingsSupported() bool {
	p := root.CurrentPlatform()
	return p.Windows() || nativeDarwinSettings && p.Supported()
}

// settingsPath is per user, independent of the installation; a variable for tests.
var settingsPath = func() string {
	if !settingsSupported() {
		return ""
	}
	base := host.UserSettingsDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "update-settings.json")
}

// LoadSettings reads the settings; missing or unreadable means GitHub without CDK. A CDK that does not decrypt
// (another user's file, a copied profile) counts as none.
func LoadSettings() Settings {
	s := Settings{Source: SourceGitHub}
	path := settingsPath()
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
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
		if plain, err := host.Unprotect(blob, cdkEntropy); err == nil {
			s.cdk = strings.TrimSpace(string(plain))
		}
	}
	return s
}

// checkCDK is cdk trimmed, or invalid_argument when it cannot be a MirrorChyan CDK. The error never quotes it.
func checkCDK(cdk string) (string, error) {
	v := strings.TrimSpace(cdk)
	if len(v) > 256 || strings.ContainsAny(v, "\r\n\t &?#=") {
		return "", errs.New(errs.InvalidArgument, "CDK 的格式不对")
	}
	return v, nil
}

// CheckSource is source when it is SourceGitHub or SourceMirrorChyan, else invalid_argument.
func CheckSource(source string) (string, error) {
	switch source {
	case SourceGitHub, SourceMirrorChyan:
		return source, nil
	}
	return "", errs.Newf(errs.InvalidArgument, "更新源只能是 %s 或 %s，收到「%s」", SourceGitHub, SourceMirrorChyan, source)
}

// CallerCDK is the MirrorChyan CDK a caller passed for one run (`modavd update --cdk-dpapi`): b64 is the base64 of
// a Windows DPAPI blob of the UTF-8 CDK, protected by the caller without entropy (CryptProtectData; user or machine
// scope, the blob says which). "" (or only blanks) is no CDK. A blob that is not base64, does not decrypt or is not a CDK is
// invalid_argument; other platforms are unsupported. Neither the blob nor the CDK ever goes into an error.
func CallerCDK(b64 string) (string, error) {
	if !root.CurrentPlatform().Windows() {
		return "", errs.New(errs.Unsupported, "--cdk-dpapi 只在 Windows 上可用")
	}
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return "", nil
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(blob) == 0 {
		return "", errs.New(errs.InvalidArgument, "--cdk-dpapi 不是 base64")
	}
	plain, err := host.Unprotect(blob, nil)
	if err != nil {
		return "", errs.New(errs.InvalidArgument, "--cdk-dpapi 解不开（要用当前用户或本机的 DPAPI 加密，不带 entropy）")
	}
	v, err := checkCDK(string(plain))
	clear(plain)
	if err != nil {
		return "", errs.New(errs.InvalidArgument, "--cdk-dpapi 解出来的 CDK 格式不对")
	}
	return v, nil
}

// SaveSettings changes the source (when not "") and the CDK (when cdk is not nil; "" clears it).
func SaveSettings(source string, cdk *string) (Settings, error) {
	s := LoadSettings()
	if !settingsSupported() {
		return s, errs.New(errs.Unsupported, "当前平台暂不支持保存更新设置")
	}
	switch source {
	case "":
	case SourceGitHub, SourceMirrorChyan:
		s.Source = source
	default:
		return s, errs.Newf(errs.InvalidArgument, "更新源只能是 %s 或 %s，收到「%s」", SourceGitHub, SourceMirrorChyan, source)
	}
	if cdk != nil {
		v, err := checkCDK(*cdk)
		if err != nil {
			return s, err
		}
		s.cdk = v
	}
	f := settingsFile{Source: s.Source}
	if s.cdk != "" {
		blob, err := host.Protect([]byte(s.cdk), cdkEntropy)
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
	if path == "" {
		return s, errs.New(errs.Internal, "找不到当前用户的配置目录，无法保存更新设置")
	}
	dirMode := os.FileMode(0o755)
	if runtime.GOOS == "darwin" {
		dirMode = 0o700
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
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
