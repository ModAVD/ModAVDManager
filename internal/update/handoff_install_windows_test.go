// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/errs"
)

func TestInstallHandoffNewerStageDoesNotCheckRemote(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(writeRoot(t, 36, nil))
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.requests.Load()
	// A remote version newer than the accepted stage must not change the handoff.
	f.gh = 38
	st, err := u.InstallHandoff(context.Background())
	if err != nil || !st.Updated || st.CurrentVersion != "v0.37.0" || f.requests.Load() != before {
		t.Fatalf("handoff checked remote or did not install accepted stage: %+v, %v, requests %d -> %d", st, err, before, f.requests.Load())
	}
}

func TestInstallHandoffRevalidatesStageAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*testing.T, *Updater)
		cancel bool
		code   string
	}{
		{"changed file", func(t *testing.T, u *Updater) {
			if err := os.WriteFile(filepath.Join(u.l.stageEmulator(), "emulator.exe"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, errs.UpdateRejected},
		{"missing marker", func(t *testing.T, u *Updater) {
			if err := os.Remove(u.l.stagedMarker()); err != nil {
				t.Fatal(err)
			}
		}, false, errs.UpdateRejected},
		{"cancel", func(*testing.T, *Updater) {}, true, errs.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(writeRoot(t, 36, nil))
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := f.requests.Load()
			tc.edit(t, u)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			st, err := u.InstallHandoff(ctx)
			if errs.CodeOf(err) != tc.code || st.Updated || st.CurrentVersion != "v0.36.0" || f.requests.Load() != before {
				t.Fatalf("refusal: %+v %v requests %d -> %d", st, err, before, f.requests.Load())
			}
		})
	}
}

func TestInstallHandoffWithoutNewerStageKeepsOrdinaryPreparation(t *testing.T) {
	for _, current := range []int{0, 37, 38} {
		t.Run(fmt.Sprint(current), func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(writeRoot(t, 36, nil))
			if current > 0 {
				if _, err := u.Prepare(context.Background()); err != nil {
					t.Fatal(err)
				}
				setInstalledHandoffVersion(t, u, fmt.Sprintf("v0.%d.0", current))
				f = newSources(t, 39, zipOf(t, 39, pkgFiles(39, nil), false))
				u.githubAPI, u.httpClient = f.srv.URL+"/gh", f.srv.Client()
			}
			before := f.requests.Load()
			st, err := u.InstallHandoff(context.Background())
			if err != nil || !st.Updated || f.requests.Load() == before {
				t.Fatalf("ordinary prepare was skipped: %+v %v", st, err)
			}
		})
	}
}

func TestOrdinaryInstallStillChecksLatestWithNewerStage(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(writeRoot(t, 36, nil))
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	f = newSources(t, 38, zipOf(t, 38, pkgFiles(38, nil), false))
	u.githubAPI, u.httpClient = f.srv.URL+"/gh", f.srv.Client()
	st, err := u.Install(context.Background())
	if err != nil || !st.Updated || st.CurrentVersion != "v0.38.0" || f.requests.Load() == 0 {
		t.Fatalf("ordinary install semantics changed: %+v %v", st, err)
	}
}
