//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"modavd/internal/errs"
)

func compatArchive(t *testing.T, files map[string]string, incremental, manifest bool, deleted []string) []byte {
	t.Helper()
	payload := merged(files)
	if manifest {
		payload[manifestName] = string(manifestFor(37, files))
	}
	if incremental {
		modified := make([]string, 0, len(payload))
		for name := range payload {
			modified = append(modified, name)
		}
		sort.Strings(modified)
		changes, err := json.Marshal(map[string][]string{"modified": modified, "deleted": deleted})
		if err != nil {
			t.Fatal(err)
		}
		payload["changes.json"] = string(changes)
	}
	return zipOf(t, 37, payload, true)
}

func stageCachedPackage(t *testing.T, data []byte, incremental bool, tag, lockTarget string, baseFiles map[string]string) (*Updater, string, error) {
	t.Helper()
	r := writeRoot(t, 36, baseFiles)
	u := New(r, nil, Options{})
	archive := u.l.downloadAt("cached.zip")
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rel := &release{source: SourceMirrorChyan, tag: tag, version: Version{0, 37, 0}, assetName: "cached.zip", url: "https://unused.invalid/cached.zip", size: int64(len(data)), sha256: hashBytes(data), incremental: incremental, base: Version{0, 36, 0}}
	var held syscall.Handle
	defer func() {
		if held != 0 {
			syscall.CloseHandle(held)
		}
	}()
	if lockTarget != "" {
		u.Opt.Progress = func(p Progress) {
			if p.Stage != "verifying" || held != 0 {
				return
			}
			name, err := syscall.UTF16PtrFromString(u.l.pkgAt(lockTarget))
			if err != nil {
				t.Fatal(err)
			}
			held, err = syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	err := u.stage(context.Background(), rel, &Status{})
	return u, archive, err
}

func TestRequiredPackageDirectoriesRejectAndDeleteCache(t *testing.T) {
	for _, mode := range []string{"full", "incremental", "legacy"} {
		for _, target := range []string{manifestName, "sdk/emulator/source.properties", "sdk/emulator/qemu/windows-x86_64/qemu-system-x86_64-headless.exe"} {
			t.Run(mode+"/"+target, func(t *testing.T) {
				files := pkgFiles(37, nil)
				delete(files, target)
				files[target+"/child"] = "directory supplied by the package"
				incremental := mode == "incremental"
				manifest := mode != "legacy" && target != manifestName
				tag := "v0.37.0"
				if mode == "legacy" {
					tag = "v37.2.10-mas37"
				}
				u, archive, err := stageCachedPackage(t, compatArchive(t, files, incremental, manifest, []string{target}), incremental, tag, "", nil)
				if mode == "legacy" && target == manifestName {
					if err != nil || stagedMode(u) != "full" || !lexists(archive) {
						t.Fatalf("optional legacy manifest directory changed behavior: %v", err)
					}
				} else if errs.CodeOf(err) != errs.UpdateRejected || lexists(archive) || phaseOf(u.l.readState()) != phaseIdle {
					t.Fatalf("package directory: error %v, code %s, cached %v, phase %s", err, errs.CodeOf(err), lexists(archive), phaseOf(u.l.readState()))
				}
			})
		}
	}
}

func TestPackageReadErrorsRetainFullAndIncrementalCache(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		for _, target := range []string{manifestName, "sdk/emulator/emulator.exe"} {
			name := "full/" + target
			if incremental {
				name = "incremental/" + target
			}
			t.Run(name, func(t *testing.T) {
				data := compatArchive(t, pkgFiles(37, nil), incremental, true, nil)
				u, archive, err := stageCachedPackage(t, data, incremental, "v0.37.0", target, nil)
				var pe *os.PathError
				if !errors.As(err, &pe) || errs.CodeOf(err) == errs.UpdateRejected || !lexists(archive) || lexists(u.l.stagedMarker()) {
					t.Fatalf("local read: %v, cached %v", err, lexists(archive))
				}
				// The read-denying handle is released before cleanup retries the parent-directory rename.
				if err := u.l.abortStaging("test read failure"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestWholeAndLegacyPackageSuccess(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name, tag := "full", "v0.37.0"
		if legacy {
			name, tag = "legacy", "v37.2.10-mas37"
		}
		t.Run(name, func(t *testing.T) {
			files := pkgFiles(37, nil)
			u, _, err := stageCachedPackage(t, compatArchive(t, files, false, !legacy, nil), false, tag, "", nil)
			if err != nil || stagedMode(u) != "full" {
				t.Fatalf("stage: %v", err)
			}
			if err := u.l.checkStaged(u.l.readState()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIncrementalPackageReusesAddsAndDeletes(t *testing.T) {
	files := pkgFiles(37, map[string]string{"tools/keep.ps1": "kept", "tools/added.ps1": "added"})
	payload := merged(files)
	delete(payload, "tools/keep.ps1")
	payload[manifestName] = string(manifestFor(37, files))
	modified := []string{manifestName}
	for name := range payload {
		if name != manifestName && name != "tools/added.ps1" {
			modified = append(modified, name)
		}
	}
	sort.Strings(modified)
	changes, err := json.Marshal(map[string][]string{"added": {"tools/added.ps1"}, "modified": modified, "deleted": {"tools/deleted.ps1"}})
	if err != nil {
		t.Fatal(err)
	}
	payload["changes.json"] = string(changes)
	u, _, err := stageCachedPackage(t, zipOf(t, 37, payload, true), true, "v0.37.0", "", map[string]string{"tools/keep.ps1": "kept", "tools/deleted.ps1": "old"})
	if err != nil || stagedMode(u) != "incremental" {
		t.Fatalf("stage: %v", err)
	}
	for name, want := range files {
		path := u.l.pkgAt(name)
		if suffix, ok := strings.CutPrefix(name, "sdk/emulator/"); ok {
			path = filepath.Join(u.l.stageEmulator(), filepath.FromSlash(suffix))
		}
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("%s: %q %v", name, body, err)
		}
	}
	if lexists(u.l.pkgAt("tools/deleted.ps1")) || !lexists(u.l.at("tools/deleted.ps1")) {
		t.Fatal("incremental deletion or installed-root isolation failed")
	}
	if err := u.l.checkStaged(u.l.readState()); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorPackageDirectoryFallsBackToGitHub(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		name, updateType := "full", "full"
		if incremental {
			name, updateType = "incremental", "incremental"
		}
		t.Run(name, func(t *testing.T) {
			withSettings(t, SourceMirrorChyan, "test-cdk")
			target := "sdk/emulator/source.properties"
			badFiles := pkgFiles(37, nil)
			delete(badFiles, target)
			badFiles[target+"/child"] = "not a properties file"
			bad := compatArchive(t, badFiles, incremental, true, []string{target})
			good := zipOf(t, 37, pkgFiles(37, nil), false)
			f := newSources(t, 37, good)
			f.mirror = func(map[string]string) (int, any) {
				return 0, map[string]any{"version_name": "v0.37.0", "url": f.srv.URL + "/dl/bad.zip", "filesize": len(bad), "sha256": hashBytes(bad), "update_type": updateType}
			}
			log := &dlLog{}
			serve := log.serve(good, nil)
			f.dl = func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/bad.zip") {
					http.ServeContent(w, r, "bad.zip", zipTime, bytes.NewReader(bad))
					return
				}
				serve(w, r)
			}
			u := f.updater(writeRoot(t, 36, nil))
			st, err := u.Prepare(context.Background())
			if err != nil || st.Phase != phaseStaged || stagedMode(u) != "full" || log.fullGets() != 1 || len(st.Notices) != 1 || !strings.Contains(st.Notices[0], "Mirror 酱") {
				t.Fatalf("fallback: %+v, error %v, full GETs %d", st, err, log.fullGets())
			}
		})
	}
}
