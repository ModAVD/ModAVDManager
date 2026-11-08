//go:build windows

package update

import (
	"os"

	"modavd/internal/win"
)

// ownProcesses is win.ListProcesses narrowed to this test binary and the processes it started (and theirs). go test
// ./... runs packages at once, and other packages' tests start fake emulators of their own (qemu-system-x86_64-
// headless.exe and the like, in their temp roots); while one of them exits its path cannot be read, and the real scan
// rightly counts an unreadable qemu-system* as a possible occupant. The tests here must see only their own fixtures.
func ownProcesses() ([]win.Process, error) {
	all, err := win.ListProcesses()
	if err != nil {
		return nil, err
	}
	mine := map[uint32]bool{uint32(os.Getpid()): true}
	for grew := true; grew; {
		grew = false
		for _, p := range all {
			if !mine[p.PID] && p.PID != 0 && mine[p.PPID] {
				mine[p.PID] = true
				grew = true
			}
		}
	}
	var out []win.Process
	for _, p := range all {
		if mine[p.PID] {
			out = append(out, p)
		}
	}
	return out, nil
}

// ownScan is the real scan (scanProcesses with the native image paths) over ownProcesses.
func ownScan(u *Updater) func(bool) ([]processUse, error) {
	return func(programs bool) ([]processUse, error) {
		return u.scanProcesses(programs, ownProcesses, win.Process.ImagePath)
	}
}
