package emu

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

func injectQemuSnapshotError(t *testing.T, err error) *int {
	t.Helper()
	snapshot, scan := listQemuProcesses, qemuScan
	t.Cleanup(func() { listQemuProcesses, qemuScan = snapshot, scan })
	qemuScan = nil
	calls := 0
	listQemuProcesses = func() ([]win.Process, error) { calls++; return nil, err }
	return &calls
}

func TestQemuScanErrors(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	queries := map[string]func() error{
		"scanQemuOf": func() error { _, err := scanQemuOf([]*root.Root{r}, []int{1}); return err },
		"scanQemu":   func() error { _, err := m.scanQemu([]int{1}); return err },
		"qemus":      func() error { _, err := m.qemus([]int{1}); return err },
		"qemuOf":     func() error { _, _, err := m.qemuOf(1); return err },
		"findQemu":   func() error { _, _, err := m.findQemu(1); return err },
	}
	for _, failure := range []struct {
		name string
		err  error
		want bool
	}{
		{"unsupported", fmt.Errorf("snapshot: %w", errs.New(errs.Unsupported, "test host API unsupported")), true},
		{"ordinary failure", errors.New("test snapshot failed"), false},
	} {
		for name, query := range queries {
			t.Run(failure.name+"/"+name, func(t *testing.T) {
				calls := injectQemuSnapshotError(t, failure.err)
				err := query()
				if *calls != 1 {
					t.Fatalf("snapshot calls=%d, want 1", *calls)
				}
				if failure.want {
					if !errors.Is(err, failure.err) || errs.CodeOf(err) != errs.Unsupported {
						t.Fatalf("lost snapshot unsupported: %v", err)
					}
				} else if err != nil {
					t.Fatalf("ordinary snapshot failure changed behavior: %v", err)
				}
			})
		}
	}
}

func TestQemuWaitsPropagateUnsupported(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	failure := errs.New(errs.Unsupported, "test snapshot unsupported")
	injectQemuSnapshotError(t, failure)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"waitQemuGone": func() error { return m.waitQemuGone(ctx, 1) },
		"stopAndWait":  func() error { return m.stopAndWait(ctx, 1) },
		"waitBoot": func() error {
			_, err := m.waitBoot(ctx, 1, 0, nil, nil, "", nil, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, failure) {
				t.Fatalf("lost snapshot unsupported: %v", err)
			}
		})
	}
}
