//go:build windows

package emu

import (
	"context"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
)

// TestFreeIndexes: a batch create's indexes are the smallest free ones in order, skipping instances; asking for more
// than are free is refused, saying how many are (ModAVD/ModAVD#240).
func TestFreeIndexes(t *testing.T) {
	m, _ := opsTestManager(t)
	two := 2
	_, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &two, NoBridge: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.FreeIndex()
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.FreeIndexes(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != first || !slices.IsSorted(got) || slices.Contains(got, 2) || slices.Contains(got, 5) ||
		len(slices.Compact(slices.Clone(got))) != 3 {
		t.Fatalf("FreeIndexes(3) = %v (FreeIndex %d)", got, first)
	}
	// nine valid indexes, one taken: at most eight
	_, err = m.FreeIndexes(root.MaxIndex)
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(errs.As(err).Message, "建不了 9 台") {
		t.Fatalf("FreeIndexes(9) = %v", err)
	}
}

// TestCreateSpaceError: the batch create's disk check is check's 磁盘 item, on the drive of the instances.
func TestCreateSpaceError(t *testing.T) {
	m := diskTestManager(t, minFreeDiskGBToBoot<<30)
	if err := m.CreateSpaceError(); err != nil {
		t.Fatalf("enough space: %v", err)
	}
	m = diskTestManager(t, 3<<30)
	err := m.CreateSpaceError()
	e := errs.As(err)
	if errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(e.Message, "剩余 3.0 GB") || e.Advice == "" {
		t.Fatalf("low space: %v", err)
	}
}
