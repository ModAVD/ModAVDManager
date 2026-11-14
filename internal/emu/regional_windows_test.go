//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package emu

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/regional"
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
