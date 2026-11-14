// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/win"
)

func managerImageUpdate(t *testing.T) *Updater {
	t.Helper()
	u, image := imageUpdate(t)
	if err := os.WriteFile(u.l.at("ModAVD.exe"), image, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, map[string]string{"ModAVD.exe": string(image)}), false))
	u.githubAPI, u.httpClient = f.srv.URL+"/gh", f.srv.Client()
	return u
}

func TestManagerImageGateCoversSwapAndRecovery(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "recover"}[recover], func(t *testing.T) {
			u := managerImageUpdate(t)
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			if recover {
				u.cutSwap(t, 1)
			}
			meta, free, err := win.TryLock(u.Root.MetadataPath() + ".lock")
			if err != nil || !free {
				t.Fatalf("metadata lock: %v %v", free, err)
			}
			defer meta.Unlock()
			var result *Status
			var installErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				if recover {
					result, installErr = u.Recover(context.Background())
				} else {
					result, installErr = u.InstallHandoff(context.Background())
				}
			}()
			t.Cleanup(func() {
				meta.Unlock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("update did not finish")
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for isFile(u.l.pkgAt("ModAVD.exe")) || !isFile(u.l.prevAt("ModAVD.exe")) {
				select {
				case <-done:
					t.Fatalf("update ended before commit: %+v %v", result, installErr)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("manager replacement did not complete")
				}
				time.Sleep(10 * time.Millisecond)
			}
			assertImageBlocked(t, u.l.at("ModAVD.exe"))
			assertImageBlocked(t, u.l.prevAt("ModAVD.exe"))
			meta.Unlock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("commit did not finish")
			}
			if installErr != nil || result.Phase != phaseIdle {
				t.Fatalf("guarded manager replacement: %+v %v", result, installErr)
			}
			assertImageStarts(t, u.l.at("ModAVD.exe"))
		})
	}
}

func TestLoadedManagerImageBlocksBeforeSwapAndCanRetry(t *testing.T) {
	u := managerImageUpdate(t)
	p := assertImageStarts(t, u.l.at("ModAVD.exe"))
	st, err := u.InstallHandoff(context.Background())
	if errs.CodeOf(err) != errs.Busy || st.Phase != phaseStaged || st.Updated {
		t.Fatalf("loaded manager: %+v %v", st, err)
	}
	assertStartLocks(t, u.Root, false)
	p.Kill()
	p.Wait()
	if st, err = u.InstallHandoff(context.Background()); err != nil || !st.Updated {
		t.Fatalf("retry: %+v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(u.Root.Dir, "avd", "mas_6.avd", "userdata-qemu.img")); err != nil {
		t.Fatal(err)
	}
}
