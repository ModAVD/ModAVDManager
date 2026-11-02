//go:build !windows

// Package win is the platform boundary used by the rest of ModAVD.  The
// non-Windows implementation deliberately exposes the same API while
// returning errs.Unsupported for operations that need a host implementation.
package win

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"modavd/internal/errs"
)

var errUnsupported = errs.New(errs.Unsupported, "当前平台暂不支持此操作")

type Process struct {
	PID     uint32
	PPID    uint32
	Name    string
	Created uint64
}

type Listener struct {
	Port int
	PID  uint32
}

type StartOptions struct {
	Dir        string
	Env        []string
	Output     *os.File
	OutputPath string
}

var OrphanHelper string

const SpawnHelperArg = "__spawn"
const HideConsole = 0

type FileLock struct{}

// HideConsoleCmd is a no-op because non-Windows has no CREATE_NO_WINDOW flag.
func HideConsoleCmd(*exec.Cmd) {}

// ListProcesses uses CreateToolhelp32Snapshot and Process32First/Next on Windows.
func ListProcesses() ([]Process, error) { return nil, errUnsupported }

// FindProcess resolves a PID from the Windows process snapshot and checks its identity.
func FindProcess(uint32) (Process, bool) { return Process{}, false }

// ProcessName reads the executable name from the Windows process snapshot.
func ProcessName(uint32) string { return "" }

// CommandLine opens the Windows process and reads its command line from the PEB.
func (p Process) CommandLine() ([]string, error) { return nil, errUnsupported }

// ImagePath calls QueryFullProcessImageNameW on Windows.
func (p Process) ImagePath() (string, error) { return "", errUnsupported }

// StartedAt is unavailable without the Windows process APIs. Returning zero
// prevents a fabricated Unix timestamp from being reported as a process start.
func (p Process) StartedAt() time.Time {
	return time.Time{}
}

// Alive compares the Windows process identity and checks its exit state.
func (p Process) Alive() bool { return false }

// Wait uses WaitForSingleObject on the Windows process handle.
func (p Process) Wait(time.Duration) bool { return true }

// ExitCode calls GetExitCodeProcess on Windows.
func (p Process) ExitCode() (uint32, bool) { return 0, false }

// CPUTime reads the Windows process kernel and user FILETIMEs.
func (p Process) CPUTime() (time.Duration, bool) { return 0, false }

// Kill calls TerminateProcess on Windows.
func (p Process) Kill() error { return errUnsupported }

// KillTree enumerates child processes and terminates the Windows process tree.
func KillTree(Process) error { return errUnsupported }

// StartDetached creates a Windows process with detached-console flags.
func StartDetached(string, []string, StartOptions) (Process, error) { return Process{}, errUnsupported }

// StartOrphan starts the Windows helper that adopts the child after its parent exits.
func StartOrphan(string, []string, StartOptions) (Process, error) { return Process{}, errUnsupported }

// SpawnHelperMain is the Windows orphan helper's entry point.
func SpawnHelperMain([]string, *os.File) int { return 1 }

// TryLock takes a non-blocking Windows LockFileEx lock.
func TryLock(string) (*FileLock, bool, error) { return nil, false, errUnsupported }

// Lock retries Windows LockFileEx until the timeout.
func Lock(string, time.Duration) (*FileLock, bool, error) { return nil, false, errUnsupported }

// Unlock releases the Windows LockFileEx handle.
func (l *FileLock) Unlock() {}

// Listeners reads the Windows TCP table with GetExtendedTcpTable.
func Listeners() ([]Listener, error) { return nil, errUnsupported }

// ListenerPID finds the owning PID in the Windows TCP table.
func ListenerPID(int) (uint32, bool, error) { return 0, false, errUnsupported }

// BusyPorts compares requested ports with the Windows TCP and UDP tables.
func BusyPorts([]int) ([]int, error) { return nil, errUnsupported }

// SetAboveNormalPriority calls SetPriorityClass with ABOVE_NORMAL_PRIORITY_CLASS.
func SetAboveNormalPriority(Process) error { return errUnsupported }

// SetAffinity calls SetProcessAffinityMask on Windows.
func SetAffinity(Process, uint64) error { return errUnsupported }

// GetAffinity calls GetProcessAffinityMask on Windows.
func GetAffinity(Process) (uint64, error) { return 0, errUnsupported }

// L3Group is one L3 cache: its size in bytes and the logical processors sharing it.
type L3Group struct {
	Size uint32
	Mask uint64
}

// L3Groups lists the L3 caches of the host.
func L3Groups() []L3Group { return nil }

// SharedMemoryExists calls OpenFileMappingW for the Windows named mapping.
func SharedMemoryExists(string) bool { return false }

// DiskFree calls GetDiskFreeSpaceExW for the Windows volume.
func DiskFree(string) (uint64, error) { return 0, errUnsupported }

// AvailableMemoryMB calls GlobalMemoryStatusEx on Windows.
func AvailableMemoryMB() (uint64, error) { return 0, errUnsupported }

// Protect calls CryptProtectData (DPAPI) for the current Windows user.
func Protect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }

// Unprotect calls CryptUnprotectData (DPAPI) for the current Windows user.
func Unprotect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }

// SetEmulatorWindowsVisible finds the emulator window and calls ShowWindow on Windows.
func SetEmulatorWindowsVisible(uint32, string, bool) (int, error) { return 0, errUnsupported }

// GetConsoleWindow calls the Windows GetConsoleWindow API.
func GetConsoleWindow() uintptr { return 0 }

// MessageBox calls MessageBoxW from Windows user32.
func MessageBox(string) error { return errors.New("当前平台暂不支持消息框") }
