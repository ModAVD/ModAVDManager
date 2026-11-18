package i18n

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// PrefAuto follows the system language.
const PrefAuto = "auto"

// EnvVar overrides the preference for one process (tests, support): MODAVD_LANG=en.
const EnvVar = "MODAVD_LANG"

type prefFile struct {
	Language string `json:"language"`
}

// prefPath is per user, next to update-settings.json (host.UserSettingsDir, repeated here: host imports win, which
// translates its dialogs); a variable for tests.
var prefPath = func() string {
	base := userSettingsDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "language.json")
}

// Preference is the saved choice: PrefAuto or a code of Languages. Missing or unreadable is PrefAuto.
func Preference() string {
	path := prefPath()
	if path == "" {
		return PrefAuto
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return PrefAuto
	}
	var f prefFile
	if json.Unmarshal(b, &f) != nil || !Supported(f.Language) {
		return PrefAuto
	}
	return f.Language
}

var saveMu sync.Mutex

// SavePreference stores pref (PrefAuto or a code of Languages) for this user.
func SavePreference(pref string) error {
	if pref != PrefAuto && !Supported(pref) {
		return os.ErrInvalid
	}
	path := prefPath()
	if path == "" {
		return os.ErrNotExist
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(prefFile{Language: pref})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func effectivePreference() string {
	if v := strings.TrimSpace(os.Getenv(EnvVar)); v != "" {
		return v
	}
	return Preference()
}

// SystemLocale is the user's interface language as a locale name ("zh-CN", "en-US"); "" when unknown.
func SystemLocale() string { return systemLocale() }

// Writer translates whatever is written through it into lang (the CLI's human output). Every Write is translated as a
// whole, so a caller should write whole messages (fmt.Fprintf does). Chinese is w itself.
func Writer(w io.Writer, lang string) io.Writer {
	if lang == Chinese || w == nil {
		return w
	}
	return &writer{w: w, lang: lang}
}

type writer struct {
	w    io.Writer
	lang string
}

func (t *writer) Write(p []byte) (int, error) {
	s := string(p)
	if !hasNonASCII(p) {
		return t.w.Write(p)
	}
	out := Translate(t.lang, s)
	if !HasHan(out) {
		// A line laid out from translated parts still has the Chinese punctuation that joined them.
		out = fullWidth.Replace(out)
	}
	if _, err := io.WriteString(t.w, out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func userSettingsDir() string {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
	} else {
		base, _ = os.UserConfigDir()
		if base == "" {
			return ""
		}
	}
	return filepath.Join(base, "ModAVD")
}

// LangOf is the language a Writer translates into; Chinese for any other writer. For output laid out before it is
// written (table columns are padded to the translated width).
func LangOf(w io.Writer) string {
	if t, ok := w.(*writer); ok {
		return t.lang
	}
	return Chinese
}
