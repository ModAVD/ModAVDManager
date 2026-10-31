//go:build windows

package emu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/win"
)

func TestStatusOfAndBusyOf(t *testing.T) {
	if statusOf(false, false) != StatusOffline || statusOf(false, true) != StatusOffline {
		t.Fatal("no qemu = offline")
	}
	if statusOf(true, false) != StatusStarting || statusOf(true, true) != StatusOnline {
		t.Fatal("qemu: starting until booted")
	}
	if busyOf(false, false, false) != "" || busyOf(false, true, false) != "starting" || busyOf(true, false, false) != "stopping" ||
		busyOf(true, true, false) != "stopping" || busyOf(false, false, true) != "bridging" || busyOf(true, true, true) != "bridging" {
		t.Fatal("busyOf")
	}
}

// TestBridgeStateWithoutGuest: what info / list say about the bridge of an instance that is off or still booting
// (no guest to ask). Only a turned-off bridge is certain; the marker, which stays while the bridge is installed,
// means unknown once the first boot has run.
func TestBridgeStateWithoutGuest(t *testing.T) {
	r := managerTestRoot(t, 1)
	m := New(r, nil)
	ctx := context.Background()
	check := func(label string, initialized bool, want string) {
		t.Helper()
		if got := m.bridgeState(ctx, 1, false, initialized); got != want {
			t.Errorf("%s: bridge = %q, want %q", label, got, want)
		}
	}
	check("no marker", false, "pending")
	check("no marker, first boot done elsewhere", true, "pending")
	if err := writeBridgeMarker(r, 1); err != nil {
		t.Fatal(err)
	}
	check("marker, first boot not run", false, "pending")
	check("marker after the first boot", true, "unknown")
	if err := m.setBridgeWanted(1, false); err != nil {
		t.Fatal(err)
	}
	check("turned off", true, "off")
	check("turned off before the first boot", false, "off")
}

// TestInstanceInfoStartedAtMS: started_at_ms is started_at's moment in Unix milliseconds; both empty when off.
func TestInstanceInfoStartedAtMS(t *testing.T) {
	m := New(managerTestRoot(t, 1), nil)
	in, err := m.instance(1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 10, 28, 15, 4, 5, 123_000_000, time.Local)
	booted := false
	info := m.instanceInfo(ctx, in, &win.Process{PID: 4242, Created: filetimeOf(at)}, &booted)
	if info.StartedAtMS != at.UnixMilli() || info.StartedAt != at.Format(time.RFC3339) || info.PID != 4242 {
		t.Fatalf("running: started_at %q, started_at_ms %d (want %d)", info.StartedAt, info.StartedAtMS, at.UnixMilli())
	}
	off := m.instanceInfo(ctx, in, nil, nil)
	if off.StartedAtMS != 0 || off.StartedAt != "" || off.Bridge != "pending" {
		t.Fatalf("off: %+v", off)
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
