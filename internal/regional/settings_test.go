// Copyright 2026 jinghero. All rights reserved.

package regional

import (
	"testing"

	"modavd/internal/jsonord"
)

func TestWindowsZones(t *testing.T) {
	for key, want := range map[string]string{
		"China Standard Time": "Asia/Shanghai", "Pacific Standard Time": "America/Los_Angeles",
		"GMT Standard Time": "Europe/London", "UTC": "Etc/UTC", "India Standard Time": "Asia/Calcutta",
	} {
		got, err := WindowsZone(key)
		if err != nil || got != want {
			t.Fatalf("%q: %q, %v", key, got, err)
		}
	}
	for _, key := range []string{"", "中国标准时间", "China Standard Tim", "Unknown/Unknown"} {
		if _, err := WindowsZone(key); err == nil {
			t.Errorf("accepted unknown Windows identifier %q", key)
		}
	}
	for key, zone := range windowsZones {
		if got, err := TimeZone(zone); err != nil || got != zone {
			t.Errorf("CLDR %s: %s, %v", key, zone, err)
		}
	}
}

func TestRegionalChoices(t *testing.T) {
	for input, want := range map[string]string{"host": "host", "preserve": "preserve", "zh-cn": "zh-CN", "zh-hans-cn": "zh-Hans-CN", "EN-us": "en-US", "en": "en", "es-419": "es-419", "ca-ES-valencia": "ca-ES-valencia"} {
		if got, err := Language(input); err != nil || got != want {
			t.Errorf("Language(%q)=%q,%v", input, got, err)
		}
	}
	for _, input := range []string{"", "zh_CN", "zh-CN;id", "en-US\nmalicious", "en-x-foo", "123", "en-abc-XY", "en-123-456", "en-US-posix-posix", "en-419-Latn"} {
		if _, err := Language(input); err == nil {
			t.Errorf("invalid language accepted: %q", input)
		}
	}
	for input, want := range map[string]string{"UTC": "Etc/UTC", "Etc/UTC": "Etc/UTC", "Asia/Shanghai": "Asia/Shanghai", "Etc/GMT+8": "Etc/GMT+8", "host": "host", "preserve": "preserve"} {
		if got, err := TimeZone(input); err != nil || got != want {
			t.Errorf("TimeZone(%q)=%q,%v", input, got, err)
		}
	}
	for _, input := range []string{"", "GMT", "Local", "Unknown/Unknown", "../Etc/UTC", "Etc/UTC;id", "Asia/ Shanghai"} {
		if _, err := TimeZone(input); err == nil {
			t.Errorf("invalid time zone accepted: %q", input)
		}
	}
}

func TestSettingsCompatibility(t *testing.T) {
	legacy, _ := jsonord.Parse([]byte(`{"initialized":false}`))
	if got, err := Read(legacy.Obj()); err != nil || got != nil {
		t.Fatalf("legacy: %+v %v", got, err)
	}
	request, err := NewLocaleRequest("zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{Language: Host, TimeZone: Host, LocaleRequest: request}
	s.Set(legacy.Obj())
	legacy.Obj().Get(Key).Obj().Set("future", jsonord.NewString("keep"))
	s.Set(legacy.Obj())
	if value, _ := legacy.Obj().Get(Key).Obj().Get("future").Str(); value != "keep" {
		t.Fatal("unknown regional metadata was lost")
	}
	if got, err := Read(legacy.Obj()); err != nil || got.LocaleRequest.ID != request.ID {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	for _, raw := range []string{`null`, `"host"`, `{}`, `{"language":"preserve","timeZone":"../UTC"}`, `{"language":"host","timeZone":"host","localeRequest":{"id":"1","locale":"en-US"}}`, `{"language":"preserve","timeZone":"host","localeRequest":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","locale":"en-US"}}`} {
		v, err := jsonord.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		legacy.Obj().Set(Key, v)
		if _, err := Read(legacy.Obj()); err == nil {
			t.Errorf("invalid section accepted: %s", raw)
		}
	}
}
