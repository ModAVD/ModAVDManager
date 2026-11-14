// Copyright 2026 jinghero. All rights reserved.

package emu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/guest"
	"modavd/internal/jsonord"
	"modavd/internal/regional"
	"modavd/internal/root"
)

// RegionalConfig contains choices and the last language request, never live guest observations.
type RegionalConfig struct {
	Language        string `json:"language"`
	TimeZone        string `json:"time_zone"`
	PendingLanguage string `json:"pending_language,omitempty"`
	LastLanguage    string `json:"last_language,omitempty"`
	Error           string `json:"error,omitempty"`
}

type RegionalGuest struct {
	Language string `json:"language"`
	TimeZone string `json:"time_zone"`
}

type RegionalResult struct {
	Index        int            `json:"index"`
	Supported    bool           `json:"supported"`
	Config       RegionalConfig `json:"config"`
	HostLanguage string         `json:"host_language,omitempty"`
	HostTimeZone string         `json:"host_time_zone,omitempty"`
	HostErrors   []string       `json:"host_errors,omitempty"`
	Guest        *RegionalGuest `json:"guest"` // null when offline or unreachable
	GuestError   string         `json:"guest_error,omitempty"`
}

func regionalConfig(meta *jsonord.Obj) *RegionalConfig {
	if !root.CurrentPlatform().Windows() {
		return nil
	}
	cfg := &RegionalConfig{Language: regional.Preserve, TimeZone: regional.Preserve}
	s, err := regional.Read(meta)
	if err != nil {
		cfg.Error = err.Error()
		return cfg
	}
	if s != nil {
		cfg.Language, cfg.TimeZone, cfg.LastLanguage = s.Language, s.TimeZone, s.LanguageApplied
		if s.LocaleRequest != nil {
			cfg.PendingLanguage = s.LocaleRequest.Locale
		}
	}
	return cfg
}

func (m *Manager) readRegional(i int) (*regional.Settings, error) {
	meta, err := m.Root.ReadMetadata()
	if err != nil {
		return nil, err
	}
	s, err := regional.Read(meta.Obj().Get("instances").Obj().Get(strconv.Itoa(i)).Obj())
	if err != nil {
		return nil, errs.Wrap(errs.MetadataUnreadable, err, fmt.Sprintf("实例 %d 的语言/时区设置无效: %v", i, err))
	}
	return s, nil
}

// regionalChoices parses external choices before any instance files are changed.
func (m *Manager) regionalChoices(language, zone *string) (l, z *string, request *regional.LocaleRequest, err error) {
	if language == nil && zone == nil {
		return
	}
	if err = requireWindows(); err != nil {
		return nil, nil, nil, errs.New(errs.Unsupported, "实例语言/时区设置只支持 Windows")
	}
	if zone != nil {
		var value string
		value, err = regional.TimeZone(*zone)
		if err != nil {
			return
		}
		z = &value
	}
	if language != nil {
		var value string
		value, err = regional.Language(*language)
		if err != nil {
			return
		}
		l = &value
		if value != regional.Preserve {
			target := value
			if target == regional.Host {
				target, err = m.hostLanguage()
				if err != nil {
					err = errs.Wrap(errs.NotReady, err, "读不出 Windows 界面语言: "+err.Error())
					return
				}
				target, err = regional.Language(target)
				if err != nil || target == regional.Host || target == regional.Preserve {
					err = errs.New(errs.NotReady, "Windows 返回的界面语言无效")
					return
				}
			}
			request, err = regional.NewLocaleRequest(target)
		}
	}
	return
}

func (m *Manager) newRegional(language, zone *string) (*regional.Settings, error) {
	if !root.CurrentPlatform().Windows() && language == nil && zone == nil {
		return nil, nil
	}
	host := regional.Host
	if language == nil {
		language = &host
	}
	if zone == nil {
		zone = &host
	}
	l, z, request, err := m.regionalChoices(language, zone)
	if err != nil {
		return nil, err
	}
	return &regional.Settings{Language: *l, TimeZone: *z, LocaleRequest: request}, nil
}

func (m *Manager) saveRegional(i int, language, zone *string, request *regional.LocaleRequest) error {
	l, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return err
	}
	defer l.Unlock()
	if _, err := m.instance(i); err != nil {
		return err
	}
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		s, err := regional.Read(meta)
		if err != nil {
			return err
		}
		if s == nil {
			s = &regional.Settings{Language: regional.Preserve, TimeZone: regional.Preserve}
		}
		if language != nil {
			s.Language, s.LocaleRequest = *language, request
		}
		if zone != nil {
			s.TimeZone = *zone
		}
		s.Set(meta)
		return nil
	})
}

func (m *Manager) localeBootPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".locale")
}

// regionalLaunch runs for every cold launch. The receipt freezes which language request belongs to that boot,
// so a settings edit while Android is running waits for the next cold launch, even if start adopts that process.
func (m *Manager) regionalLaunch(i int) ([]string, error) {
	if !root.CurrentPlatform().Windows() {
		return nil, nil
	}
	s, err := m.readRegional(i)
	if err != nil {
		return nil, err
	}
	if s != nil && s.LocaleRequest != nil {
		if err := writeFileReplace(m.localeBootPath(i), []byte(s.LocaleRequest.ID), strconv.Itoa(os.Getpid())); err != nil {
			return nil, err
		}
	} else {
		os.Remove(m.localeBootPath(i)) // stale receipts have no effect without a pending request
	}
	if s == nil || s.TimeZone == regional.Preserve {
		return nil, nil
	}
	zone := s.TimeZone
	if zone == regional.Host {
		zone, err = m.hostTimeZone()
		if err == nil {
			zone, err = regional.TimeZone(zone)
		}
		if err == nil && (zone == regional.Host || zone == regional.Preserve) {
			err = fmt.Errorf("宿主返回了无效时区 %q", zone)
		}
		if err != nil {
			m.Log.Warnf("实例 %d 读不出宿主 IANA 时区，沿用模拟器原有检测: %v", i, err)
			return nil, nil
		}
	}
	return []string{"-timezone", zone}, nil
}

func (m *Manager) regionalCommand(ctx context.Context, i int, request *regional.LocaleRequest) (map[string]string, error) {
	local, err := guest.LocalRegional(filepath.Join(os.TempDir(), "modavd-guest"))
	if err != nil {
		return nil, err
	}
	const remote = "/data/local/tmp/modavd-regional.dex"
	if !m.push(ctx, i, local, remote) {
		return nil, fmt.Errorf("推送语言助手失败")
	}
	command := "env CLASSPATH=" + remote + " app_process / org.modavd.regional.Regional"
	if request != nil {
		command += " " + request.ID + " " + request.Locale
	}
	result := m.su(ctx, i, command, 0)
	if result.Code != 0 {
		return nil, fmt.Errorf("客体语言接口失败: %s", tail(result.Out, 400))
	}
	values := map[string]string{}
	for _, line := range outputLines(result.Out) {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	if values["language"] == "" || values["time_zone"] == "" {
		return nil, fmt.Errorf("客体语言/时区读回不完整")
	}
	return values, nil
}

func (m *Manager) applyRegional(ctx context.Context, i int, mode bootSetup) error {
	if !root.CurrentPlatform().Windows() || mode == setupAdopted {
		return nil
	}
	s, err := m.readRegional(i)
	if err != nil || s == nil || s.LocaleRequest == nil {
		return err
	}
	request := s.LocaleRequest
	bootID, err := os.ReadFile(m.localeBootPath(i))
	if err != nil {
		return err
	}
	if string(bootID) != request.ID {
		return nil
	} // a later explicit edit is for the next cold launch
	values, err := m.regionalCommand(ctx, i, request)
	if err != nil {
		return err
	}
	if values["applied"] != request.ID {
		return fmt.Errorf("客体没有确认语言请求")
	}
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		current, err := regional.Read(meta)
		if err != nil {
			return err
		}
		if current != nil && current.LocaleRequest != nil && current.LocaleRequest.ID == request.ID {
			current.LocaleRequest = nil
			current.LanguageApplied = request.Locale
			current.Set(meta)
		}
		return nil
	})
}

// Regional reads live values only on demand, not on the manager's instance poll.
func (m *Manager) Regional(ctx context.Context, i int) (*RegionalResult, error) {
	in, err := m.Info(ctx, i)
	if err != nil {
		return nil, err
	}
	result := &RegionalResult{Index: i, Supported: root.CurrentPlatform().Windows()}
	if !result.Supported {
		return result, nil
	}
	result.Config = *in.Regional
	if result.HostLanguage, err = m.hostLanguage(); err != nil {
		result.HostErrors = append(result.HostErrors, err.Error())
	}
	if result.HostTimeZone, err = m.hostTimeZone(); err != nil {
		result.HostErrors = append(result.HostErrors, err.Error())
	}
	if in.Booted && in.Busy == "" {
		values, err := m.regionalCommand(ctx, i, nil)
		if err != nil {
			result.GuestError = err.Error()
		} else {
			result.Guest = &RegionalGuest{Language: values["language"], TimeZone: values["time_zone"]}
		}
	}
	return result, nil
}
