//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

func TestCaptureRuntimeRejectsInvalidTransport(t *testing.T) {
	m, _, _ := reviewCaptureFixture(t)
	for _, raw := range []string{`{}`, `{"port_base":21000,"adb_server_port":20050,"script_adb_server_port":21049}`, `{"port_base":"21000"}`, `{"port_base":21000,"adb_server_port":21050,"script_adb_server_port":21049,"extra":true}`, `{"port_base":21000,"adb_server_port":21050,"script_adb_server_port":21049} {}`} {
		if _, err := captureRuntimeFromToken(m.Root, base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Fatalf("accepted transport %s", raw)
		}
	}
}

func TestUnreadableMetadataRetainsTrustedCaptureTarget(t *testing.T) {
	for _, failure := range []string{"bad-json", "read-denied"} {
		t.Run(failure, func(t *testing.T) {
			m, old, adbLog := reviewCaptureFixture(t)
			old.Kill()
			old.Wait(time.Second)
			sdk := filepath.Join(m.Root.Dir, "alternate-sdk")
			copyTestExe(t, filepath.Join(sdk, "platform-tools", "adb.exe"))
			if err := m.Root.EditMetadata(func(o *jsonord.Obj) (bool, error) { o.Set("sdkRoot", jsonord.NewString(sdk)); return true, nil }); err != nil {
				t.Fatal(err)
			}
			q := fakeQemu(t, filepath.Join(sdk, "emulator", "qemu", "windows-x86_64"), 6, 21060, false, filepath.Join(m.Root.AvdHome(), "mas_6.avd"))
			qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: q} }
			pinned, err := m.Root.SnapshotRuntime()
			if err != nil {
				t.Fatal(err)
			}
			token, err := captureRuntimeToken(pinned)
			if err != nil {
				t.Fatal(err)
			}
			observed := filepath.Join(m.Root.Dir, "capture-env.json")
			t.Setenv("MODAVD_TEST_CAPTURE_ENV", observed)
			if failure == "bad-json" {
				if err := os.WriteFile(m.Root.MetadataPath(), []byte(`{"instances":`), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				name, _ := syscall.UTF16PtrFromString(m.Root.MetadataPath())
				h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { syscall.CloseHandle(h) })
			}
			var stderr bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- runLogcatCapture([]string{m.Root.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10), token}, &stderr, logcatSegmentBytes)
			}()
			t.Cleanup(func() {
				q.Kill()
				q.Wait(time.Second)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("capture did not exit")
				}
			})
			waitLogcatTest(t, func() bool { _, err := os.Stat(observed); return err == nil })
			q.Kill()
			q.Wait(time.Second)
			code := <-done
			done <- code
			if code != 0 {
				t.Fatalf("capture=%d: %s", code, stderr.String())
			}
			var got struct {
				Exe                    string
				Args                   []string
				SDK, Home, AVD, Server string
			}
			b, err := os.ReadFile(observed)
			if err != nil || json.Unmarshal(b, &got) != nil {
				t.Fatal(err, string(b))
			}
			if !strings.EqualFold(got.Exe, filepath.Join(sdk, "platform-tools", "adb.exe")) || got.SDK != sdk || got.Home != sdk || got.AVD != m.Root.AvdHome() || got.Server != strconv.Itoa(pinned.AdbServerPort()) || !logcatCmdlineMatches(got.Args, strconv.Itoa(pinned.AdbServerPort()), "emulator-21060") {
				t.Fatalf("metadata retargeted capture: %s", b)
			}
			if strings.Contains(strings.Join(got.Args, " "), "kernel") {
				t.Fatal("unreadable diagnostic policy did not fall back off")
			}
			guest, err := os.ReadFile(adbLog)
			if err != nil || !bytes.Contains(guest, []byte("logcat -b all -G 16M")) {
				t.Fatalf("ordinary guest buffers were not applied: %s/%v", guest, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(guest)), "\n") {
				var args []string
				if json.Unmarshal([]byte(line), &args) != nil {
					continue
				}
				for k, a := range args {
					if a == "-P" && (k+1 >= len(args) || args[k+1] != strconv.Itoa(pinned.AdbServerPort())) {
						t.Fatalf("guest buffer operation retargeted: %q", args)
					}
					if a == "-s" && (k+1 >= len(args) || args[k+1] != "emulator-21060") {
						t.Fatalf("guest serial retargeted: %q", args)
					}
				}
			}
			if strings.Count(stderr.String(), "诊断策略读不了") != 1 {
				t.Fatalf("warning episode=%s", stderr.String())
			}
		})
	}
}

func TestCaptureWithoutInitialTargetNeverGuessesDefaults(t *testing.T) {
	m, q, _ := reviewCaptureFixture(t)
	if err := os.WriteFile(m.Root.MetadataPath(), []byte(`{"instances":`), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := runLogcatCapture([]string{m.Root.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10)}, &output, logcatSegmentBytes); code != 1 || !strings.Contains(output.String(), "不猜默认") {
		t.Fatalf("initial unknown target=%d/%s", code, output.String())
	}
	if files, _ := os.ReadDir(m.Root.LogsDir()); len(files) != 0 {
		t.Fatal("unknown target started writing capture", files)
	}
}

func TestCaptureRejectsForgedDefaultTargetForCustomQemu(t *testing.T) {
	m, q, _ := reviewCaptureFixture(t)
	forged, err := m.Root.WithRuntime(root.RuntimeConfig{PortBase: 20000, AdbServerPort: 20050, ScriptAdbServerPort: 20049})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := captureRuntimeToken(forged)
	var stderr bytes.Buffer
	if code := runLogcatCapture([]string{m.Root.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10), token}, &stderr, logcatSegmentBytes); code != 1 || !strings.Contains(stderr.String(), "归属不匹配") {
		t.Fatalf("forged target accepted=%d/%s", code, stderr.String())
	}
}
