// Copyright 2026 Ji Mao Cai
// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTempDirOwnership(t *testing.T) {
	var first, second string
	t.Run("allocate", func(t *testing.T) {
		first, second = TempDir(t), TempDir(t)
		if first == second || filepath.Dir(first) != filepath.Clean(os.TempDir()) {
			t.Fatalf("temporary directories are not unique allocations under os.TempDir: %q %q", first, second)
		}
		for _, dir := range []string{first, second} {
			if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, dir := range []string{first, second} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup left %q: %v", dir, err)
		}
	}
}

func TestMkdirTempCustomParent(t *testing.T) {
	parent := TempDir(t)
	var child string
	t.Run("allocate", func(t *testing.T) {
		child = MkdirTemp(t, parent, "fixture-")
		if filepath.Dir(child) != parent {
			t.Fatalf("custom parent was ignored: %q", child)
		}
	})
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("custom parent cleanup left entries: %v %v", entries, err)
	}
}

func TestCleanupDoesNotRetryUnrelatedErrors(t *testing.T) {
	want := errors.New("fixture failure")
	attempts := 0
	err := removeAll("unused", time.Second, func(string) error {
		attempts++
		return want
	})
	if err != want || attempts != 1 {
		t.Fatalf("unrelated error: %v, attempts %d", err, attempts)
	}
}
