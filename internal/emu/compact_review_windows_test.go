//go:build windows

package emu

import (
	"context"
	"testing"
)

// The process verification script also runs this with syncFile growing the
// temporary file after flushing. That catches measuring before sync completes.
func TestCompactBudgetMeasuresAfterSync(t *testing.T) {
	m, _, overlay := compactFakeManager(t, compactFakeRequest{OutputSize: 33 << 20})
	res, err := m.DiskCompact(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := compactHostBytes(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.AfterOnDiskBytes != actual || res.OnDiskDeltaBytes != actual-res.BeforeOnDiskBytes {
		t.Fatalf("reported allocation differs from flushed image: result=%+v actual=%d", res, actual)
	}
}
