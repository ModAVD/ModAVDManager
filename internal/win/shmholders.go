//go:build windows

package win

import (
	"strings"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/errs"
)

var (
	procNtQuerySystemInformation = modntdll.NewProc("NtQuerySystemInformation")
	procNtQueryObject            = modntdll.NewProc("NtQueryObject")
)

const (
	systemExtendedHandleInformation = 64 // SYSTEM_INFORMATION_CLASS
	objectNameInformation           = 1  // OBJECT_INFORMATION_CLASS
	statusSuccess                   = 0
	// SYSTEM_HANDLE_TABLE_ENTRY_INFO_EX on 64-bit Windows: Object, UniqueProcessId, HandleValue (pointer-sized),
	// GrantedAccess (u32), CreatorBackTraceIndex (u16), ObjectTypeIndex (u16), HandleAttributes (u32), Reserved (u32)
	handleEntrySize   = 40
	handleEntryPID    = 8
	handleEntryHandle = 16
	handleEntryType   = 30 // ObjectTypeIndex: after GrantedAccess (24..28) and CreatorBackTraceIndex (28..30)
	handleTableHeader = 16 // NumberOfHandles, Reserved
	holdersBudget     = 3 * time.Second
)

// handleEntry is one row of the system handle table that matters here.
type handleEntry struct {
	object uintptr
	pid    uint32
	handle uintptr
	typ    uint16
}

// parseHandleTable reads a SYSTEM_HANDLE_INFORMATION_EX buffer; rows past the end of buf are dropped.
func parseHandleTable(buf []byte) []handleEntry {
	if len(buf) < handleTableHeader {
		return nil
	}
	n := *(*uintptr)(unsafe.Pointer(&buf[0]))
	if max := uintptr(len(buf)-handleTableHeader) / handleEntrySize; n > max {
		n = max
	}
	out := make([]handleEntry, 0, n)
	for i := uintptr(0); i < n; i++ {
		row := buf[handleTableHeader+i*handleEntrySize:]
		out = append(out, handleEntry{
			object: *(*uintptr)(unsafe.Pointer(&row[0])),
			pid:    uint32(*(*uintptr)(unsafe.Pointer(&row[handleEntryPID]))),
			handle: *(*uintptr)(unsafe.Pointer(&row[handleEntryHandle])),
			typ:    *(*uint16)(unsafe.Pointer(&row[handleEntryType])),
		})
	}
	return out
}

// systemHandles is the system handle table (NtQuerySystemInformation, SystemExtendedHandleInformation).
func systemHandles() ([]handleEntry, error) {
	size := uint32(4 << 20)
	for size <= 512<<20 {
		buf := make([]byte, size)
		var need uint32
		st, _, _ := procNtQuerySystemInformation.Call(systemExtendedHandleInformation, uintptr(unsafe.Pointer(&buf[0])),
			uintptr(size), uintptr(unsafe.Pointer(&need)))
		if st == statusSuccess {
			return parseHandleTable(buf), nil
		}
		if uint32(st) != statusInfoLengthMismatch {
			return nil, errs.Newf(errs.Internal, "读不到系统句柄表（NTSTATUS 0x%08X）", uint32(st))
		}
		// the table grows between calls: take what it asked for and some more
		next := size * 2
		if need+need/4 > next {
			next = need + need/4
		}
		size = next
	}
	return nil, errs.New(errs.Internal, "系统句柄表太大，没读")
}

// objectName is the kernel name of a handle of this process (NtQueryObject, ObjectNameInformation), "" when it has
// none or cannot be read. Called only on section handles: name queries on some other types (pipes) can block.
func objectName(h syscall.Handle) string {
	var buf [2048]byte
	var ret uint32
	st, _, _ := procNtQueryObject.Call(uintptr(h), objectNameInformation, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), uintptr(unsafe.Pointer(&ret)))
	if st != statusSuccess {
		return ""
	}
	// UNICODE_STRING: Length (bytes), MaximumLength, padding, Buffer
	length := *(*uint16)(unsafe.Pointer(&buf[0]))
	p := *(**uint16)(unsafe.Pointer(&buf[8]))
	if length == 0 || p == nil {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice(p, length/2))
}

// SharedMemoryHolders are the other processes with a handle to the named mapping `name` (e.g. "SHM_videmulator20000"),
// best effort: processes that cannot be opened for handle duplication (other users, protected) are not seen, nor is
// a process that only keeps a mapped view after closing its handle. Nil when the mapping does not exist.
func SharedMemoryHolders(name string) ([]Process, error) {
	if name == "" {
		return nil, nil
	}
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil
	}
	own, _ := call(procOpenFileMappingW, fileMapRead, 0, uintptr(unsafe.Pointer(p)))
	if own == 0 {
		return nil, nil
	}
	defer syscall.CloseHandle(syscall.Handle(own))
	table, err := systemHandles()
	if err != nil {
		return nil, err
	}
	self := uint32(syscall.Getpid())
	var mine *handleEntry
	for i := range table {
		if table[i].pid == self && table[i].handle == own {
			mine = &table[i]
			break
		}
	}
	if mine == nil {
		return nil, errs.New(errs.Internal, "系统句柄表里找不到自己打开的这块共享内存")
	}
	holders := map[uint32]bool{}
	if mine.object != 0 {
		// kernel addresses are visible (an elevated caller): the same object is the same mapping
		for _, e := range table {
			if e.pid != self && e.object == mine.object {
				holders[e.pid] = true
			}
		}
	} else {
		// Windows 11 24H2 and later hide them: duplicate each section handle and compare names
		suffix := `\` + strings.ToLower(name)
		procs := map[uint32]syscall.Handle{}
		defer func() {
			for _, h := range procs {
				if h != 0 {
					syscall.CloseHandle(h)
				}
			}
		}()
		me, _ := syscall.GetCurrentProcess()
		deadline := time.Now().Add(holdersBudget)
		for _, e := range table {
			if e.typ != mine.typ || e.pid == self || holders[e.pid] || time.Now().After(deadline) {
				continue
			}
			src, seen := procs[e.pid]
			if !seen {
				src, _ = syscall.OpenProcess(processDupHandle, false, e.pid)
				procs[e.pid] = src
			}
			if src == 0 {
				continue
			}
			var dup syscall.Handle
			if syscall.DuplicateHandle(src, syscall.Handle(e.handle), me, &dup, 0, false, syscall.DUPLICATE_SAME_ACCESS) != nil {
				continue
			}
			if strings.HasSuffix(strings.ToLower(objectName(dup)), suffix) {
				holders[e.pid] = true
			}
			syscall.CloseHandle(dup)
		}
	}
	if len(holders) == 0 {
		return nil, nil
	}
	all, err := snapshot()
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, pr := range all {
		if holders[pr.PID] {
			out = append(out, pr)
		}
	}
	return out, nil
}
