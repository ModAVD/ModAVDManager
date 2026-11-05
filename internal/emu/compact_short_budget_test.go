//go:build windows

package emu

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/errs"
)

// verify_compact_process.py uses go's file overlay to shorten only the budget
// calculation to three seconds. The real DiskCompact timeout/cleanup path is
// unchanged, and normal tests never wait for the production multi-hour budget.
func TestCompactBudgetShortDeadline(t *testing.T) {
	if os.Getenv("MODAVD_TEST_COMPACT_SHORT_BUDGET") != "1" {
		t.Skip("run scripts/verify_compact_process.py to exercise scaled compact deadlines")
	}
	for _, stage := range []string{"convert", "compare"} {
		t.Run(stage, func(t *testing.T) {
			m, path, overlay := compactFakeManager(t, compactFakeRequest{Hold: stage, OutputSize: 1 << 20})
			before, err := os.ReadFile(overlay)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := m.DiskCompact(context.Background(), 2); errs.CodeOf(err) != errs.Timeout || res != nil {
				t.Fatalf("expected compact timeout: %+v %v", res, err)
			}
			if b, _ := os.ReadFile(path + ".ready"); string(b) != stage {
				t.Fatal("helper never entered slow operation")
			}
			after, err := os.ReadFile(overlay)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("timeout changed original: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(overlay), userdataCompactTmp)); !os.IsNotExist(err) {
				t.Fatal("timeout left scratch image")
			}
			unlock, err := m.lockInstanceForCompact(2)
			if err != nil {
				t.Fatal(err)
			}
			unlock()
		})
	}
}
