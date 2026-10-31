//go:build windows

package emu

import (
	"context"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/win"
)

// qemuScanRecorder makes the process scan find nothing and records the indexes it is asked for, so a test sees that
// a call looked through its Manager (m.qemuOf goes through qemuScan) without any qemu or adb.
func qemuScanRecorder(t *testing.T) *[][]int {
	t.Helper()
	asked := &[][]int{}
	saved := qemuScan
	qemuScan = func(indexes []int) map[int]win.Process {
		*asked = append(*asked, slices.Clone(indexes))
		return map[int]win.Process{}
	}
	t.Cleanup(func() { qemuScan = saved })
	return asked
}

// opsTestInstance is a Manager on a fresh root with one instance, and that instance's index.
func opsTestInstance(t *testing.T) (*Manager, int) {
	t.Helper()
	m, _ := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	return m, created.Index
}

// Shell on an instance whose qemu does not run is Unsupported, decided by the Manager's own process scan before any
// adb command.
func TestShellNotRunning(t *testing.T) {
	m, i := opsTestInstance(t)
	asked := qemuScanRecorder(t)
	_, err := m.Shell(context.Background(), i, "getprop ro.build.version.sdk", 0)
	if errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "没有在运行") {
		t.Fatalf("shell: %v", err)
	}
	if len(*asked) != 1 || !slices.Equal((*asked)[0], []int{i}) {
		t.Fatalf("scans = %v", *asked)
	}

	if _, err := m.Shell(context.Background(), i, "  ", 0); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("empty command: %v", err)
	}
	if _, err := m.Shell(context.Background(), 5, "id", 0); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("index 5: %v", err)
	}
	if len(*asked) != 1 {
		t.Fatalf("invalid calls scanned: %v", *asked)
	}
}
