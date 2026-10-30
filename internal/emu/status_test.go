//go:build windows

package emu

import (
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

func TestStatusOfAndBusyOf(t *testing.T) {
	if statusOf(false, false) != StatusOffline || statusOf(false, true) != StatusOffline {
		t.Fatal("no qemu = offline")
	}
	if statusOf(true, false) != StatusStarting || statusOf(true, true) != StatusOnline {
		t.Fatal("qemu: starting until booted")
	}
	if busyOf(false, false) != "" || busyOf(false, true) != "starting" || busyOf(true, false) != "stopping" || busyOf(true, true) != "stopping" {
		t.Fatal("busyOf")
	}
}

func TestMetaTruthy(t *testing.T) {
	v, err := jsonord.Parse([]byte(`{"t":true,"f":false,"one":1,"zero":0,"fz":0.0,"fx":0.5,"s":"x","e":"","a":[1],"ea":[],"o":{"k":1},"eo":{},"n":null}`))
	if errs.CodeOf(err) == errs.NotImplemented {
		t.Skip("jsonord not implemented yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	o := v.Obj()
	want := map[string]bool{"t": true, "f": false, "one": true, "zero": false, "fz": false, "fx": true, "s": true, "e": false,
		"a": true, "ea": false, "o": true, "eo": false, "n": false, "missing": false}
	for k, w := range want {
		if got := metaTruthy(o.Get(k)); got != w {
			t.Errorf("metaTruthy(%s) = %v, want %v", k, got, w)
		}
	}
	if metaText(o.Get("s")) != "x" || metaText(o.Get("one")) != "" || metaText(o.Get("missing")) != "" {
		t.Error("metaText")
	}
}

func TestFormatApplied(t *testing.T) {
	if got := formatApplied(map[string]any{"memory_mb": 4096, "cpu": 4, "title": "主号"}); got != "cpu=4, memory_mb=4096, title=主号" {
		t.Fatalf("formatApplied = %q", got)
	}
	if got := formatApplied(nil); got != "无改动" {
		t.Fatalf("empty = %q", got)
	}
}

func TestCheckAPKPath(t *testing.T) {
	dir := opsTestTempDir(t)
	apk := filepath.Join(dir, "Game.APK")
	if err := os.WriteFile(apk, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := checkAPKPath(`"` + apk + `"`); err != nil || got != apk {
		t.Fatalf("quoted path: %q %v", got, err)
	}
	for _, bad := range []string{"", "  ", filepath.Join(dir, "x.xapk"), filepath.Join(dir, "missing.apk"), dir + `\sub.apk`} {
		if _, err := checkAPKPath(bad); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("checkAPKPath(%q) = %v", bad, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "d.apk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkAPKPath(filepath.Join(dir, "d.apk")); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("a directory is no APK: %v", err)
	}
}

func TestInstallResultLine(t *testing.T) {
	if got := installResultLine("Performing Streamed Install\r\nSuccess\r\n\r\n"); got != "Success" {
		t.Fatalf("got %q", got)
	}
	if got := installResultLine("  \n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// opsTestTempDir is a fresh directory, removed after the test: under $MODAVD_TEST_TMP\emu-ops when that is set (to keep
// test data on a chosen drive), otherwise t.TempDir().
func opsTestTempDir(t *testing.T) string {
	t.Helper()
	env := os.Getenv("MODAVD_TEST_TMP")
	if env == "" {
		return t.TempDir()
	}
	base := filepath.Join(env, "emu-ops")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
