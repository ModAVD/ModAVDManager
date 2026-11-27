// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
)

// scriptedGuest answers the shell commands of the import code the way a guest would, and records every script.
// Nothing here starts adb: importDevice.exec stands in for it.
type scriptedGuest struct {
	m       *Manager // for tests that need a root
	mu      sync.Mutex
	scripts []string
	reply   func(script string) (string, error)
}

var mktempParent = regexp.MustCompile(`(/[^'"\s]+)/\.modavd-import\.XXXXXXXX`)

func (g *scriptedGuest) device() importDevice {
	m := g.m
	if m == nil {
		m = &Manager{Log: logx.Nop()}
	}
	return importDevice{m: m, exec: func(_ context.Context, args ...string) (string, error) {
		script := args[len(args)-1]
		g.mu.Lock()
		g.scripts = append(g.scripts, script)
		g.mu.Unlock()
		if strings.Contains(script, "mktemp -d") {
			// mktemp -d '<parent>/.modavd-import.XXXXXXXX', possibly inside the su wrapper's extra quoting
			parent := mktempParent.FindStringSubmatch(script)[1]
			return parent + "/.modavd-import.AbCd1234\n", nil
		}
		if g.reply != nil {
			return g.reply(script)
		}
		return "", nil
	}}
}

func (g *scriptedGuest) ran(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.scripts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// fakeStream replaces the adb transfer: size gives the number of bytes the source delivers for a read/write pair.
// An APK goes through as raw bytes, a resource as a tar holding one file named like the item.
func fakeStream(t *testing.T, size func(read, write string) int) {
	t.Helper()
	saved := importStreamFunc
	t.Cleanup(func() { importStreamFunc = saved })
	importStreamFunc = func(_ context.Context, _, _ importDevice, read, write string, copyData func(io.Writer, io.Reader) error) error {
		n := size(read, write)
		if strings.HasPrefix(write, "cat > ") {
			return copyData(io.Discard, bytes.NewReader(make([]byte, n)))
		}
		name := strings.TrimSuffix(read[strings.Index(read, "tar -cf - './")+len("tar -cf - './"):], "'")
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		if err := w.WriteHeader(&tar.Header{Name: name, Size: int64(n), Mode: 0660, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := w.Write(make([]byte, n)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return copyData(io.Discard, &buf)
	}
}

// ModAVD/ModAVD#271: an item whose transferred size differs from the size counted at the start is not published.
func TestCopyResourceDoesNotPublishChangedSource(t *testing.T) {
	const full = "/sdcard/Android/data/example.game/files/assets"
	for _, tc := range []struct {
		name      string
		got, want int
		publish   bool
	}{
		{"shorter than counted", 150, 200, false},
		{"longer than counted", 250, 200, false},
		{"as counted", 200, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &importProgress{done: 1000} // earlier items of the same import
			fakeStream(t, func(string, string) int { return tc.got })
			g := &scriptedGuest{}
			dev := g.device()
			err := copyResource(context.Background(), dev, dev, full, int64(tc.want), p)
			if tc.publish {
				if err != nil {
					t.Fatal(err)
				}
			} else if errs.CodeOf(err) != errs.Internal || !strings.Contains(err.Error(), "源文件大小在导入期间发生变化") {
				t.Fatalf("err=%v", err)
			}
			if g.ran("publish()") != tc.publish {
				t.Fatalf("published=%v scripts=%q", g.ran("publish()"), g.scripts)
			}
			// Only this item's staging directory is removed, whatever happened.
			if !g.ran("rm -rf '/sdcard/Android/data/example.game/files/.modavd-import.AbCd1234'") {
				t.Fatalf("staging directory was not removed: %q", g.scripts)
			}
			if g.ran("rm -rf '" + full) {
				t.Fatalf("removed the final directory: %q", g.scripts)
			}
		})
	}
}

// ModAVD/ModAVD#272: staging directories an interrupted import left behind are removed from the parents of the
// resources and from /data/local/tmp, and only those whose name is exactly .modavd-import. plus eight characters.
func TestRemoveStaleImportDirsScript(t *testing.T) {
	g := &scriptedGuest{}
	dev := g.device().as("10123,10123,9997")
	parents := resourceParents([]string{
		"/sdcard/Android/data/example.game/files/a", "/sdcard/Android/data/example.game/files/b",
		"/sdcard/Android/obb/example.game/main.1.example.game.obb",
	})
	want := []string{"/sdcard/Android/data/example.game/files", "/sdcard/Android/obb/example.game"}
	if !slices.Equal(parents, want) {
		t.Fatalf("parents=%q", parents)
	}
	if err := removeStaleImportDirs(context.Background(), dev, append(parents, "/data/local/tmp")); err != nil {
		t.Fatal(err)
	}
	if len(g.scripts) != 3 {
		t.Fatalf("scripts=%q", g.scripts)
	}
	for i, parent := range append(want, "/data/local/tmp") {
		s := g.scripts[i]
		for _, sub := range []string{"su 10123,10123,9997", "find '\"'\"'" + parent + "'\"'\"' -mindepth 1 -maxdepth 1 -type d", ".modavd-import.????????", "rm -rf"} {
			if !strings.Contains(s, sub) {
				t.Fatalf("script %d lacks %q: %s", i, sub, s)
			}
		}
	}
}

func TestRemoveStaleImportDirsKeepsGoingAfterAFailure(t *testing.T) {
	g := &scriptedGuest{reply: func(string) (string, error) { return "", errors.New("permission denied") }}
	if err := removeStaleImportDirs(context.Background(), g.device(), []string{"/a", "/b"}); err != nil || len(g.scripts) != 2 {
		t.Fatalf("err=%v scripts=%d", err, len(g.scripts))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeStaleImportDirs(ctx, g.device(), []string{"/a"}); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("err=%v", err)
	}
}

// Runs the generated command with a real POSIX shell (skipped without one) on a temporary directory.
func TestRemoveStaleImportDirsOnDisk(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	parent := filepath.ToSlash(t.TempDir())
	mkdir := func(name string) {
		if err := os.MkdirAll(parent+"/"+name, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		".modavd-import.AbCd1234/data", ".modavd-import.ZZZZ9999",
		".modavd-import.short", ".modavd-import.AbCd12345", "modavd-import.AbCd1234", "keep/.modavd-import.AbCd1234",
	} {
		mkdir(name)
	}
	if err := os.WriteFile(parent+"/.modavd-import.FileFile", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent+"/.modavd-import.AbCd1234/data/big", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Git for Windows keeps find in ..\usr\bin; Windows' own find.exe must not be the one that runs.
	posix := func(script string) (string, error) {
		cmd := exec.Command(sh, "-c", script)
		bin := filepath.Dir(sh)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+filepath.Join(bin, "..", "usr", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if _, err := posix("find '" + parent + "' -mindepth 1 -maxdepth 1 -type d -name 'nothing' -exec rm -rf {} +"); err != nil {
		t.Skip("sh has no POSIX find: ", err)
	}
	g := &scriptedGuest{reply: posix}
	dev := g.device()
	if err := removeStaleImportDirs(context.Background(), dev, []string{parent, parent + "/missing"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{".modavd-import.AbCd12345", ".modavd-import.FileFile", ".modavd-import.short", "keep", "modavd-import.AbCd1234"}
	if !slices.Equal(left, want) {
		t.Fatalf("left %q, want %q", left, want)
	}
	if _, err := os.Stat(parent + "/keep/.modavd-import.AbCd1234"); err != nil {
		t.Fatal("removed a nested directory")
	}
}

// ModAVD/ModAVD#273: a game running on either side stops the import before anything is touched.
func TestAppRunningReadsProcessNames(t *testing.T) {
	list := func(names ...string) string {
		return "NAME\ninit\nzygote64\n" + strings.Join(names, "\n") + "\nsystem_server\nexit:0\n"
	}
	for _, tc := range []struct {
		name, out string
		running   bool
		fail      bool
	}{
		{"main process", list("example.game"), true, false},
		{"only a service", list("example.game:download"), true, false},
		{"main process and service", list("example.game", "example.game:push"), true, false},
		{"CRLF", strings.ReplaceAll(list("example.game:download"), "\n", "\r\n"), true, false},
		{"no header", "init\nexample.game\nexit:0\n", true, false},
		{"padded names", "NAME                       \ninit                       \nexample.game               \nexit:0\n", true, false},
		{"not running", list(), false, false},
		{"longer package name", list("example.gamex", "example.game.extra"), false, false},
		{"other package's service named like the game", list("other.app:example.game"), false, false},
		{"suffix only", list("my.example.game", "my.example.game:svc"), false, false},
		{"no ps", "/system/bin/sh: ps: inaccessible or not found\nexit:127\n", false, true},
		{"ps failed after a partial list", "NAME\ninit\nexit:1\n", false, true},
		{"no process list", "NAME\nexit:0\n", false, true},
		{"no output", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
			running, err := appRunning(context.Background(), g.device(), "example.game")
			if running != tc.running || (err != nil) != tc.fail {
				t.Fatalf("running=%v err=%v", running, err)
			}
			if len(g.scripts) != 1 || !strings.Contains(g.scripts[0], "ps -A -o NAME") {
				t.Fatalf("scripts=%q", g.scripts)
			}
		})
	}
}

func TestCheckGamesStopped(t *testing.T) {
	games := []ImportGame{{Package: "example.one"}, {Package: "example.two"}}
	answer := func(names ...string) func(string) (string, error) {
		return func(string) (string, error) {
			return "NAME\ninit\n" + strings.Join(names, "\n") + "\nexit:0\n", nil
		}
	}
	for _, tc := range []struct {
		name     string
		dst, src []string
		wantErr  string
		srcAsked bool
	}{
		{"both stopped", nil, nil, "", true},
		{"target running", []string{"example.two"}, nil, "目标实例上的 example.two 正在运行", false},
		{"target service only", []string{"example.two:download"}, nil, "目标实例上的 example.two 正在运行", false},
		{"source running", nil, []string{"example.one"}, "源模拟器上的 example.one 正在运行", true},
		{"source service only", nil, []string{"example.one:download"}, "源模拟器上的 example.one 正在运行", true},
		{"both running", []string{"example.one"}, []string{"example.one"}, "目标实例上的 example.one 正在运行", false},
		{"similar names are not the game", []string{"example.onex", "other:example.one"}, []string{"example.two.x"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst, src := &scriptedGuest{reply: answer(tc.dst...)}, &scriptedGuest{reply: answer(tc.src...)}
			err := checkGamesStopped(context.Background(), src.device(), dst.device(), games)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err=%v", err)
			}
			if (len(src.scripts) > 0) != tc.srcAsked {
				t.Fatalf("source asked=%v", len(src.scripts) > 0)
			}
			for _, s := range append(slices.Clone(src.scripts), dst.scripts...) {
				if strings.Contains(s, "kill") || strings.Contains(s, "force-stop") || strings.Contains(s, "am ") {
					t.Fatalf("not read-only: %q", s)
				}
			}
		})
	}
}

// ModAVD/ModAVD#274: after an import our connection to the source is dropped from the script adb server. The
// command names the endpoint and is not bound to it with -s; a failure is logged and does not panic.
func TestImportDeviceDisconnect(t *testing.T) {
	var calls [][]string
	var logged []string
	log := logx.New(func(_ logx.Level, msg string) { logged = append(logged, msg) })
	fail := false
	d := importDevice{m: &Manager{Log: log}, serial: "127.0.0.1:16384", exec: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		if fail {
			return "", errors.New("adb: boom")
		}
		return "disconnected 127.0.0.1:16384\n", nil
	}}
	d.disconnect()
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"disconnect", "127.0.0.1:16384"}) || len(logged) != 0 {
		t.Fatalf("calls=%q logged=%q", calls, logged)
	}
	fail = true
	d.disconnect()
	if len(calls) != 2 || len(logged) != 1 || !strings.Contains(logged[0], "127.0.0.1:16384") || !strings.Contains(logged[0], "boom") {
		t.Fatalf("calls=%q logged=%q", calls, logged)
	}
	// A device that never connected has nothing to drop.
	importDevice{m: &Manager{Log: log}, exec: d.exec}.disconnect()
	if len(calls) != 2 {
		t.Fatalf("disconnected an unconnected device: %q", calls)
	}
}

// A batch is checked again before each later game: one started on either side after the batch began stops the
// import there, with the games before it imported.
func TestImportEachChecksBeforeEveryLaterGame(t *testing.T) {
	games := []ImportGame{{Package: "example.one"}, {Package: "example.two"}, {Package: "example.three"}}
	for _, tc := range []struct {
		name        string
		startAfter  int // number of games imported when the game below starts running
		onTarget    bool
		running     string
		wantImports []string
		wantErr     string
	}{
		{"nothing starts", -1, true, "", []string{"example.one", "example.two", "example.three"}, ""},
		{"target game starts during the first", 1, true, "example.two", []string{"example.one"}, "目标实例上的 example.two 正在运行"},
		{"source game starts during the second", 2, false, "example.three:push", []string{"example.one", "example.two"}, "源模拟器上的 example.three 正在运行"},
		{"a game already imported may run", 1, true, "example.one", []string{"example.one", "example.two", "example.three"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var imported []string
			names := func() string { return "NAME\ninit\nexit:0\n" }
			reply := func(string) (string, error) { return names(), nil }
			dst, src := &scriptedGuest{reply: reply}, &scriptedGuest{reply: reply}
			guest := src
			if tc.onTarget {
				guest = dst
			}
			guest.reply = func(string) (string, error) {
				if tc.startAfter >= 0 && len(imported) >= tc.startAfter {
					return "NAME\ninit\n" + tc.running + "\nexit:0\n", nil
				}
				return names(), nil
			}
			saved := importGameFunc
			t.Cleanup(func() { importGameFunc = saved })
			importGameFunc = func(_ context.Context, _, _ importDevice, g ImportGame, _ func(Progress)) (GameImportResult, error) {
				imported = append(imported, g.Package)
				return GameImportResult{Package: g.Package}, nil
			}
			m := &Manager{Log: logx.Nop()}
			results, err := m.importEach(context.Background(), src.device(), dst.device(), games, nil)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err=%v", err)
			}
			if !slices.Equal(imported, tc.wantImports) || len(results) != len(tc.wantImports) {
				t.Fatalf("imported %q, results %d, want %q", imported, len(results), tc.wantImports)
			}
		})
	}
}
