//go:build windows

package emu

import (
	"context"
	"slices"
	"testing"

	"modavd/internal/errs"
)

// An instance whose qemu does not run has no window to show or hide: zero windows, no error, no warning (scripts call
// this every round); the running check is the Manager's own process scan.
func TestSetWindowVisibleNotRunning(t *testing.T) {
	m, logs := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	asked := qemuScanRecorder(t)
	for _, visible := range []bool{true, false} {
		res, err := m.SetWindowVisible(context.Background(), i, visible)
		if err != nil || res.Index != i || res.Visible != visible || res.Windows != 0 {
			t.Fatalf("visible %v: %+v %v", visible, res, err)
		}
	}
	if len(*asked) != 2 || !slices.Equal((*asked)[0], []int{i}) || !slices.Equal((*asked)[1], []int{i}) {
		t.Fatalf("scans = %v", *asked)
	}
	if w := logs.Messages(); len(w) != 0 {
		t.Fatalf("warnings: %q", w)
	}
	if _, err := m.SetWindowVisible(context.Background(), 11, true); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("index 11: %v", err)
	}
}
