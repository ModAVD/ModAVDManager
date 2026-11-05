//go:build windows

package win

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	procThreadAttributeHandleList     = 0x20002
	procThreadAttributeJobList        = 0x2000d // Windows 10+
	extendedStartupInfoPresent        = 0x80000
)

var (
	procCreateJobObjectW                  = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject           = modkernel32.NewProc("SetInformationJobObject")
	procTerminateJobObject                = modkernel32.NewProc("TerminateJobObject")
	procInitializeProcThreadAttributeList = modkernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = modkernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = modkernel32.NewProc("DeleteProcThreadAttributeList")
)

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit, PerJobUserTimeLimit int64
	LimitFlags                                   uint32
	MinimumWorkingSetSize, MaximumWorkingSetSize uintptr
	ActiveProcessLimit                           uint32
	Affinity                                     uintptr
	PriorityClass, SchedulingClass               uint32
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation                                                        jobBasicLimitInformation
	IOCounters                                                                   [6]uint64
	ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed uintptr
}

type boundStartupInfo struct {
	syscall.StartupInfo
	AttributeList *byte
}

// startBound creates the child in its own kill-on-close job. JOB_LIST assigns it
// before its first thread runs, including when the manager dies during launch.
// The job is unnamed and non-inheritable; only this invocation owns its handle.
// See https://devblogs.microsoft.com/oldnewthing/20230209-00/?p=107812/.
func startBound(exe string, args []string, files [3]*os.File) (*os.Process, syscall.Handle, error) {
	job, _, err := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return nil, 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	h := syscall.Handle(job)
	ok := false
	defer func() {
		if !ok {
			syscall.CloseHandle(h)
		}
	}()
	limits := jobExtendedLimitInformation{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if r, _, e := procSetInformationJobObject.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); r == 0 {
		return nil, 0, fmt.Errorf("SetInformationJobObject: %w", e)
	}
	var size uintptr
	procInitializeProcThreadAttributeList.Call(0, 2, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, 0, fmt.Errorf("InitializeProcThreadAttributeList: no size")
	}
	attributes := make([]byte, size)
	list := &attributes[0]
	if r, _, e := procInitializeProcThreadAttributeList.Call(uintptr(unsafe.Pointer(list)), 2, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		return nil, 0, fmt.Errorf("InitializeProcThreadAttributeList: %w", e)
	}
	defer func() {
		procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(list)))
		runtime.KeepAlive(attributes)
	}()
	update := func(attribute uintptr, value unsafe.Pointer, size uintptr) error {
		if r, _, e := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(list)), 0, attribute, uintptr(value), size, 0, 0); r == 0 {
			return fmt.Errorf("UpdateProcThreadAttribute: %w", e)
		}
		return nil
	}
	if err := update(procThreadAttributeJobList, unsafe.Pointer(&h), unsafe.Sizeof(h)); err != nil {
		return nil, 0, err
	}
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, 0, err
	}
	var handles [3]syscall.Handle
	for i, f := range files {
		if err := syscall.DuplicateHandle(self, syscall.Handle(f.Fd()), self, &handles[i], 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, 0, err
		}
		defer syscall.CloseHandle(handles[i])
	}
	// Restrict inheritance to stdin/stdout/stderr. In particular the child must
	// never inherit the job handle and keep itself alive after the manager dies.
	if err := update(procThreadAttributeHandleList, unsafe.Pointer(&handles[0]), unsafe.Sizeof(handles)); err != nil {
		return nil, 0, err
	}
	exe, err = resolveExe(exe)
	if err != nil {
		return nil, 0, err
	}
	application, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return nil, 0, err
	}
	argv := []string{syscall.EscapeArg(exe)}
	for _, arg := range args {
		argv = append(argv, syscall.EscapeArg(arg))
	}
	command, err := syscall.UTF16PtrFromString(strings.Join(argv, " "))
	if err != nil {
		return nil, 0, err
	}
	si := boundStartupInfo{AttributeList: list}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = handles[0], handles[1], handles[2]
	var pi syscall.ProcessInformation
	err = syscall.CreateProcess(application, command, nil, nil, true, HideConsole|extendedStartupInfoPresent, nil, nil, &si.StartupInfo, &pi)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(&h)
	runtime.KeepAlive(files)
	if err != nil {
		return nil, 0, err
	}
	defer syscall.CloseHandle(pi.Thread)
	defer syscall.CloseHandle(pi.Process)
	p, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		return nil, 0, err
	}
	ok = true
	return p, h, nil
}

// waitBound owns the process and job handles until the child has exited.
func waitBound(ctx context.Context, p *os.Process, job syscall.Handle) (state *os.ProcessState, err error) {
	defer p.Release()
	closeJob := func() {
		if job != 0 {
			syscall.CloseHandle(job)
			job = 0
		}
	}
	defer closeJob()
	done := make(chan struct{})
	go func() { state, err = p.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// An observer may hold another job handle. Explicit termination must not
		// depend on ours being the last handle; kill-on-close still covers crashes.
		if r, _, _ := procTerminateJobObject.Call(uintptr(job), 1); r == 0 {
			_ = p.Kill()
		}
		closeJob()
		<-done
	}
	return state, err
}

// RunBound captures a short-lived helper's output and waits for it. Its job dies
// with the calling process, even on TerminateProcess or a console-window close.
// Cancellation terminates only this invocation's job and waits for the child to exit.
// A nil state denotes a launch failure; a nonzero exit retains state and output.
func RunBound(ctx context.Context, exe string, args ...string) (stdout, stderr []byte, state *os.ProcessState, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	in, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, nil, err
	}
	defer in.Close()
	ro, wo, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	defer ro.Close()
	defer wo.Close()
	re, we, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	defer re.Close()
	defer we.Close()
	p, job, err := startBound(exe, args, [3]*os.File{in, wo, we})
	if err != nil {
		return nil, nil, nil, err
	}
	wo.Close()
	we.Close()
	type output struct {
		data []byte
		err  error
	}
	read := func(f *os.File) <-chan output {
		ch := make(chan output, 1)
		go func() { b, e := io.ReadAll(f); ch <- output{b, e} }()
		return ch
	}
	so, se := read(ro), read(re)
	state, err = waitBound(ctx, p, job)
	// Match qemu-img's existing WaitDelay, bounding inherited output pipes.
	timer := time.AfterFunc(2*time.Second, func() { ro.Close(); re.Close() })
	o, e := <-so, <-se
	timer.Stop()
	if err == nil {
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case !state.Success():
			err = &exec.ExitError{ProcessState: state}
		case o.err != nil || e.err != nil:
			err = exec.ErrWaitDelay
		}
	}
	return o.data, e.data, state, err
}
