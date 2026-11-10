//go:build windows

package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

// protectedCDK is what AUTO-MAS passes as --cdk-dpapi: CryptProtectData of the UTF-8 CDK, no entropy, base64.
func protectedCDK(t *testing.T, cdk string) string {
	t.Helper()
	blob, err := win.Protect([]byte(cdk), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(blob)
}

func TestCallerCDK(t *testing.T) {
	for _, empty := range []string{"", "   ", "\t\r\n"} {
		if got, err := CallerCDK(empty); err != nil || got != "" {
			t.Fatalf(`%q: %q %v`, empty, got, err)
		}
	}
	if got, err := CallerCDK(protectedCDK(t, "  caller-cdk-42\r\n")); err != nil || got != "caller-cdk-42" {
		t.Fatalf("valid: %q %v", got, err)
	}
	withEntropy, err := win.Protect([]byte("entropy-cdk-7"), []byte("other"))
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"not base64":       "%%%not-base64%%%",
		"not a DPAPI blob": base64.StdEncoding.EncodeToString([]byte("plain-cdk-not-encrypted")),
		"other entropy":    base64.StdEncoding.EncodeToString(withEntropy),
		"space in CDK":     protectedCDK(t, "bad cdk-9"),
		"= in CDK":         protectedCDK(t, "cdk=9"),
		"too long":         protectedCDK(t, strings.Repeat("x", 257)),
	}
	for name, in := range bad {
		got, err := CallerCDK(in)
		if errs.CodeOf(err) != errs.InvalidArgument || got != "" {
			t.Errorf("%s: %q %v", name, got, err)
			continue
		}
		e := errs.As(err)
		text := e.Message + e.Advice + err.Error()
		for _, secret := range []string{in, "plain-cdk-not-encrypted", "entropy-cdk-7", "bad cdk-9", "cdk=9", "xxxxxxxx"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s: the error quotes %q: %s", name, secret, text)
			}
		}
	}
}

// collect is a logger that keeps every message and also writes the --json lines (with fields).
func collect() (*logx.Logger, *bytes.Buffer, func() string) {
	var mu sync.Mutex
	var msgs []string
	var buf bytes.Buffer
	l := logx.NewFields(logx.JSONFieldSink(&buf, logx.Debug), func(_ logx.Level, msg string, _ logx.Fields) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	})
	return l, &buf, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(msgs, "\n")
	}
}

func settingsBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCallerSettingsOverride: --source / --cdk-dpapi of one run beat the saved settings, reach MirrorChyan, are
// reported as in effect, and are never saved, logged or written anywhere in the root.
func TestCallerSettingsOverride(t *testing.T) {
	withSettings(t, SourceGitHub, "saved-cdk-1")
	before := settingsBytes(t)
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	var seen []map[string]string
	f.mirror = func(q map[string]string) (int, any) {
		seen = append(seen, q)
		if q["cdk"] == "" {
			return 0, map[string]any{"version_name": "v0.37.0", "release_note": ""}
		}
		return 7002, nil
	}
	u := f.updater(r)
	log, jsonLines, msgs := collect()
	u.Log = log
	cdk := "caller-cdk-secret-9"
	u.Opt.Source, u.Opt.CDK = SourceMirrorChyan, &cdk

	if st := u.Status(); st.Source != SourceMirrorChyan || !st.CDKSet {
		t.Fatalf("status: %+v", st)
	}
	st, err := u.Install(context.Background())
	if err != nil || !st.Updated || len(st.Notices) != 1 || !strings.Contains(st.Notices[0], "填入的 CDK 错误") {
		t.Fatalf("install: %+v %v", st, err)
	}
	if len(seen) == 0 || seen[len(seen)-1]["cdk"] != cdk {
		t.Fatalf("MirrorChyan did not get the caller's CDK: %v", seen)
	}
	for _, q := range seen {
		if q["cdk"] == "saved-cdk-1" {
			t.Fatal("the saved CDK was used")
		}
	}
	if !bytes.Equal(settingsBytes(t), before) {
		t.Fatal("the settings file changed")
	}
	if s := LoadSettings(); s.Source != SourceGitHub || s.CDK() != "saved-cdk-1" {
		t.Fatalf("saved settings changed: %+v", s)
	}
	stJSON, _ := json.Marshal(st)
	state, _ := os.ReadFile(filepath.Join(r.Dir, ".update", "state.json"))
	for where, text := range map[string]string{"logs": msgs(), "json logs": jsonLines.String(), "status": string(stJSON),
		"state.json": string(state), "settings": string(before)} {
		if strings.Contains(text, cdk) {
			t.Errorf("the caller's CDK is in the %s", where)
		}
	}
}

// TestCallerSettingsNoCDK: --cdk-dpapi "" is no CDK even with one saved; --source github never asks MirrorChyan.
func TestCallerSettingsNoCDK(t *testing.T) {
	withSettings(t, SourceMirrorChyan, "saved-cdk-2")
	before := settingsBytes(t)
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	var seen []map[string]string
	f.mirror = func(q map[string]string) (int, any) {
		seen = append(seen, q)
		return 0, map[string]any{"version_name": "v0.37.0", "release_note": ""}
	}
	u := f.updater(r)
	none := ""
	u.Opt.CDK = &none
	if st := u.Status(); st.Source != SourceMirrorChyan || st.CDKSet {
		t.Fatalf("status: %+v", st)
	}
	st, err := u.Prepare(context.Background())
	if err != nil || st.PreparedVersion != "v0.37.0" || st.CDKSet {
		t.Fatalf("prepare: %+v %v", st, err)
	}
	for _, q := range seen {
		if q["cdk"] != "" {
			t.Fatalf("a CDK was sent: %v", q)
		}
	}

	seen = nil
	u.Opt.Source, u.Opt.CDK = SourceGitHub, nil
	if st := u.Status(); st.Source != SourceGitHub || !st.CDKSet {
		t.Fatalf("status with --source github: %+v", st)
	}
	if st, err := u.Check(context.Background()); err != nil || st.LatestSource != SourceGitHub {
		t.Fatalf("check: %+v %v", st, err)
	}
	if len(seen) != 0 {
		t.Fatalf("MirrorChyan was asked: %v", seen)
	}
	if !bytes.Equal(settingsBytes(t), before) {
		t.Fatal("the settings file changed")
	}
}

// TestStepProgressLine: a stage change is one --json log line with the progress object.
func TestStepProgressLine(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	log, buf, _ := collect()
	u.Log = log
	var got []Progress
	u.Opt.Progress = func(p Progress) { got = append(got, p) }
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	stages := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		p, ok := m["progress"].(map[string]any)
		if !ok {
			continue
		}
		if len(p) != 4 || p["percent"] != -1.0 || p["done_bytes"] != 0.0 || p["total_bytes"] != 0.0 || m["msg"] == "" ||
			m["level"] != "info" {
			t.Errorf("progress line %q", line)
		}
		stages[p["stage"].(string)] = true
	}
	for _, s := range []string{"checking", "extracting", "verifying"} {
		if !stages[s] {
			t.Errorf("no progress line for %s: %s", s, buf.String())
		}
	}
	if len(got) == 0 || got[0].Stage != "checking" || got[0].Percent != -1 {
		t.Errorf("Progress callback: %+v", got)
	}
}
