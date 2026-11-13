//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
)

func TestPrepareHandoffOffline(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	st, err := u.PrepareHandoff(context.Background())
	if err != nil || st.PreparedVersion != "v0.37.0" {
		t.Fatalf("valid newer stage must remain usable offline: %+v, %v", st, err)
	}
	if st.CurrentVersion != "v0.36.0" || st.CheckError == "" || len(st.Notices) != 1 || st.Updated {
		t.Fatalf("offline preparation lost its status: %+v", st)
	}
	// Retry uses the same verified stage; install still rechecks it and actually swaps the package offline.
	if _, err := u.PrepareHandoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = u.Install(context.Background())
	if err != nil || !st.Updated || st.CurrentVersion != "v0.37.0" || st.Phase != phaseIdle {
		t.Fatalf("offline install: %+v, %v", st, err)
	}
}

func TestPrepareHandoffOfflineRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, u *Updater)
		code string
	}{
		{"no stage", func(t *testing.T, u *Updater) {
			if err := u.l.abortStaging("test"); err != nil {
				t.Fatal(err)
			}
		}, errs.UpdateUnavailable},
		{"changed file", func(t *testing.T, u *Updater) {
			if err := os.WriteFile(filepath.Join(u.l.stageEmulator(), "emulator.exe"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, errs.UpdateRejected},
		{"missing marker", func(t *testing.T, u *Updater) {
			if err := os.Remove(u.l.stagedMarker()); err != nil {
				t.Fatal(err)
			}
		}, errs.UpdateRejected},
		{"equal version", func(t *testing.T, u *Updater) { setInstalledHandoffVersion(t, u, "v0.37.0") }, errs.UpdateUnavailable},
		{"older stage", func(t *testing.T, u *Updater) { setInstalledHandoffVersion(t, u, "v0.38.0") }, errs.UpdateUnavailable},
		{"unknown installed version", func(t *testing.T, u *Updater) {
			if err := os.Remove(filepath.Join(u.Root.Dir, manifestName)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(u.l.emulator(), "source.properties"), []byte("Pkg.BuildId=mas-999\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, errs.UpdateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			u := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false)).updater(writeRoot(t, 36, nil))
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			tc.edit(t, u)
			u.githubAPI += "/offline" // local fixture answers 404; no real source is contacted
			st, err := u.PrepareHandoff(context.Background())
			if errs.CodeOf(err) != tc.code || st.Updated {
				t.Fatalf("status %+v, error %v, want %s", st, err, tc.code)
			}
			if tc.code == errs.UpdateRejected && st.Phase != phaseIdle {
				t.Fatalf("bad stage was retained: %+v", st)
			}
		})
	}
}

func setInstalledHandoffVersion(t *testing.T, u *Updater, version string) {
	t.Helper()
	data := strings.ReplaceAll(string(manifestFor(36, pkgFiles(36, nil))), "v0.36.0", version)
	if err := os.WriteFile(filepath.Join(u.Root.Dir, manifestName), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareHandoffKeepsCancellationAndOtherErrors(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	u := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false)).updater(writeRoot(t, 36, nil))
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.PrepareHandoff(ctx); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("canceled: %v", err)
	}
	u.Opt.AutoMASVersion = "1.0.0"
	if _, err := u.PrepareHandoff(context.Background()); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("version requirement: %v", err)
	}
	u.Opt.AutoMASVersion = ""
	lock, err := u.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, err := u.PrepareHandoff(context.Background()); errs.CodeOf(err) != errs.Busy {
		t.Fatalf("update lock: %v", err)
	}
}
