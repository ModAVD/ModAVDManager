//go:build windows

package win

import (
	"errors"
	"fmt"
	"sort"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/errs"
)

const (
	processQueryLimitedInformation = 0x1000
	processSetInformation          = 0x0200
	processDupHandle               = 0x0040
	stillActive                    = 259
	waitObject0                    = 0x00000000
	waitTimeout                    = 0x00000102
	errorInvalidParameter          = syscall.Errno(87)

	processCommandLineInformation = 60 // PROCESSINFOCLASS, Windows 8.1+
	statusInfoLengthMismatch      = 0xC0000004
	statusBufferOverflow          = 0x80000005
	statusBufferTooSmall          = 0xC0000023
)

// errGone: no process with this PID (and creation time, when known) exists any more.
var errGone = errors.New("process gone")

func filetimeValue(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// handleTimes is GetProcessTimes as plain FILETIME values (creation / exit: 100 ns since 1601; kernel / user: 100 ns).
func handleTimes(h syscall.Handle) (created, exited, kernel, user uint64, err error) {
	var c, e, k, u syscall.Filetime
	if err = syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, 0, 0, 0, err
	}
	return filetimeValue(c), filetimeValue(e), filetimeValue(k), filetimeValue(u), nil
}

// creationTime of pid, 0 when it cannot be opened (gone, protected, PID 0).
func creationTime(pid uint32) uint64 {
	if pid == 0 {
		return 0
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	c, _, _, _, err := handleTimes(h)
	if err != nil {
		return 0
	}
	return c
}

// running: the process behind h has not exited (h needs SYNCHRONIZE).
func running(h syscall.Handle) bool {
	ev, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && ev == waitTimeout
}

// exitCode is GetExitCodeProcess, which needs only PROCESS_QUERY_LIMITED_INFORMATION; ok=false while the process
// runs (STILL_ACTIVE) or when unreadable. The fallback where SYNCHRONIZE is denied (psutil pid_exists does the
// same); a process that really exited with code 259 reads as running here.
func exitCode(h syscall.Handle) (uint32, bool) {
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code == stillActive {
		return 0, false
	}
	return code, true
}

// waitMillis converts a timeout for WaitForSingleObject, rounding up and staying below INFINITE.
func waitMillis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms >= 0xFFFFFFFE {
		return 0xFFFFFFFE
	}
	return uint32(ms)
}

func (p Process) label() string {
	if p.Name != "" {
		return fmt.Sprintf("%s（PID %d）", p.Name, p.PID)
	}
	return fmt.Sprintf("进程 %d", p.PID)
}

// open returns a handle with access (plus PROCESS_QUERY_LIMITED_INFORMATION for the identity check) after checking
// that the PID still names this process: errGone when nothing has the PID or, with Created known, when its creation
// time differs (the PID was reused). An exited process whose object is still held open is not gone here.
func (p Process) open(access uint32) (syscall.Handle, error) {
	if p.PID == 0 {
		return 0, errGone
	}
	h, err := syscall.OpenProcess(access|processQueryLimitedInformation, false, p.PID)
	if err != nil {
		if err == errorInvalidParameter {
			return 0, errGone
		}
		return 0, err
	}
	if p.Created != 0 {
		c, _, _, _, err := handleTimes(h)
		if err != nil {
			syscall.CloseHandle(h)
			return 0, err
		}
		if c != p.Created {
			syscall.CloseHandle(h)
			return 0, errGone
		}
	}
	return h, nil
}

// openRunning is open for an operation on a live process: an exited one (object still held open, e.g. by
// keepHandle) counts as gone. what names the operation for the error message.
func (p Process) openRunning(access uint32, what string) (syscall.Handle, error) {
	h, err := p.open(access | syscall.SYNCHRONIZE)
	if err != nil {
		return 0, p.openErr(err, what)
	}
	if !running(h) {
		syscall.CloseHandle(h)
		return 0, p.openErr(errGone, what)
	}
	return h, nil
}

// openErr turns an open failure into the error callers return.
func (p Process) openErr(err error, what string) error {
	if err == errGone {
		return errs.Newf(errs.Internal, "%s已经退出，无法%s", p.label(), what)
	}
	return errs.Wrap(errs.Internal, err, fmt.Sprintf("打不开%s，无法%s: %v", p.label(), what, err))
}

// snapshot is the toolhelp process list without creation times.
func snapshot() ([]Process, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不到进程列表: "+err.Error())
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	procs := make([]Process, 0, 512)
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		procs = append(procs, Process{PID: e.ProcessID, PPID: e.ParentProcessID, Name: syscall.UTF16ToString(e.ExeFile[:])})
	}
	if err != syscall.ERROR_NO_MORE_FILES {
		return nil, errs.Wrap(errs.Internal, err, "读不到进程列表: "+err.Error())
	}
	return procs, nil
}

// ListProcesses is a toolhelp snapshot of all processes with name, parent and (best effort) creation time.
func ListProcesses() ([]Process, error) {
	procs, err := snapshot()
	if err != nil {
		return nil, err
	}
	for i := range procs {
		procs[i].Created = creationTime(procs[i].PID)
	}
	return procs, nil
}

// FindProcess returns the process with this pid (fresh snapshot / OpenProcess), ok=false when gone.
func FindProcess(pid uint32) (Process, bool) {
	if pid == 0 {
		return Process{}, false
	}
	procs, err := snapshot()
	if err != nil {
		return Process{}, false
	}
	for _, p := range procs {
		if p.PID != pid {
			continue
		}
		p.Created = creationTime(pid)
		if p.Created != 0 && !p.Alive() {
			return Process{}, false
		}
		return p, true
	}
	return Process{}, false
}

// ProcessName is the executable name of pid, "" when unknown.
func ProcessName(pid uint32) string {
	procs, err := snapshot()
	if err != nil {
		return ""
	}
	for _, p := range procs {
		if p.PID == pid {
			return p.Name
		}
	}
	return ""
}

type unicodeString struct {
	Length        uint16 // bytes, without a terminating NUL
	MaximumLength uint16
	Buffer        *uint16
}

func ntError(status uintptr) error {
	if procRtlNtStatusToDosError.Find() == nil {
		if code, _, _ := procRtlNtStatusToDosError.Call(status); code != 0 && code != 317 { // ERROR_MR_MID_NOT_FOUND
			return syscall.Errno(code)
		}
	}
	return fmt.Errorf("NTSTATUS 0x%08X", uint32(status))
}

// queryCommandLine reads the process's command line string (ProcessCommandLineInformation: a UNICODE_STRING
// followed by its text in the same buffer). Needs only PROCESS_QUERY_LIMITED_INFORMATION, unlike reading the PEB.
func queryCommandLine(h syscall.Handle) (string, error) {
	if err := procNtQueryInformationProcess.Find(); err != nil {
		return "", err
	}
	size := uint32(4096)
	for range 6 {
		buf := make([]uint64, (size+7)/8) // 8-byte aligned for the UNICODE_STRING's pointer
		size = uint32(len(buf) * 8)
		var ret uint32
		st, _, _ := procNtQueryInformationProcess.Call(uintptr(h), processCommandLineInformation,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
		switch uint32(st) {
		case 0:
			us := (*unicodeString)(unsafe.Pointer(&buf[0]))
			if us.Length == 0 || us.Buffer == nil {
				return "", nil
			}
			return syscall.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2)), nil
		case statusInfoLengthMismatch, statusBufferOverflow, statusBufferTooSmall:
			if ret <= size {
				ret = size * 2
			}
			size = ret
		default:
			return "", ntError(st)
		}
	}
	return "", errors.New("command line keeps growing")
}

// splitCommandLine is CommandLineToArgvW, except that an empty line is no arguments (the API would return our own
// executable's path for it).
func splitCommandLine(line string) ([]string, error) {
	if line == "" {
		return []string{}, nil
	}
	p, err := syscall.UTF16PtrFromString(line)
	if err != nil {
		return nil, err
	}
	var argc int32
	argv, err := syscall.CommandLineToArgv(p, &argc)
	if err != nil {
		return nil, err
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(argv))))
	// Walk the pointers directly: the [8192] array types of syscall.CommandLineToArgv are only a view.
	ptrs := unsafe.Slice((**uint16)(unsafe.Pointer(argv)), argc)
	args := make([]string, 0, argc)
	for _, a := range ptrs {
		n := 0
		for *(*uint16)(unsafe.Add(unsafe.Pointer(a), n*2)) != 0 {
			n++
		}
		args = append(args, syscall.UTF16ToString(unsafe.Slice(a, n)))
	}
	return args, nil
}

// CommandLine is the process's argv (NtQueryInformationProcess ProcessCommandLineInformation, then
// CommandLineToArgvW), like psutil Process.cmdline(). Error when access is denied or the process is gone.
func (p Process) CommandLine() ([]string, error) {
	h, err := p.open(0)
	if err != nil {
		return nil, p.openErr(err, "读命令行")
	}
	defer syscall.CloseHandle(h)
	line, err := queryCommandLine(h)
	if err != nil {
		// An exited process whose object is still held open answers "access denied": say what really happened.
		if _, exited := exitCode(h); exited {
			return nil, p.openErr(errGone, "读命令行")
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到%s的命令行: %v", p.label(), err))
	}
	args, err := splitCommandLine(line)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("解析不了%s的命令行: %v", p.label(), err))
	}
	return args, nil
}

// StartedAt converts Created to local time; zero time when unknown.
func (p Process) StartedAt() time.Time {
	if p.Created == 0 {
		return time.Time{}
	}
	ft := syscall.Filetime{LowDateTime: uint32(p.Created), HighDateTime: uint32(p.Created >> 32)}
	return time.Unix(0, ft.Nanoseconds())
}

// Alive: the same process (PID and creation time match) is still running.
func (p Process) Alive() bool {
	h, err := p.open(syscall.SYNCHRONIZE)
	if err == errGone {
		return false
	}
	if err != nil {
		// SYNCHRONIZE denied: query access still checks the identity, and the exit code tells.
		if h, err := p.open(0); err == nil {
			defer syscall.CloseHandle(h)
			_, exited := exitCode(h)
			return !exited
		} else if err == errGone {
			return false
		}
		// Denied altogether (a protected process): the snapshot is all we have; its identity cannot be checked then.
		return ProcessName(p.PID) != ""
	}
	defer syscall.CloseHandle(h)
	return running(h)
}

// Wait waits up to timeout for it to exit; true when it is gone (or was never there).
func (p Process) Wait(timeout time.Duration) bool {
	h, err := p.open(syscall.SYNCHRONIZE)
	if err == errGone {
		return true
	}
	if err != nil {
		deadline := time.Now().Add(timeout)
		for p.Alive() {
			if !time.Now().Before(deadline) {
				return false
			}
			time.Sleep(100 * time.Millisecond)
		}
		return true
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, waitMillis(timeout))
	return err == nil && ev == waitObject0
}

// ExitCode is the exit code once the process has exited; ok=false while running or unreadable.
//
// The exit code outlives the process only while some handle to it is open. Processes started by StartDetached /
// StartOrphan keep one in this package (see keepHandle), so their code stays readable, like Popen.poll() in
// AUTO-MAS ("emulator.exe 退出码 N").
func (p Process) ExitCode() (uint32, bool) {
	h, err := p.open(syscall.SYNCHRONIZE)
	if err == errGone {
		return 0, false
	}
	if err != nil {
		if h, err = p.open(0); err != nil { // SYNCHRONIZE denied: the exit code alone has to do
			return 0, false
		}
		defer syscall.CloseHandle(h)
		return exitCode(h)
	}
	defer syscall.CloseHandle(h)
	if running(h) {
		return 0, false
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return 0, false
	}
	return code, true
}

// CPUTime is user + kernel time; ok=false when unreadable.
func (p Process) CPUTime() (time.Duration, bool) {
	h, err := p.open(0)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	_, _, k, u, err := handleTimes(h)
	if err != nil {
		return 0, false
	}
	return time.Duration(k+u) * 100, true
}

// Kill terminates this process only (TerminateProcess), after checking it is still the same process. A process
// that is already gone is not an error (AUTO-MAS suppresses NoSuchProcess).
func (p Process) Kill() error {
	h, err := p.open(syscall.PROCESS_TERMINATE | syscall.SYNCHRONIZE)
	if err == errGone {
		return nil
	}
	if err != nil {
		return p.openErr(err, "结束它")
	}
	defer syscall.CloseHandle(h)
	if !running(h) {
		return nil
	}
	if err := syscall.TerminateProcess(h, 1); err != nil {
		// A process that is already exiting refuses TerminateProcess with access denied.
		if !running(h) {
			return nil
		}
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("结束%s失败: %v", p.label(), err))
	}
	return nil
}

// KillTree kills the descendants (children whose creation time is not before the parent's), then the process.
//
// One snapshot; a child counts only when both creation times are known and it is not older than its parent (an
// older "child" holds a PPID that was reused). Deepest first; each kill re-checks identity. When p itself is gone
// nothing is killed (psutil raises NoSuchProcess there): its PID may name a new process whose children would match.
func KillTree(p Process) error {
	root := p
	if root.Created == 0 {
		root.Created = creationTime(root.PID)
	}
	if h, err := root.open(syscall.SYNCHRONIZE); err == errGone {
		return nil
	} else if err == nil {
		syscall.CloseHandle(h) // an exited p whose object is still held keeps its PID: its children are still its own
	}
	procs, err := ListProcesses()
	if err != nil {
		if kerr := root.Kill(); kerr != nil {
			return kerr
		}
		return err
	}
	children := map[uint32][]Process{}
	for _, c := range procs {
		if c.PID != 0 && c.PID != c.PPID {
			children[c.PPID] = append(children[c.PPID], c)
		}
	}
	type node struct {
		p     Process
		depth int
	}
	var found []node
	seen := map[uint32]bool{root.PID: true}
	queue := []node{{root, 0}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.p.Created == 0 {
			continue
		}
		for _, c := range children[n.p.PID] {
			if seen[c.PID] || c.Created == 0 || c.Created < n.p.Created {
				continue
			}
			seen[c.PID] = true
			child := node{c, n.depth + 1}
			found = append(found, child)
			queue = append(queue, child)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].depth > found[j].depth })
	var first error
	for _, n := range found {
		if err := n.p.Kill(); err != nil && first == nil {
			first = err
		}
	}
	if err := root.Kill(); err != nil {
		return err
	}
	return first
}
