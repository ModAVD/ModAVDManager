//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/regional"
	"modavd/internal/win"
)

func textPtr(s string) *string { return &s }

func TestRegionalNewAndLegacy(t *testing.T) {
	m, _ := opsTestManager(t)
	m.hostLanguage = func() (string, error) { return "zh-CN", nil }
	m.hostTimeZone = func() (string, error) { return "Asia/Shanghai", nil }
	ctx := context.Background()
	in, err := m.Create(ctx, CreateRequest{filesOnly: true, NoBridge: true})
	if err != nil {
		t.Fatal(err)
	}
	i := in.Index
	s, err := m.readRegional(i)
	if err != nil || s.Language != regional.Host || s.TimeZone != regional.Host || s.LocaleRequest.Locale != "zh-CN" {
		t.Fatalf("new settings: %+v, %v", s, err)
	}
	id := s.LocaleRequest.ID
	if got, err := m.regionalLaunch(i); err != nil || !reflect.DeepEqual(got, []string{"-timezone", "Asia/Shanghai"}) {
		t.Fatalf("launch: %v %v", got, err)
	}
	if got, _ := os.ReadFile(m.localeBootPath(i)); string(got) != id {
		t.Fatalf("boot request %q", got)
	}
	m.hostTimeZone = func() (string, error) { return "America/New_York", nil }
	if got, err := m.regionalLaunch(i); err != nil || got[1] != "America/New_York" {
		t.Fatalf("changed host: %v %v", got, err)
	}
	if _, err := m.Update(ctx, i, UpdateRequest{TimeZone: textPtr("Etc/UTC")}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.regionalLaunch(i); err != nil || got[1] != "Etc/UTC" {
		t.Fatalf("fixed: %v %v", got, err)
	}
	if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Delete(regional.Key)
		meta.Set("initialized", jsonord.NewBool(false))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.regionalLaunch(i); err != nil || len(got) != 0 {
		t.Fatalf("legacy uninitialized: %v %v", got, err)
	}
	if got, err := m.readRegional(i); err != nil || got != nil {
		t.Fatalf("legacy was migrated: %+v %v", got, err)
	}
	if _, err := m.Update(ctx, i, UpdateRequest{TimeZone: textPtr("host")}); err != nil {
		t.Fatal(err)
	}
	s, _ = m.readRegional(i)
	if s.Language != regional.Preserve || s.LocaleRequest != nil || s.TimeZone != regional.Host {
		t.Fatalf("explicit legacy choice: %+v", s)
	}
}

func TestRegionalAPIFailuresAndInvalidRequests(t *testing.T) {
	m, logs := opsTestManager(t)
	m.hostLanguage = func() (string, error) { return "", errors.New("language API failed") }
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true}); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("language failure: %v", err)
	}
	if list, _ := avd.List(m.Root); len(list) != 0 {
		t.Fatal("language API failure left files")
	}
	m.hostLanguage = func() (string, error) { return "preserve", nil }
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true}); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("invalid host value: %v", err)
	}
	in, err := m.Create(context.Background(), CreateRequest{filesOnly: true, NoBridge: true, Language: textPtr("preserve")})
	if err != nil {
		t.Fatal(err)
	}
	m.hostTimeZone = func() (string, error) { return "", errors.New("unknown Windows time zone") }
	if args, err := m.regionalLaunch(in.Index); err != nil || len(args) != 0 {
		t.Fatalf("timezone failure: %v %v", args, err)
	}
	if len(logs.Messages()) == 0 {
		t.Fatal("fallback was not reported")
	}
	before, _ := os.ReadFile(m.Root.MetadataPath())
	if _, err := m.Update(context.Background(), in.Index, UpdateRequest{Title: textPtr("changed"), TimeZone: textPtr("Unknown/Unknown")}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("bad timezone: %v", err)
	}
	after, _ := os.ReadFile(m.Root.MetadataPath())
	if string(before) != string(after) {
		t.Fatal("invalid regional request partially changed metadata")
	}
	current, _ := m.instance(in.Index)
	if current.Title == "changed" {
		t.Fatal("invalid request changed config.ini")
	}
}

func TestRegionalRequestSurvivesCreateRetryAndEdits(t *testing.T) {
	m, _ := opsTestManager(t)
	m.hostLanguage = func() (string, error) { return "en-US", nil }
	h := &createHooks{check: errs.New(errs.NotReady, "later setup failed")}
	h.install(t)
	i := 1
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true}); err == nil {
		t.Fatal("expected create failure")
	}
	s, _ := m.readRegional(i)
	id := s.LocaleRequest.ID
	m.hostLanguage = func() (string, error) { return "zh-CN", nil }
	h.check = nil
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, Resume: true}); err != nil {
		t.Fatal(err)
	}
	s, _ = m.readRegional(i)
	if s.LocaleRequest.ID != id || s.LocaleRequest.Locale != "en-US" {
		t.Fatalf("resume resampled language: %+v", s)
	}
	if _, err := m.Update(context.Background(), i, UpdateRequest{Language: textPtr("host")}); err != nil {
		t.Fatal(err)
	}
	s, _ = m.readRegional(i)
	if s.LocaleRequest.ID == id || s.LocaleRequest.Locale != "zh-CN" {
		t.Fatalf("explicit host edit: %+v", s)
	}
	if _, err := m.Update(context.Background(), i, UpdateRequest{Language: textPtr("preserve")}); err != nil {
		t.Fatal(err)
	}
	s, _ = m.readRegional(i)
	if s.LocaleRequest != nil {
		t.Fatal("preserve did not cancel pending request")
	}
}

func TestRegionalCancelledAndDeferredApplication(t *testing.T) {
	m, _ := opsTestManager(t)
	m.hostLanguage = func() (string, error) { return "en-US", nil }
	in, err := m.Create(context.Background(), CreateRequest{filesOnly: true, NoBridge: true})
	if err != nil {
		t.Fatal(err)
	}
	i := in.Index
	initial, _ := m.readRegional(i)
	// A missing boot receipt cannot turn an ordinary adoption into a language edit.
	if err := m.applyRegional(context.Background(), i, setupAdopted); err != nil {
		t.Fatal(err)
	}
	if err := m.applyRegional(context.Background(), i, setupOwed); err == nil {
		t.Fatal("missing receipt was accepted")
	}
	if _, err := m.regionalLaunch(i); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.applyRegional(ctx, i, setupLaunched); err == nil {
		t.Fatal("canceled guest operation was acknowledged")
	}
	still, _ := m.readRegional(i)
	if still.LocaleRequest == nil || still.LocaleRequest.ID != initial.LocaleRequest.ID {
		t.Fatal("cancellation lost pending language")
	}
	// Save another language after the boot's receipt was frozen. Even owed setup leaves it for the next launch.
	if _, err := m.Update(context.Background(), i, UpdateRequest{Language: textPtr("ja-JP")}); err != nil {
		t.Fatal(err)
	}
	if err := m.applyRegional(context.Background(), i, setupOwed); err != nil {
		t.Fatal(err)
	}
	still, _ = m.readRegional(i)
	if still.LocaleRequest == nil || still.LocaleRequest.Locale != "ja-JP" {
		t.Fatal("deferred language was consumed")
	}
}

func TestRegionalFailedLanguageDoesNotBlockCreation(t *testing.T) {
	r, _ := portPlanRoot(t)
	m := New(r, logx.Nop())
	m.hostLanguage = func() (string, error) { return "en-US", nil }
	oldScan := qemuScan
	qemuScan = func([]int) map[int]win.Process { return nil }
	t.Cleanup(func() { qemuScan = oldScan })
	t.Setenv(fakeAdbRegionalExitEnv, "1")
	t.Setenv(fakeAdbRegionalAppliedEnv, "app_process unavailable")
	var request *regional.LocaleRequest
	h := &createHooks{start: func(m *Manager, ctx context.Context, i int, _ StartOptions) (*StartResult, error) {
		s, err := m.readRegional(i)
		if err != nil {
			t.Fatal(err)
		}
		request = s.LocaleRequest
		if _, err := m.regionalLaunch(i); err != nil {
			t.Fatal(err)
		}
		if err := m.applyRegional(ctx, i, setupLaunched); err == nil {
			t.Fatal("expected guest language failure")
		}
		if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
			meta.Set("initialized", jsonord.NewBool(true))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return &StartResult{HostIME: true, LanguagePending: true}, nil
	}}
	h.install(t)
	// Keep the real check; restore the package hook explicitly rather than relying on the order of install's cleanup.
	oldCheck := createCheck
	t.Cleanup(func() { createCheck = oldCheck })
	createCheck = (*Manager).checkCreated
	in, err := m.Create(context.Background(), CreateRequest{NoBridge: true})
	if err != nil {
		t.Fatalf("language failure blocked creation: %v", err)
	}
	if in.Status != StatusOffline || in.Creation != "" || h.stopped != 1 || in.Regional.PendingLanguage != "en-US" {
		t.Fatalf("creation did not finish normally: %+v, stops %d", in, h.stopped)
	}
	s, err := m.readRegional(in.Index)
	if err != nil || !reflect.DeepEqual(s.LocaleRequest, request) {
		t.Fatalf("lost pending request: %+v, %v", s, err)
	}
	// The next cold boot retries the same request even if the host language changed.
	m.hostLanguage = func() (string, error) { return "zh-CN", nil }
	if _, err := m.regionalLaunch(in.Index); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeAdbRegionalExitEnv, "0")
	t.Setenv(fakeAdbRegionalAppliedEnv, "applied="+request.ID+"\nlanguage=en-US\ntime_zone=Etc/UTC\n")
	if err := m.applyRegional(context.Background(), in.Index, setupLaunched); err != nil {
		t.Fatal(err)
	}
	s, err = m.readRegional(in.Index)
	if err != nil || s.LocaleRequest != nil || s.LanguageApplied != "en-US" {
		t.Fatalf("retry not acknowledged: %+v, %v", s, err)
	}
}

func TestRegionalReadUsesUnprivilegedProperties(t *testing.T) {
	r, log := portPlanRoot(t)
	m := New(r, logx.Nop())
	in, err := m.Create(context.Background(), CreateRequest{filesOnly: true, NoBridge: true, Language: textPtr("en-US")})
	if err != nil {
		t.Fatal(err)
	}
	q := fakeQemu(t, filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64"), in.Index,
		r.Ports().Console(in.Index), false, avd.Dir(r, in.Index))
	oldScan := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{in.Index: q} }
	t.Cleanup(func() { qemuScan = oldScan })
	before, _ := os.ReadFile(r.MetadataPath())
	for _, tc := range []struct {
		name, output, exit string
		want               *RegionalGuest // nil: the read fails
	}{
		{"current guest differs from saved language", "language=ja-JP\r\ntime_zone=Asia/Tokyo\r\n", "0", &RegionalGuest{"ja-JP", "Asia/Tokyo"}},
		{"locale list shows its first language", "language=zh-CN,en-US\ntime_zone=Asia/Shanghai\n", "0", &RegionalGuest{"zh-CN", "Asia/Shanghai"}},
		{"unset time zone keeps the language", "language=ja-JP\ntime_zone=\n", "0", &RegionalGuest{"ja-JP", ""}},
		{"missing language", "language=\ntime_zone=Asia/Tokyo\n", "0", nil},
		{"empty first locale", "language=,en-US\ntime_zone=Asia/Tokyo\n", "0", nil},
		{"incomplete output", "language=ja-JP\n", "0", nil},
		{"adb failure", "device unavailable", "1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(fakeAdbRegionalPropertiesEnv, tc.output)
			t.Setenv(fakeAdbRegionalExitEnv, tc.exit)
			result, err := m.Regional(context.Background(), in.Index)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if result.Guest != nil || result.GuestError == "" {
					t.Fatalf("failed read was accepted: %+v", result)
				}
			} else if result.Guest == nil || *result.Guest != *tc.want || result.GuestError != "" {
				t.Fatalf("wrong guest values: %+v", result)
			}
		})
	}
	after, _ := os.ReadFile(r.MetadataPath())
	if string(before) != string(after) {
		t.Fatal("reading regional settings changed metadata")
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"push", "su 0", "app_process", "modavd-regional.dex"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("regional read ran %q: %s", forbidden, b)
		}
	}
	if strings.Count(string(b), "getprop persist.sys.locale") != 7 || !strings.Contains(string(b), "persist.sys.timezone") {
		t.Fatalf("missing property queries: %s", b)
	}
	// Offline and busy instances do not query their regional properties.
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, ok, err := win.TryLock(m.lockPath(in.Index, "start"))
	if err != nil || !ok {
		t.Fatalf("start lock: %v %v", ok, err)
	}
	result, err := m.Regional(context.Background(), in.Index)
	lock.Unlock()
	if err != nil || result.Guest != nil {
		t.Fatalf("busy guest was queried: %+v %v", result, err)
	}
	qemuScan = func([]int) map[int]win.Process { return nil }
	result, err = m.Regional(context.Background(), in.Index)
	if err != nil || result.Guest != nil {
		t.Fatalf("offline guest was queried: %+v %v", result, err)
	}
	b, _ = os.ReadFile(log)
	if strings.Contains(string(b), "persist.sys.locale") {
		t.Fatalf("offline or busy guest received a regional query: %s", b)
	}
}
