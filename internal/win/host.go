//go:build windows

package win

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

const (
	aboveNormalPriorityClass = 0x00008000
	relationCache            = 2 // LOGICAL_PROCESSOR_RELATIONSHIP RelationCache
	fileMapRead              = 0x0004
)

// SetAboveNormalPriority sets ABOVE_NORMAL_PRIORITY_CLASS.
func SetAboveNormalPriority(p Process) error {
	h, err := p.openRunning(processSetInformation, "调整优先级")
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	if r, err := call(procSetPriorityClass, uintptr(h), aboveNormalPriorityClass); r == 0 {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("调整%s的优先级失败: %v", p.label(), err))
	}
	return nil
}

// SetAffinity sets the process affinity mask (processor group 0).
func SetAffinity(p Process, mask uint64) error {
	if mask == 0 {
		return errs.New(errs.InvalidArgument, "绑核的处理器掩码不能为 0")
	}
	h, err := p.openRunning(processSetInformation, "绑核")
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	if r, err := call(procSetProcessAffinityMask, uintptr(h), uintptr(mask)); r == 0 {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("给%s绑核（掩码 0x%x）失败: %v", p.label(), mask, err))
	}
	return nil
}

// GetAffinity is the process affinity mask (processor group 0).
func GetAffinity(p Process) (uint64, error) {
	h, err := p.openRunning(processQueryLimitedInformation, "读取绑核")
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var proc, sys uintptr
	if r, err := call(procGetProcessAffinityMask, uintptr(h), uintptr(unsafe.Pointer(&proc)), uintptr(unsafe.Pointer(&sys))); r == 0 {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("读取%s的绑核失败: %v", p.label(), err))
	}
	return uint64(proc), nil
}

// L3Group is one L3 cache: its size in bytes and the logical processors sharing it (processor group 0).
type L3Group struct {
	Size uint32
	Mask uint64
}

// L3Groups lists the L3 caches of the host, nil when the topology is unreadable.
func L3Groups() []L3Group {
	var out []L3Group
	for _, g := range parseL3Groups(cacheTopology()) {
		out = append(out, L3Group{Size: g.size, Mask: g.mask})
	}
	return out
}

// l3Group is the parsed form of L3Group.
type l3Group struct {
	size uint32
	mask uint64
}

// cacheTopology is the raw GetLogicalProcessorInformationEx(RelationCache) buffer, nil when unreadable (AUTO-MAS
// emulator_native.cpu_cache_topology).
func cacheTopology() []byte {
	if procGetLogicalProcessorInformationEx.Find() != nil {
		return nil
	}
	var n uint32
	procGetLogicalProcessorInformationEx.Call(relationCache, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	r, _, _ := procGetLogicalProcessorInformationEx.Call(relationCache, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)))
	if r == 0 || int(n) > len(buf) {
		return nil
	}
	return buf[:n]
}

// parseL3Groups walks SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX entries (size at +4) and keeps the level-3 caches:
// CACHE_RELATIONSHIP Level at +8, CacheSize at +12, GroupMask.Mask at +40 (AUTO-MAS host._l3_groups).
func parseL3Groups(raw []byte) []l3Group {
	var groups []l3Group
	for off := 0; off+48 <= len(raw); {
		size := int(binary.LittleEndian.Uint32(raw[off+4:]))
		if size <= 0 {
			break
		}
		if raw[off+8] == 3 {
			groups = append(groups, l3Group{
				size: binary.LittleEndian.Uint32(raw[off+12:]),
				mask: binary.LittleEndian.Uint64(raw[off+40:]),
			})
		}
		off += size
	}
	return groups
}

// SharedMemoryExists: OpenFileMappingW(FILE_MAP_READ, FALSE, name) succeeds (e.g. "SHM_videmulator20000").
func SharedMemoryExists(name string) bool {
	if name == "" {
		return false
	}
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, _ := call(procOpenFileMappingW, fileMapRead, 0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}

// DiskFree is the free bytes available to the caller on the volume holding path (GetDiskFreeSpaceExW); path's
// nearest existing ancestor is used.
func DiskFree(path string) (uint64, error) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("路径 %s 不可用: %v", path, err))
	}
	for {
		fi, err := os.Stat(dir)
		if err == nil {
			if !fi.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, errs.Newf(errs.Internal, "找不到 %s 所在的磁盘", path)
		}
		dir = parent
	}
	// UNC roots need the trailing backslash; it is harmless for every other directory.
	if !strings.HasSuffix(dir, `\`) {
		dir += `\`
	}
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("路径 %s 不可用: %v", path, err))
	}
	var free, total, totalFree uint64
	if r, err := call(procGetDiskFreeSpaceExW, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); r == 0 {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到 %s 所在磁盘的剩余空间: %v", dir, err))
	}
	return free, nil
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// AvailableMemoryMB is GlobalMemoryStatusEx ullAvailPhys in MiB (psutil virtual_memory().available).
func AvailableMemoryMB() (uint64, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, err := call(procGlobalMemoryStatusEx, uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到可用内存: %v", err))
	}
	return m.AvailPhys / (1024 * 1024), nil
}
