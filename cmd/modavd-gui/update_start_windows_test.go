// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/root"
	"modavd/internal/update"
)

func TestManagerLaunchDuringHandoffDoesNotTakeInstanceLock(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.MetadataPath(), []byte(`{"portBase":34000,"adbServerPort":34098,"scriptAdbServerPort":34099,"shareDefaultAdbServer":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id := update.HandoffID(strings.Repeat("a", 32))
	g, err := update.BeginHandoff(r, id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	errors, _ := showBoxes(t)
	oldNotice := updatingNotice
	t.Cleanup(func() { updatingNotice = oldNotice })
	notices := 0
	updatingNotice = func() error { notices++; return nil }
	for _, noWindow := range []bool{false, true} {
		args := []string{"--root", r.Dir, "--port", "34097"}
		if noWindow {
			args = append(args, "--no-window")
		}
		if code := run(args); code != 0 {
			t.Fatalf("duplicate launch exit %d", code)
		}
	}
	if notices != 1 || len(*errors) != 0 {
		t.Fatalf("notices %d, errors %v", notices, *errors)
	}
	if _, err := os.Stat(lockPath(r)); !os.IsNotExist(err) {
		t.Fatal("duplicate took manager lock before checking handoff")
	}
	if _, err := os.Stat(statePath(r)); !os.IsNotExist(err) {
		t.Fatal("duplicate wrote manager state")
	}
	b, err := os.ReadFile(filepath.Join(r.LogsDir(), logFileName))
	if err != nil || !strings.Contains(string(b), update.UpdatingMessage) {
		t.Fatalf("missing update notice in log: %v", err)
	}
	if !update.HandoffReady(r, id) {
		t.Fatal("duplicate disturbed owner")
	}
}
