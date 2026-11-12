//go:build windows

package win

import (
	"syscall"
	"unsafe"
)

var (
	procK32GetProcessMemoryInfo = modkernel32.NewProc("K32GetProcessMemoryInfo")
	procGlobalMemoryStatusEx    = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

// Memory is GetProcessMemoryInfo of p, in bytes: commit is its private commit charge (Task Manager's "Commit size"),
// peakCommit the highest that has been, workingSet what it has in physical memory now.
func (p Process) Memory() (commit, peakCommit, workingSet uint64, err error) {
	h, err := p.open(0)
	if err != nil {
		return 0, 0, 0, err
	}
	defer syscall.CloseHandle(h)
	var c processMemoryCountersEx
	c.cb = uint32(unsafe.Sizeof(c))
	if r, _, e := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r == 0 {
		return 0, 0, 0, e
	}
	return uint64(c.privateUsage), uint64(c.peakPagefileUsage), uint64(c.workingSetSize), nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// SystemMemory is GlobalMemoryStatusEx, in bytes: physical memory in all and available, the commit limit (physical
// memory plus page files) and how much of it is committed.
func SystemMemory() (totalPhys, availPhys, commitLimit, committed uint64, err error) {
	var s memoryStatusEx
	s.length = uint32(unsafe.Sizeof(s))
	if r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return 0, 0, 0, 0, e
	}
	return s.totalPhys, s.availPhys, s.totalPageFile, s.totalPageFile - min(s.availPageFile, s.totalPageFile), nil
}
