//go:build !windows

package host

import (
	"os"
	"os/exec"

	"modavd/internal/errs"
)

var errUnsupported = errs.New(errs.Unsupported, "当前平台暂不支持此操作")

const HideConsole = 0

// L3Group is the cache topology exposed by the host interface.
type L3Group struct {
	Size uint32
	Mask uint64
}

// HideConsoleCmd is a no-op on hosts without a Windows console.
func HideConsoleCmd(*exec.Cmd) {}

// SetAboveNormalPriority calls SetPriorityClass with ABOVE_NORMAL_PRIORITY_CLASS.
func SetAboveNormalPriority(Process) error { return errUnsupported }

// SetAffinity calls SetProcessAffinityMask on Windows.
func SetAffinity(Process, uint64) error { return errUnsupported }

// GetAffinity calls GetProcessAffinityMask on Windows.
func GetAffinity(Process) (uint64, error) { return 0, errUnsupported }

// GetProcessMemory calls GetProcessMemoryInfo on Windows.
func GetProcessMemory(Process) (ProcessMemory, error) { return ProcessMemory{}, errUnsupported }

// GetSystemMemory calls GlobalMemoryStatusEx on Windows.
func GetSystemMemory() (SystemMemory, error) { return SystemMemory{}, errUnsupported }

// L3Groups lists the L3 caches of the host.
func L3Groups() []L3Group { return nil }

// CoreClass is one efficiency class of the host's cores (Windows EfficiencyClass).
type CoreClass struct {
	Class   uint8
	Cores   int
	Logical int
}

// CoreClasses lists the efficiency classes of the host's cores on Windows.
func CoreClasses() []CoreClass { return nil }

// DisableExecutionThrottling calls SetProcessInformation(ProcessPowerThrottling) on Windows.
func DisableExecutionThrottling(Process) error { return errUnsupported }

// SharedMemoryExists calls OpenFileMappingW for the Windows named mapping.
func SharedMemoryExists(string) bool { return false }

// SharedMemoryHolders lists the other processes with a handle to the Windows named mapping.
func SharedMemoryHolders(string) ([]Process, error) { return nil, nil }

// GetCompressedFileSize calls GetCompressedFileSizeW for the Windows file.
func GetCompressedFileSize(string) (uint64, error) { return 0, errUnsupported }

// MoveFileReplaceThrough calls MoveFileExW (REPLACE_EXISTING | WRITE_THROUGH).
func MoveFileReplaceThrough(string, string) error { return errUnsupported }

// SetEmulatorWindowsVisible finds the emulator window and calls ShowWindow on Windows.
func SetEmulatorWindowsVisible(uint32, bool) (int, error) { return 0, errUnsupported }

// EmulatorWindowsShown tells whether the emulator window is shown on Windows.
func EmulatorWindowsShown(uint32) (bool, int, error) { return false, 0, errUnsupported }

// GetConsoleWindow calls the Windows GetConsoleWindow API.
func GetConsoleWindow() uintptr { return 0 }

// OpenStableRead requires Windows file-sharing semantics.
func OpenStableRead(string) (*os.File, error) { return nil, errUnsupported }

// FileChangeStamp disables caching without a supported file identity API.
func FileChangeStamp(*os.File) string { return "" }
