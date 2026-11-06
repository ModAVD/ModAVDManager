//go:build darwin && cgo

package host

import (
	"os"
	"sync/atomic"
	"time"

	"modavd/internal/macos"
)

// Keep the existing host API for callers; macOS owns the implementation and
// process methods, including the single child reaper and exit-status cache.
type Process = macos.Process
type Listener = macos.Listener
type StartOptions = macos.StartOptions
type FileLock = macos.FileLock

const SpawnHelperArg = macos.SpawnHelperArg

var orphanHelper atomic.Pointer[string]

// SetOrphanHelper selects the executable implementing the __spawn protocol.
func SetOrphanHelper(path string) { orphanHelper.Store(&path) }

func ListProcesses() ([]Process, error)      { return macos.ListProcesses() }
func FindProcess(pid uint32) (Process, bool) { return macos.FindProcess(pid) }
func ProcessName(pid uint32) string          { return macos.ProcessName(pid) }
func KillTree(p Process) error               { return macos.KillTree(p) }

func StartDetached(exe string, args []string, opt StartOptions) (Process, error) {
	return macos.StartDetached(exe, args, opt)
}

func StartOrphan(exe string, args []string, opt StartOptions) (Process, error) {
	var helper string
	if path := orphanHelper.Load(); path != nil {
		helper = *path
	}
	return macos.StartOrphan(exe, args, opt, helper)
}

func SpawnHelperMain(args []string, stdout *os.File) int {
	return macos.SpawnHelperMain(args, stdout)
}

func TryLock(path string) (*FileLock, bool, error) { return macos.TryLock(path) }
func Lock(path string, timeout time.Duration) (*FileLock, bool, error) {
	return macos.Lock(path, timeout)
}

func Listeners() ([]Listener, error)             { return macos.Listeners() }
func ListenerPID(port int) (uint32, bool, error) { return macos.ListenerPID(port) }
func BusyPorts(ports []int) ([]int, error)       { return macos.BusyPorts(ports) }
