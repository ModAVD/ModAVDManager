//go:build windows

package emu

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
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

func TestSetWindowVisibleHostErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"ordinary failure", errors.New("test window failed")},
		{"unsupported", errs.New(errs.Unsupported, "test window unsupported")},
	} {
		for _, visible := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/visible=%v", tc.name, visible), func(t *testing.T) {
				apiErr := tc.err
				m := New(managerTestRoot(t, 1), nil)
				logs := &logx.Collector{}
				m.Log = logx.New(logs.Sink())
				scan, set := qemuScan, setEmulatorWindowsVisible
				t.Cleanup(func() { qemuScan, setEmulatorWindowsVisible = scan, set })
				qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{1: {PID: 4242}} }
				called := false
				setEmulatorWindowsVisible = func(pid uint32, show bool) (int, error) {
					called = true
					if pid != 4242 || show != visible {
						t.Fatalf("pid=%d show=%v", pid, show)
					}
					return 2, apiErr
				}
				res, err := m.SetWindowVisible(context.Background(), 1, visible)
				if !called {
					t.Fatal("window API was not called")
				}
				if errs.CodeOf(apiErr) == errs.Unsupported {
					if !errors.Is(err, apiErr) || res != nil {
						t.Fatalf("lost window unsupported: result=%+v error=%v", res, err)
					}
				} else {
					want := 2
					if apiErr != nil {
						want = 0
					}
					if err != nil || res == nil || res.Windows != want || res.Visible != visible {
						t.Fatalf("result=%+v error=%v", res, err)
					}
					if apiErr != nil && len(logs.Messages()) != 1 {
						t.Fatalf("warnings=%v", logs.Messages())
					}
				}
			})
		}
	}
}
