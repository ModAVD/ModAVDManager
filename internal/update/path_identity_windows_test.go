//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

func updatePathAlias(t *testing.T, target, kind string) string {
	t.Helper()
	if kind == "short name" {
		ptr, err := syscall.UTF16PtrFromString(target)
		if err != nil {
			t.Fatal(err)
		}
		var buf [32768]uint16
		n, err := syscall.GetShortPathName(ptr, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 || n >= uint32(len(buf)) {
			t.Fatalf("GetShortPathName: %d, %v", n, err)
		}
		alias := syscall.UTF16ToString(buf[:n])
		if strings.EqualFold(alias, target) {
			t.Skip("test volume does not create 8.3 names")
		}
		return alias
	}
	alias := filepath.Join(testutil.TempDir(t), "alias")
	if kind == "symlink" {
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("directory symlink unavailable: %v", err)
		}
	} else {
		cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", alias, target)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v\n%s", err, out)
		}
	}
	st, err := os.Stat(alias)
	if err != nil || !st.IsDir() {
		t.Fatalf("invalid directory alias: %v", err)
	}
	return alias
}

func TestSelfUpdatePathAliases(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"junction", "symlink", "short name"} {
		for _, reverse := range []bool{false, true} {
			for _, action := range []string{"check", "apply", "restore", "recover finish", "recover rollback"} {
				name := kind + "/alias root/" + action
				if reverse {
					name = kind + "/alias image/" + action
				}
				t.Run(name, func(t *testing.T) {
					real := filepath.Join(testutil.TempDir(t), "root with long name")
					if err := copyFile(self, filepath.Join(real, "tools", "modavd.exe")); err != nil {
						t.Fatal(err)
					}
					alias := updatePathAlias(t, real, kind)
					rootPath, imageRoot := alias, real
					if reverse {
						rootPath, imageRoot = real, alias
					}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, filepath.Join(imageRoot, "tools", "modavd.exe"), "-test.run=^TestSelfUpdatePathAliasChild$", "-test.v")
					cmd.Env = append(os.Environ(), "MODAVD_PATH_ALIAS_ROOT="+rootPath, "MODAVD_PATH_ALIAS_ACTION="+action)
					cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("self update: %v\n%s", err, out)
					}
				})
			}
		}
	}
}

func TestSelfUpdatePathAliasChild(t *testing.T) {
	dir := os.Getenv("MODAVD_PATH_ALIAS_ROOT")
	if dir == "" {
		t.Skip("self-update fixture only")
	}
	l := layout{root: dir}
	const rel = "tools/modavd.exe"
	plan := rootPlan{replace: []string{rel}}
	action := os.Getenv("MODAVD_PATH_ALIAS_ACTION")
	if action == "check" {
		if busy := l.rootFileInUse(plan); busy != "" {
			t.Fatalf("self treated as occupant: %s", busy)
		}
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := copyFile(self, l.at("ModAVD.exe")); err != nil {
			t.Fatal(err)
		}
		occupancyChild(t, l.at("ModAVD.exe"))
		plan.replace = append(plan.replace, "ModAVD.exe")
		if busy := l.rootFileInUse(plan); busy != "ModAVD.exe" {
			t.Fatalf("different running image was exempted: %q", busy)
		}
		return
	}
	source := l.pkgAt(rel)
	if action == "restore" || action == "recover rollback" {
		source = l.prevAt(rel)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(source, action)
	var err error
	switch action {
	case "apply":
		err = l.applyRootFiles(plan)
	case "restore":
		err = l.restoreRootFiles(plan)
	case "recover finish", "recover rollback":
		write(l.at("mas-avd.json"), "{}")
		if err := os.MkdirAll(l.at("sdk"), 0o755); err != nil {
			t.Fatal(err)
		}
		st := newState(phaseSwapping, "plan", plan.value(), "swap", stepVal(stepOldMoved))
		target := jsonord.NewObject()
		target.Obj().Set("version", jsonord.NewString("v0.37.0"))
		st.Set("target", target)
		if action == "recover finish" {
			write(filepath.Join(l.stageEmulator(), "marker"), "new SDK")
			staged := jsonord.NewObject()
			staged.Obj().Set("verified", jsonord.NewBool(true))
			st.Set("staged", staged)
		} else {
			write(filepath.Join(l.previousEmulator(), "marker"), "old SDK")
		}
		if err := l.writeState(st); err != nil {
			t.Fatal(err)
		}
		r, err := root.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		u := New(r, nil, Options{})
		u.scan = ownScan(u)
		if _, err := u.recover(); err != nil {
			t.Fatal(err)
		}
		if phaseOf(l.readState()) != phaseIdle || !isDir(l.emulator()) {
			t.Fatal("recovery did not commit its result")
		}
		wantSDK := "old SDK"
		if action == "recover finish" {
			wantSDK = "new SDK"
		}
		if got, err := os.ReadFile(filepath.Join(l.emulator(), "marker")); err != nil || string(got) != wantSDK {
			t.Fatalf("wrong recovered SDK: %q, %v", got, err)
		}
		ok, _ := l.readState().Get("lastResult").Obj().Get("ok").BoolVal()
		if ok != (action == "recover finish") {
			t.Fatalf("wrong recovery result: ok=%v", ok)
		}
	default:
		t.Fatalf("unknown action: %s", action)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(l.at(rel)); err != nil || string(got) != action {
		t.Fatalf("wrong replacement: %q, %v", got, err)
	}
}

func TestPathAliasProcessOwnership(t *testing.T) {
	for _, kind := range []string{"junction", "symlink", "short name"} {
		for _, reverse := range []bool{false, true} {
			for _, rel := range []string{"tools/modavd.exe", "ModAVD.exe", "sdk/emulator/emulator.exe", ".update/previous/emulator/emulator.exe"} {
				name := kind + "/alias root/" + rel
				if reverse {
					name = kind + "/alias image/" + rel
				}
				t.Run(name, func(t *testing.T) {
					r := writeRoot(t, 36, nil)
					self, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					if err := copyFile(self, filepath.Join(r.Dir, filepath.FromSlash(rel))); err != nil {
						t.Fatal(err)
					}
					alias := updatePathAlias(t, r.Dir, kind)
					rootPath, imageRoot := alias, r.Dir
					if reverse {
						rootPath, imageRoot = r.Dir, alias
					}
					u := New(&root.Root{Dir: rootPath}, nil, Options{})
					if err := u.l.writeState(newState(phaseSwapping)); err != nil {
						t.Fatal(err)
					}
					child := occupancyChild(t, filepath.Join(imageRoot, filepath.FromSlash(rel)))
					program := rel == "tools/modavd.exe" || rel == "ModAVD.exe"
					for _, programs := range []bool{false, true} {
						uses, err := u.scanProcesses(programs, ownProcesses, win.Process.ImagePath)
						if err != nil {
							t.Fatal(err)
						}
						want := !program || programs
						if !want {
							if len(uses) != 0 {
								t.Fatalf("program scan disabled: %+v", uses)
							}
							continue
						}
						if len(uses) != 1 || uses[0].PID != uint32(child.Process.Pid) || uses[0].RootProgram != program || uses[0].ShutdownHelper {
							t.Fatalf("wrong process ownership: %+v", uses)
						}
					}
					if program {
						image := filepath.Join(imageRoot, filepath.FromSlash(rel))
						if !u.shutdownHelper(image, []string{image, "__bootwatch", imageRoot, "0", "123", "456"}) {
							t.Error("same root's bootwatch was not recognized")
						}
						if u.shutdownHelper(image, []string{image, "__bootwatch", t.TempDir(), "0", "123", "456"}) {
							t.Error("another root's bootwatch was recognized")
						}
					}
				})
			}
		}
	}
}

func TestPathAliasSDKInstall(t *testing.T) {
	u, _ := imageUpdate(t)
	alias := updatePathAlias(t, u.Root.Dir, "junction")
	u.Root = &root.Root{Dir: alias}
	u.l.root = alias
	u.scan = ownScan(u)
	if st, err := u.Install(context.Background()); err != nil || !st.Updated || st.Phase != phaseIdle {
		t.Fatalf("install through junction: %+v, %v", st, err)
	}
}

func TestPathAliasReadOnlyLoadedImage(t *testing.T) {
	dir := testutil.TempDir(t)
	alias := updatePathAlias(t, dir, "junction")
	path := filepath.Join(dir, "loaded.dll")
	free := loadDLL(t, filepath.Join(alias, "loaded.dll"))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o666) })
	for _, probe := range []string{path, filepath.Join(alias, "loaded.dll")} {
		if !loadedImageInUse(probe) {
			t.Error("loaded image query missed through junction")
		}
		if !fileInUse(probe) {
			t.Error("read-only loaded image missed through junction")
		}
	}
	free()
	if fileInUse(path) {
		t.Error("unloaded read-only image is still busy")
	}
}

func TestPathAliasDifferentRoot(t *testing.T) {
	r := writeRoot(t, 36, nil)
	alias := updatePathAlias(t, r.Dir, "junction")
	u := New(&root.Root{Dir: alias}, nil, Options{})
	other := filepath.Join(testutil.TempDir(t), "tools", "modavd.exe")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(self, other); err != nil {
		t.Fatal(err)
	}
	occupancyChild(t, other)
	if uses, err := u.scanProcesses(true, ownProcesses, win.Process.ImagePath); err != nil || len(uses) != 0 {
		t.Fatalf("another root's program matched: %+v, %v", uses, err)
	}
	if u.shutdownHelper(other, []string{other, "__bootwatch", alias, "0", "123", "456"}) {
		t.Fatal("another root's program was treated as a shutdown helper")
	}
}

func TestUpdatePathResolutionFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := resolvePath(missing); err == nil {
		t.Fatal("missing path resolved successfully")
	}
	if got := realOrSame(missing); got != missing {
		t.Fatalf("missing path fallback changed: %q", got)
	}
	if _, err := resolvePath("invalid\x00path"); err == nil {
		t.Fatal("invalid path resolved successfully")
	}
}
