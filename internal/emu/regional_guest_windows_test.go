//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package emu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/regional"
	"modavd/internal/root"
	"modavd/internal/win"
)

// Run through scripts/test_regional_guest.py on a disposable copied SDK. The test owns the acceptance handle
// until every emulator, private ADB server and background helper has finished.
func TestRegionalGuest(t *testing.T) {
	dir := os.Getenv("MODAVD_REGIONAL_ROOT")
	if dir == "" {
		t.Skip("requires a disposable SDK root and the regional acceptance runner")
	}
	lockPath := os.Getenv("MODAVD_REGIONAL_LOCK")
	if lockPath == "" {
		t.Fatal("missing exclusive acceptance lock path")
	}
	ptr, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("exclusive acceptance handle: %v", err)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := r.ReadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	share, ok := meta.Obj().Get("shareDefaultAdbServer").BoolVal()
	if !ok || share || r.Ports().Base != 34100 || r.AdbServerPort() != 34198 || r.ScriptAdbServerPort() != 34199 || r.LocalSDKRoot() != "" {
		t.Fatal("test root must own its SDK and explicitly use the assigned private port plan without 5037 sharing")
	}
	if instances, err := avd.List(r); err != nil || len(instances) != 0 {
		t.Fatalf("use a fresh test root: %v %v", instances, err)
	}
	var logMu sync.Mutex
	var messages []string
	logMessages := func() []string { logMu.Lock(); defer logMu.Unlock(); return append([]string{}, messages...) }
	m := New(r, logx.New(func(level logx.Level, message string) {
		logMu.Lock()
		messages = append(messages, message)
		logMu.Unlock()
	}))
	cli := filepath.Join(dir, "tools", "modavd.exe")
	oldHelper, oldWatch := win.OrphanHelper, bootWatchArgv
	host.SetOrphanHelper(cli)
	bootWatchArgv = func(rootDir string, i int, q host.Process) ([]string, error) {
		return []string{cli, BootWatchArg, rootDir, fmt.Sprint(i), fmt.Sprint(q.PID), fmt.Sprint(q.Created)}, nil
	}
	t.Cleanup(func() { host.SetOrphanHelper(oldHelper); bootWatchArgv = oldWatch })
	ctx := context.Background()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		indexes, err := m.StopTargets(stopCtx)
		if err != nil {
			t.Errorf("cleanup targets: %v", err)
		}
		for _, i := range indexes {
			if _, err := m.Stop(stopCtx, i, StopOptions{CancelCreate: true}); err != nil {
				t.Errorf("cleanup stop %d: %v", i, err)
			}
		}
		m.Wait(15 * time.Second)
		deadline := time.Now().Add(20 * time.Second)
		for {
			busy := false
			for _, i := range []int{0, 1, 2, 3, 4, 6, 7, 8, 9} {
				busy = busy || m.lockHeld(i, "watch") || m.lockHeld(i, "logcat")
			}
			if !busy {
				break
			}
			if time.Now().After(deadline) {
				t.Error("background helpers still hold instance locks during cleanup")
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		for _, port := range []int{34198, 34199} {
			cmd := exec.Command(r.AdbExe(), "-P", fmt.Sprint(port), "kill-server")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("private server cleanup %d: %v %s", port, err, out)
			}
		}
		procs, err := m.scanQemu([]int{0, 1, 2, 3, 4, 6, 7, 8, 9})
		if err != nil || len(procs) != 0 {
			t.Errorf("emulators remain after cleanup: %v %v", procs, err)
		}
		b, _ := json.MarshalIndent(logMessages(), "", "  ")
		if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "guest-manager-log.json"), b, 0o644); err != nil {
			t.Error(err)
		}
	})
	legacy := os.Getenv("MODAVD_REGIONAL_LEGACY_CLI")
	cmd := exec.Command(legacy, "--root", dir, "--json", "create", "--index", "0", "--memory", "3G", "--cpu", "2", "--data-gb", "16", "--no-bridge")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("legacy creation: %v %s", err, out)
	}
	legacyMeta := r.InstanceMeta(0).Clone()
	if legacyMeta.Has(regional.Key) {
		t.Fatal("legacy binary already writes regional settings")
	}
	start := func(i int) {
		t.Helper()
		if _, err := m.Start(ctx, i, StartOptions{Headless: true, NoBridge: true}); err != nil {
			t.Fatal(err)
		}
	}
	stop := func(i int) {
		t.Helper()
		if _, err := m.Stop(ctx, i, StopOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(i int, language, zone string) {
		t.Helper()
		v, err := m.regionalCommand(ctx, i, nil)
		if err != nil {
			t.Fatal(err)
		}
		if v["language"] != language || v["time_zone"] != zone {
			t.Fatalf("guest %d: %v, want %s / %s", i, v, language, zone)
		}
		res := m.shell(ctx, i, "getprop ro.build.version.release; getprop persist.sys.locale; getprop persist.sys.timezone; am get-config", 0)
		if res.Code != 0 || !strings.Contains(res.Out, "14") {
			t.Fatalf("Android evidence: %+v", res)
		}
		t.Logf("guest %d %s / %s; Android evidence: %s", i, language, zone, strings.Join(outputLines(res.Out), " | "))
	}
	update := func(i int, req UpdateRequest) {
		t.Helper()
		if _, err := m.Update(ctx, i, req); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("legacy configuration, including language selected in Android")
	start(0)
	v, err := m.regionalCommand(ctx, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyLanguage, legacyZone := v["language"], v["time_zone"]
	stop(0)
	m.hostLanguage = func() (string, error) { return "en-US", nil }
	m.hostTimeZone = func() (string, error) { return "Asia/Tokyo", nil }
	start(0)
	observe(0, legacyLanguage, legacyZone)
	stop(0)
	if r.InstanceMeta(0).Has(regional.Key) {
		t.Fatal("ordinary legacy boot wrote regional choices")
	}
	update(0, UpdateRequest{TimeZone: textPtr("host")})
	start(0)
	observe(0, legacyLanguage, "Asia/Tokyo")
	stop(0)
	update(0, UpdateRequest{TimeZone: textPtr("UTC")})
	start(0)
	observe(0, legacyLanguage, "Etc/UTC")
	stop(0)

	t.Log("new instance with actual Windows UI language and time zone")
	m.hostLanguage, m.hostTimeZone = regional.HostLanguage, regional.HostTimeZone
	actualLanguage, err := regional.HostLanguage()
	if err != nil {
		t.Fatal(err)
	}
	actualZone, err := regional.HostTimeZone()
	if err != nil {
		t.Fatal(err)
	}
	i := 1
	before := len(logMessages())
	if _, err := m.Create(ctx, CreateRequest{Index: &i, MemoryMB: regionalInt(3072), CPU: regionalInt(2), DataPartitionGB: regionalInt(16), NoBridge: true}); err != nil {
		t.Fatal(err)
	}
	launches := 0
	for _, message := range logMessages()[before:] {
		if strings.Contains(message, "启动 ModAVD 实例 1（") {
			launches++
		}
	}
	if launches != 1 {
		t.Fatalf("creation added cold launches: %d", launches)
	}
	start(1)
	observe(1, actualLanguage, actualZone)
	// Model an Android settings change through the same framework API, outside the manager's saved request.
	guestChange, _ := regional.NewLocaleRequest("ja-JP")
	identityBefore := m.shell(ctx, 1, "cat /proc/sys/kernel/random/boot_id; pidof system_server", 0).Out
	if _, err := m.regionalCommand(ctx, 1, guestChange); err != nil {
		t.Fatal(err)
	}
	identityAfter := m.shell(ctx, 1, "cat /proc/sys/kernel/random/boot_id; pidof system_server", 0).Out
	if identityBefore != identityAfter {
		t.Fatal("language change restarted Android or system_server")
	}
	observe(1, "ja-JP", actualZone)
	start(1)
	observe(1, "ja-JP", actualZone)
	stop(1)
	m.hostLanguage = func() (string, error) { return "en-US", nil }
	m.hostTimeZone = func() (string, error) { return "America/New_York", nil }
	start(1)
	observe(1, "ja-JP", "America/New_York")
	// An edit while running must not be consumed by an ordinary adoption of this process.
	update(1, UpdateRequest{Language: textPtr("host"), TimeZone: textPtr("Asia/Shanghai")})
	start(1)
	observe(1, "ja-JP", "America/New_York")
	stop(1)
	start(1)
	observe(1, "en-US", "Asia/Shanghai")
	requestBefore, _ := r.InstanceMeta(1).Get(regional.Key).Obj().Get("languageApplied").Str()
	if requestBefore != "en-US" {
		t.Fatal("explicit request not acknowledged")
	}
	// A later first-boot retry must not repeat language initialization.
	if err := r.EditInstanceMeta(1, func(meta *jsonord.Obj) error { meta.Set("initialized", jsonord.NewBool(false)); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.regionalCommand(ctx, 1, guestChange); err != nil {
		t.Fatal(err)
	}
	stop(1)
	start(1)
	observe(1, "ja-JP", "Asia/Shanghai")
	stop(1)

	t.Log("injected English host, then a recoverable failure after the language was applied")
	i = 2
	check := createCheck
	t.Cleanup(func() { createCheck = check })
	var originalRequest *regional.LocaleRequest
	createCheck = func(m *Manager, ctx context.Context, i int, bridge bool, sr *StartResult) error {
		if err := check(m, ctx, i, bridge, sr); err != nil {
			return err
		}
		observe(i, "en-US", "America/New_York")
		return errs.New(errs.Canceled, "injected cancellation after language initialization")
	}
	m.hostTimeZone = func() (string, error) { return "America/New_York", nil }
	// Capture the concrete request before the first boot clears the host record.
	progress := func(p Progress) {
		if p.Step == "launch" {
			s, _ := m.readRegional(2)
			if s != nil && s.LocaleRequest != nil {
				copy := *s.LocaleRequest
				originalRequest = &copy
			}
		}
	}
	if _, err := m.Create(ctx, CreateRequest{Index: &i, MemoryMB: regionalInt(3072), CPU: regionalInt(2), DataPartitionGB: regionalInt(16), NoBridge: true, Progress: progress}); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("injected failure: %v", err)
	}
	createCheck = check
	m.hostLanguage = func() (string, error) { return "zh-CN", nil }
	if _, err := m.Create(ctx, CreateRequest{Index: &i, Resume: true}); err != nil {
		t.Fatal(err)
	}
	start(2)
	observe(2, "en-US", "America/New_York")
	// Restore a stale host request to model losing only the acknowledgement write after guest success.
	if originalRequest == nil {
		t.Fatal("initial request not captured")
	}
	change, _ := regional.NewLocaleRequest("zh-CN")
	if _, err := m.regionalCommand(ctx, 2, change); err != nil {
		t.Fatal(err)
	}
	// Android Settings changes Configuration without changing our receipt. Restore that receipt after using
	// the helper to model a user language edit, then restore the host's unacknowledged original request.
	res := m.su(ctx, 2, "printf "+originalRequest.ID+" > /data/local/tmp/modavd-language-applied; sync", 0)
	if res.Code != 0 {
		t.Fatal(res.Out)
	}
	if err := r.EditInstanceMeta(2, func(meta *jsonord.Obj) error {
		s, err := regional.Read(meta)
		if err != nil {
			return err
		}
		s.LocaleRequest = originalRequest
		s.Set(meta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stop(2)
	start(2)
	observe(2, "zh-CN", "America/New_York")
	stop(2)
	t.Logf("actual host acceptance: %s / %s. English and host time-zone changes above use injected inputs.", actualLanguage, actualZone)
}

func regionalInt(value int) *int { return &value }
