//go:build windows

// Package win is the Windows host API modavd needs, on the standard library only (syscall + kernel32 / ntdll /
// iphlpapi / user32 / shell32 through syscall.NewLazyDLL): processes and their command lines, TCP listeners,
// detached process start, file locks, shared memory, scheduling and window visibility.
//
// psutil equivalents in AUTO-MAS are noted per function; behaviour should match them.
//
// Files: proc.go (processes), start.go (StartDetached / StartOrphan), net.go (listeners), lock.go (FileLock),
// host.go (scheduling, L3 groups, disk, memory, shared memory), window.go (emulator windows).
package win

import (
	"os"
	"syscall"
)

// Process identifies one process: PID plus creation time, so a reused PID is never mistaken for it (psutil does the
// same). Created is the FILETIME (100 ns since 1601), 0 when it could not be read.
type Process struct {
	PID     uint32
	PPID    uint32
	Name    string // executable file name, e.g. "qemu-system-x86_64-headless.exe"
	Created uint64
}

// Listener is one listening TCP socket.
type Listener struct {
	Port int
	PID  uint32
}

// StartOptions for StartDetached.
type StartOptions struct {
	Dir    string   // working directory; "" = the executable's directory
	Env    []string // full environment ("K=V"); nil = inherit ours
	Output *os.File // stdout and stderr; nil = NUL (StartDetached only)
	// OutputPath: stdout and stderr appended to this file (created with its directory). StartOrphan needs this form;
	// StartDetached accepts it too (Output wins when both are set).
	OutputPath string
}

// OrphanHelper is the path of an executable that implements SpawnHelperMain as `<OrphanHelper> __spawn` (the CLI
// sets it to os.Executable() at startup). Empty = StartOrphan falls back to StartDetached (unit tests).
var OrphanHelper string

// SpawnHelperArg is the hidden subcommand name the CLI routes to SpawnHelperMain.
const SpawnHelperArg = "__spawn"

// HideConsole returns SysProcAttr-ready creation flags for short helper commands (CREATE_NO_WINDOW), for use as
// cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}.
const HideConsole = 0x08000000

// FileLock is an exclusive lock on a lock file (LockFileEx on the whole file), held until Unlock. The lock dies
// with the process, so a killed modavd never leaves an instance locked.
type FileLock struct {
	f *os.File
}

// kernel32, ntdll, iphlpapi and shell32 are in the syscall package's system-DLL list, so syscall.LoadDLL loads them
// from System32 only; user32 is a KnownDLL, which Windows always maps from System32. No DLL preloading either way.
var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	modntdll    = syscall.NewLazyDLL("ntdll.dll")
	modiphlpapi = syscall.NewLazyDLL("iphlpapi.dll")
	moduser32   = syscall.NewLazyDLL("user32.dll")

	procGetLogicalProcessorInformationEx = modkernel32.NewProc("GetLogicalProcessorInformationEx")
	procSetPriorityClass                 = modkernel32.NewProc("SetPriorityClass")
	procSetProcessAffinityMask           = modkernel32.NewProc("SetProcessAffinityMask")
	procOpenFileMappingW                 = modkernel32.NewProc("OpenFileMappingW")
	procGetDiskFreeSpaceExW              = modkernel32.NewProc("GetDiskFreeSpaceExW")
	procGlobalMemoryStatusEx             = modkernel32.NewProc("GlobalMemoryStatusEx")
	procLockFileEx                       = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx                     = modkernel32.NewProc("UnlockFileEx")
	procGetProcessId                     = modkernel32.NewProc("GetProcessId")
	procQueryFullProcessImageNameW       = modkernel32.NewProc("QueryFullProcessImageNameW")

	procNtQueryInformationProcess = modntdll.NewProc("NtQueryInformationProcess")
	procRtlNtStatusToDosError     = modntdll.NewProc("RtlNtStatusToDosError")

	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")

	procEnumWindows              = moduser32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = moduser32.NewProc("GetWindowThreadProcessId")
	procGetClassNameW            = moduser32.NewProc("GetClassNameW")
	procGetWindowTextW           = moduser32.NewProc("GetWindowTextW")
	procShowWindowAsync          = moduser32.NewProc("ShowWindowAsync")

	// shell32 CommandLineToArgvW is syscall.CommandLineToArgv (its typed result keeps go vet's unsafeptr check quiet).
)

// call runs a Win32 function that reports failure with a zero result (BOOL / HANDLE). err is meaningful only when
// r == 0: the thread's last error, or why the function could not be bound.
func call(p *syscall.LazyProc, args ...uintptr) (r uintptr, err error) {
	if err := p.Find(); err != nil {
		return 0, err
	}
	r, _, e := p.Call(args...)
	if en, ok := e.(syscall.Errno); ok && en == 0 {
		e = syscall.EINVAL // failed without setting a last error
	}
	return r, e
}
