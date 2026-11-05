// Copyright 2026 Ji Mao Cai
// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCleanupRetriesWindowsTransientErrors(t *testing.T) {
	for _, code := range []syscall.Errno{syscall.ERROR_DIR_NOT_EMPTY, syscall.ERROR_ACCESS_DENIED, syscall.Errno(32)} {
		t.Run(code.Error(), func(t *testing.T) {
			dir := TempDir(t)
			// Error 145 was observed on an already empty directory. The other
			// cases also cover retrying removal of actual file contents.
			if code != syscall.ERROR_DIR_NOT_EMPTY {
				if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			attempts := 0
			err := removeAll(dir, time.Second, func(path string) error {
				attempts++
				if attempts == 1 {
					return &os.PathError{Op: "unlinkat", Path: path, Err: code}
				}
				return os.RemoveAll(path)
			})
			if err != nil || attempts < 2 {
				t.Fatalf("transient error was not retried: %v, attempts %d", err, attempts)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup reported success with fixture remaining: %v", err)
			}
		})
	}
}

func TestCleanupPersistentErrorIsBounded(t *testing.T) {
	want := &os.PathError{Op: "unlinkat", Path: "fixture", Err: syscall.ERROR_DIR_NOT_EMPTY}
	attempts := 0
	err := removeAll("unused", 20*time.Millisecond, func(string) error {
		attempts++
		return want
	})
	if err != want || attempts < 2 || attempts > 100 {
		t.Fatalf("persistent error: %v, attempts %d", err, attempts)
	}
}

func TestCleanupDoesNotHideLiveHandles(t *testing.T) {
	dir := TempDir(t)
	path := filepath.Join(dir, "open-file")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = removeAll(dir, 20*time.Millisecond, os.RemoveAll)
	if !errors.Is(err, syscall.Errno(32)) { // ERROR_SHARING_VIOLATION
		t.Fatalf("live handle was not reported: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live file changed: %v", err)
	}
}
