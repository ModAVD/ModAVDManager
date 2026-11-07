//go:build windows

package host

import (
	"context"
	"os"
	"os/exec"
	"time"

	"modavd/internal/win"
)

// Windows owns its native types, process methods and child-handle state.
type Process = win.Process
type Listener = win.Listener
type StartOptions = win.StartOptions
type FileLock = win.FileLock
type L3Group = win.L3Group

const SpawnHelperArg = win.SpawnHelperArg
const HideConsole = win.HideConsole

// SetOrphanHelper updates the Windows backend's single helper setting.
func SetOrphanHelper(path string) { win.OrphanHelper = path }

func HideConsoleCmd(cmd *exec.Cmd) { win.HideConsoleCmd(cmd) }

func ListProcesses() ([]Process, error)      { return win.ListProcesses() }
func FindProcess(pid uint32) (Process, bool) { return win.FindProcess(pid) }
func ProcessName(pid uint32) string          { return win.ProcessName(pid) }
func KillTree(p Process) error               { return win.KillTree(p) }

func StartDetached(exe string, args []string, opt StartOptions) (Process, error) {
	return win.StartDetached(exe, args, opt)
}

func StartOrphan(exe string, args []string, opt StartOptions) (Process, error) {
	return win.StartOrphan(exe, args, opt)
}

func SpawnHelperMain(args []string, stdout *os.File) int {
	return win.SpawnHelperMain(args, stdout)
}

func TryLock(path string) (*FileLock, bool, error) { return win.TryLock(path) }
func Lock(path string, timeout time.Duration) (*FileLock, bool, error) {
	return win.Lock(path, timeout)
}
func ExclusiveLockHeld(path string) (bool, error) { return win.ExclusiveLockHeld(path) }

func RunBound(ctx context.Context, exe string, args ...string) ([]byte, []byte, *os.ProcessState, error) {
	return win.RunBound(ctx, exe, args...)
}

func Listeners() ([]Listener, error)             { return win.Listeners() }
func ListenerPID(port int) (uint32, bool, error) { return win.ListenerPID(port) }
func BusyPorts(ports []int) ([]int, error)       { return win.BusyPorts(ports) }

func SetAboveNormalPriority(p Process) error            { return win.SetAboveNormalPriority(p) }
func SetAffinity(p Process, mask uint64) error          { return win.SetAffinity(p, mask) }
func GetAffinity(p Process) (uint64, error)             { return win.GetAffinity(p) }
func L3Groups() []L3Group                               { return win.L3Groups() }
func SharedMemoryExists(name string) bool               { return win.SharedMemoryExists(name) }
func DiskFree(path string) (uint64, error)              { return win.DiskFree(path) }
func GetCompressedFileSize(path string) (uint64, error) { return win.GetCompressedFileSize(path) }
func AllocatedFileBytes(path string) (uint64, error)    { return win.GetCompressedFileSize(path) }
func ReplaceFileDurable(src, dst string) error          { return win.MoveFileReplaceThrough(src, dst) }
func VolumeFileSystem(path string) (string, error)      { return win.VolumeFileSystem(path) }
func MoveFileReplaceThrough(src, dst string) error      { return win.MoveFileReplaceThrough(src, dst) }
func AvailableMemoryMB() (uint64, error)                { return win.AvailableMemoryMB() }
func Protect(data, entropy []byte) ([]byte, error)      { return win.Protect(data, entropy) }
func Unprotect(data, entropy []byte) ([]byte, error)    { return win.Unprotect(data, entropy) }
func SetEmulatorWindowsVisible(pid uint32, visible bool) (int, error) {
	return win.SetEmulatorWindowsVisible(pid, visible)
}
func GetConsoleWindow() uintptr                    { return win.GetConsoleWindow() }
func MessageBox(message string) error              { return win.MessageBox(message) }
func OpenStableRead(path string) (*os.File, error) { return win.OpenStableRead(path) }
func FileChangeStamp(f *os.File) string            { return win.FileChangeStamp(f) }

func TerminateRefused(p Process) bool { return win.TerminateRefused(p) }

// InfoBox displays an informational message with the Windows backend.
func InfoBox(message string) error { return win.InfoBox(message) }
