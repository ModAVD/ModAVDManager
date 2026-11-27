// Copyright 2026 Ji Mao Cai
// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TempDir allocates a test-owned directory under the default OS temporary directory.
func TempDir(t testing.TB) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, t.Name())
	if len(name) > 80 {
		name = name[:80]
	}
	return MkdirTemp(t, "", name+"-")
}

// MkdirTemp is os.MkdirTemp with cleanup owned by the test, including custom test drives.
func MkdirTemp(t testing.TB, parent, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := removeAll(dir, 2*time.Second, os.RemoveAll); err != nil {
			t.Errorf("TempDir RemoveAll cleanup: %v", err)
		}
	})
	return dir
}

func removeAll(path string, timeout time.Duration, remove func(string) error) error {
	err := remove(path)
	if !retryableRemove(err) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for pause := time.Millisecond; ; pause = min(2*pause, 50*time.Millisecond) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		time.Sleep(min(pause, remaining))
		err = remove(path)
		if !retryableRemove(err) {
			return err
		}
	}
}
