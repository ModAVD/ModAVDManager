//go:build windows

package emu

import (
	"syscall"
	"testing"
	"unsafe"
)

var lifetimeKernel32 = syscall.NewLazyDLL("kernel32.dll")

type lifetimeJobBasicLimits struct {
	PerProcessUserTimeLimit, PerJobUserTimeLimit int64
	LimitFlags                                   uint32
	MinimumWorkingSetSize, MaximumWorkingSetSize uintptr
	ActiveProcessLimit                           uint32
	Affinity                                     uintptr
	PriorityClass, SchedulingClass               uint32
}

type lifetimeJobLimits struct {
	Basic                                                                        lifetimeJobBasicLimits
	IOCounters                                                                   [6]uint64
	ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed uintptr
}

// The controller owns this job. Both test managers join it before their launch
// gates open, and neither can break away. They must create independent inner jobs.
func lifetimeOuterJob(t *testing.T) syscall.Handle {
	t.Helper()
	h, _, err := lifetimeKernel32.NewProc("CreateJobObjectW").Call(0, 0)
	if h == 0 {
		t.Fatal(err)
	}
	job := syscall.Handle(h)
	t.Cleanup(func() { syscall.CloseHandle(job) })
	var limits lifetimeJobLimits
	const killOnClose = 0x2000
	limits.Basic.LimitFlags = killOnClose // no BREAKAWAY_OK or SILENT_BREAKAWAY_OK
	if ok, _, err := lifetimeKernel32.NewProc("SetInformationJobObject").Call(h, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); ok == 0 {
		t.Fatal(err)
	}
	var actual lifetimeJobLimits
	if ok, _, err := lifetimeKernel32.NewProc("QueryInformationJobObject").Call(h, 9, uintptr(unsafe.Pointer(&actual)), unsafe.Sizeof(actual), 0); ok == 0 {
		t.Fatal(err)
	}
	if actual.Basic.LimitFlags != killOnClose {
		t.Fatalf("outer job limits=%#x, want only KILL_ON_JOB_CLOSE", actual.Basic.LimitFlags)
	}
	return job
}

func lifetimeAssignOuterJob(t *testing.T, job syscall.Handle, pid uint32) {
	t.Helper()
	const processSetQuota = 0x100
	h, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE|syscall.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if ok, _, err := lifetimeKernel32.NewProc("AssignProcessToJobObject").Call(uintptr(job), uintptr(h)); ok == 0 {
		t.Fatal(err)
	}
	lifetimeAssertInJob(t, h, job)
}

func lifetimeAssertInJob(t *testing.T, process, job syscall.Handle) {
	t.Helper()
	var member uint32
	if ok, _, err := lifetimeKernel32.NewProc("IsProcessInJob").Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member))); ok == 0 {
		t.Fatal(err)
	}
	if member == 0 {
		t.Fatal("process escaped the outer job")
	}
}
