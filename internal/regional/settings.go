// Copyright 2026 jinghero. All rights reserved.

// Package regional owns Windows host defaults and the saved regional choices of an instance.
package regional

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // Windows installations need not have a zoneinfo database.

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

const (
	Host     = "host"
	Preserve = "preserve"
	Key      = "regional"
)

// A language choice creates one request. Finishing unrelated first-boot steps never creates another one.
// LanguageApplied is the last requested value, not an observation of the current Android language.
type Settings struct {
	Language        string         `json:"language"`
	TimeZone        string         `json:"timeZone"`
	LocaleRequest   *LocaleRequest `json:"localeRequest,omitempty"`
	LanguageApplied string         `json:"languageApplied,omitempty"`
}

type LocaleRequest struct {
	ID     string `json:"id"`
	Locale string `json:"locale"`
}

var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
var zoneName = regexp.MustCompile(`^[A-Za-z0-9_+.-]+(?:/[A-Za-z0-9_+.-]+)+$`)
var requestID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Language accepts a BCP 47 language with optional script, region and variants, or a manager choice.
// Extensions and private-use tags are not system display-language choices.
func Language(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == Host || value == Preserve {
		return value, nil
	}
	if len(value) > 63 || !languageTag.MatchString(value) {
		return "", errs.New(errs.InvalidArgument, "语言要填 host、preserve 或语言标识，例如 zh-CN、en-US")
	}
	parts := strings.Split(value, "-")
	parts[0] = strings.ToLower(parts[0])
	i := 1
	if i < len(parts) && len(parts[i]) == 4 && letters(strings.ToLower(parts[i])) {
		p := strings.ToLower(parts[i])
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
		i++
	}
	if i < len(parts) && (len(parts[i]) == 2 && letters(strings.ToLower(parts[i])) || len(parts[i]) == 3 && strings.Trim(parts[i], "0123456789") == "") {
		parts[i] = strings.ToUpper(parts[i])
		i++
	}
	variants := map[string]bool{}
	for ; i < len(parts); i++ {
		p := strings.ToLower(parts[i])
		if variants[p] || !(len(p) >= 5 && len(p) <= 8 || len(p) == 4 && p[0] >= '0' && p[0] <= '9') {
			return "", errs.New(errs.InvalidArgument, "语言标识无效，例如 zh-CN、en-US")
		}
		variants[p] = true
		parts[i] = p
	}
	return strings.Join(parts, "-"), nil
}

func letters(s string) bool {
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// TimeZone only accepts named IANA zones. The emulator requires a slash, including for UTC.
func TimeZone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == Host || value == Preserve {
		return value, nil
	}
	if value == "UTC" {
		value = "Etc/UTC"
	}
	if len(value) > 128 || !zoneName.MatchString(value) || strings.Contains(value, "..") {
		return "", errs.New(errs.InvalidArgument, "时区要填 host、preserve 或 IANA 时区，例如 Asia/Shanghai、Etc/UTC")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", errs.Newf(errs.InvalidArgument, "未知的 IANA 时区 %s", value)
	}
	return value, nil
}

func NewLocaleRequest(locale string) (*LocaleRequest, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return &LocaleRequest{ID: hex.EncodeToString(id[:]), Locale: locale}, nil
}

// Read treats a missing section as the legacy behavior. Malformed new settings are never silently applied.
func Read(meta *jsonord.Obj) (*Settings, error) {
	if !meta.Has(Key) {
		return nil, nil
	}
	v := meta.Get(Key)
	if v.Kind() != jsonord.Object {
		return nil, fmt.Errorf("regional 必须是对象")
	}
	var s Settings
	if err := json.Unmarshal(v.MarshalPython(), &s); err != nil {
		return nil, err
	}
	language, err := Language(s.Language)
	if err != nil || language != s.Language {
		return nil, fmt.Errorf("regional.language 无效")
	}
	zone, err := TimeZone(s.TimeZone)
	if err != nil || zone != s.TimeZone {
		return nil, fmt.Errorf("regional.timeZone 无效")
	}
	if req := s.LocaleRequest; req != nil {
		locale, err := Language(req.Locale)
		if !requestID.MatchString(req.ID) || err != nil || locale != req.Locale || locale == Host || locale == Preserve || s.Language == Preserve {
			return nil, fmt.Errorf("regional.localeRequest 无效")
		}
	}
	return &s, nil
}

// Set changes only the fields this version owns, preserving other metadata and future regional fields.
func (s *Settings) Set(meta *jsonord.Obj) {
	v := meta.Get(Key)
	if v.Kind() != jsonord.Object {
		v = jsonord.NewObject()
	}
	o := v.Obj()
	o.Set("language", jsonord.NewString(s.Language))
	o.Set("timeZone", jsonord.NewString(s.TimeZone))
	if s.LocaleRequest == nil {
		o.Delete("localeRequest")
	} else {
		request := jsonord.NewObject()
		request.Obj().Set("id", jsonord.NewString(s.LocaleRequest.ID))
		request.Obj().Set("locale", jsonord.NewString(s.LocaleRequest.Locale))
		o.Set("localeRequest", request)
	}
	if s.LanguageApplied == "" {
		o.Delete("languageApplied")
	} else {
		o.Set("languageApplied", jsonord.NewString(s.LanguageApplied))
	}
	meta.Set(Key, v)
}

// WindowsZone uses CLDR's world default, territory 001, for a stable Windows registry identifier.
func WindowsZone(key string) (string, error) {
	if zone, ok := windowsZones[key]; ok {
		return zone, nil
	}
	return "", fmt.Errorf("Windows 时区 %q 没有 CLDR 对照", key)
}
