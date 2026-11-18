// Package i18n translates what ModAVD shows to people (ModAVD/ModAVD#165).
//
// The source text stays Chinese everywhere in the code, and it is also the key: locales/<lang>/*.json map a Chinese
// string to its translation. Go format strings are keys as written ("实例 %d 正在关机"), and a message built from
// them is matched back against those patterns, the arguments translated in turn; the extractor (catalog_test.go,
// -update) also turns a concatenation "实例 " + name + " 已存在" into the key "实例 %v 已存在". The page's own
// strings use {name} placeholders and are looked up as written (web.json).
//
// Translation happens only where text is shown: the manager's HTTP answers and events, the CLI's human output, the
// GUI's dialogs and tray. What machines read (--json, MCP, exit codes, error codes, mas-avd.json) and what is written
// to logs and problem reports stays Chinese, byte for byte as before.
//
// A key whose value is null is shown untranslated on purpose (text that only goes into logs or reports); "" means
// not translated yet, which TestCatalogComplete reports.
package i18n

import (
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Languages. Chinese is the source language and needs no table.
const (
	Chinese = "zh-CN"
	English = "en"
)

// Language is one choice in the settings.
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"` // in that language
}

// Languages lists the languages the settings offer, the source language first.
func Languages() []Language {
	out := []Language{{Chinese, "中文"}}
	for _, code := range tableCodes() {
		out = append(out, Language{code, nameOf(code)})
	}
	return out
}

func nameOf(code string) string {
	if n, ok := names[code]; ok {
		return n
	}
	return code
}

var names = map[string]string{English: "English"}

//go:embed locales
var localeFS embed.FS

var current atomic.Value // string

// Current is the language this process shows text in. Chinese until Init or Set: tests and library use see the source
// text.
func Current() string {
	if v, ok := current.Load().(string); ok && v != "" {
		return v
	}
	return Chinese
}

// Set makes lang (a supported code) the language of this process; anything else falls back to the system's.
func Set(lang string) {
	current.Store(Resolve(lang))
}

// Init picks the language from MODAVD_LANG, the saved preference, then the system. The CLI and the manager call it
// once at start.
func Init() {
	if testing.Testing() && os.Getenv(EnvVar) == "" {
		// Tests of the CLI and the manager see the source text, whatever this computer's language and settings, and
		// write no preference.
		current.Store(Chinese)
		return
	}
	current.Store(Resolve(effectivePreference()))
}

// Resolve maps a preference ("", "auto", a code, a locale such as "en-US" or "zh-TW") to a supported code.
func Resolve(pref string) string {
	pref = strings.TrimSpace(pref)
	if pref == "" || strings.EqualFold(pref, PrefAuto) {
		return Match(SystemLocale())
	}
	return Match(pref)
}

// Match maps a locale name to a supported code: zh-* is Chinese, a language with a table is that language, anything
// else English.
func Match(locale string) string {
	l := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"))
	if l == "zh" || strings.HasPrefix(l, "zh-") {
		return Chinese
	}
	for _, code := range tableCodes() {
		c := strings.ToLower(code)
		if l == c || strings.HasPrefix(l, c+"-") {
			return code
		}
	}
	return English
}

// Supported reports whether code is one of Languages.
func Supported(code string) bool {
	for _, l := range Languages() {
		if l.Code == code {
			return true
		}
	}
	return false
}

// T translates s into the current language.
func T(s string) string { return Translate(Current(), s) }

// Translate translates s into lang. Text without Chinese, and text the table does not know, come back unchanged.
func Translate(lang, s string) string {
	if lang == Chinese || s == "" || !HasHan(s) {
		return s
	}
	c := catalogFor(lang)
	if c == nil {
		return s
	}
	out := c.translate(s, 0)
	out = capitalize(s, out)
	if out != s && !HasHan(out) {
		// Glue without Chinese words ("%s%s（%s）") is no key: its full-width punctuation is changed here.
		out = fullWidth.Replace(out)
	}
	return out
}

var fullWidth = strings.NewReplacer("（", " (", "）", ")", "：", ": ", "，", ", ", "；", "; ", "。", ". ", "、", ", ",
	"「", "\"", "」", "\"", "！", "!", "？", "?")

// Messages is the table of lang for the page (web.json): the page's own strings with their {name} placeholders.
// Empty for Chinese.
func Messages(lang string) map[string]string {
	out := map[string]string{}
	if lang == Chinese {
		return out
	}
	b, err := localeFS.ReadFile(path.Join("locales", lang, "web.json"))
	if err != nil {
		return out
	}
	var raw map[string]*string
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for k, v := range raw {
		if v != nil && *v != "" {
			out[k] = *v
		}
	}
	return out
}

// HasHan reports whether s contains a Chinese character.
func HasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

var (
	codesOnce sync.Once
	codes     []string
)

// tableCodes are the languages with a folder under locales, sorted.
func tableCodes() []string {
	codesOnce.Do(func() {
		entries, _ := fs.ReadDir(localeFS, "locales")
		for _, e := range entries {
			if e.IsDir() {
				codes = append(codes, e.Name())
			}
		}
		sort.Strings(codes)
	})
	return codes
}

var (
	catalogsMu sync.Mutex
	catalogs   = map[string]*catalog{}
)

func catalogFor(lang string) *catalog {
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	if c, ok := catalogs[lang]; ok {
		return c
	}
	entries, err := loadTable(lang)
	var c *catalog
	if err == nil && len(entries) > 0 {
		c = newCatalog(entries)
	}
	catalogs[lang] = c
	return c
}

// loadTable reads every locales/<lang>/*.json; null and "" values are left out.
func loadTable(lang string) (map[string]string, error) {
	dir := path.Join("locales", lang)
	entries, err := fs.ReadDir(localeFS, dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		// web.json is the page's (Messages): its labels may say a word differently from the messages of the back end.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "web.json" {
			continue
		}
		b, err := localeFS.ReadFile(path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var raw map[string]*string
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, err
		}
		for k, v := range raw {
			if v != nil && *v != "" {
				out[k] = *v
			}
		}
	}
	return out, nil
}

// capitalize starts a translation with a capital letter when the source starts with a Chinese word: tables write
// words used inside other messages in lower case ("关机" → "shutting down"), which then also serve on their own.
func capitalize(src, out string) string {
	s := strings.TrimLeft(src, " \t\n")
	r, _ := utf8.DecodeRuneInString(s)
	if !unicode.Is(unicode.Han, r) {
		return out
	}
	lead := len(out) - len(strings.TrimLeft(out, " \t\n"))
	o, n := utf8.DecodeRuneInString(out[lead:])
	if !unicode.IsLower(o) {
		return out
	}
	return out[:lead] + string(unicode.ToUpper(o)) + out[lead+n:]
}
