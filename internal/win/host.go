//go:build windows

package win

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

const (
	aboveNormalPriorityClass = 0x00008000
	relationProcessorCore    = 0 // LOGICAL_PROCESSOR_RELATIONSHIP RelationProcessorCore (winnt.h)
	relationCache            = 2 // LOGICAL_PROCESSOR_RELATIONSHIP RelationCache
	fileMapRead              = 0x0004

	// processthreadsapi.h (Windows SDK 10.0.26100.0): ProcessPowerThrottling is the fifth PROCESS_INFORMATION_CLASS
	// (after ProcessMemoryPriority, ProcessMemoryExhaustionInfo, ProcessAppMemoryInfo, ProcessInPrivateInfo).
	processPowerThrottling               = 4
	processPowerThrottlingCurrentVersion = 1   // PROCESS_POWER_THROTTLING_CURRENT_VERSION
	processPowerThrottlingExecutionSpeed = 0x1 // PROCESS_POWER_THROTTLING_EXECUTION_SPEED
)

// powerThrottlingState is PROCESS_POWER_THROTTLING_STATE: three ULONGs, Version, ControlMask, StateMask.
type powerThrottlingState struct {
	Version     uint32
	ControlMask uint32
	StateMask   uint32
}

// DisableExecutionThrottling tells Windows never to throttle the execution speed of p (EcoQoS), visible or not:
// SetProcessInformation(ProcessPowerThrottling) with EXECUTION_SPEED in the control mask and cleared in the state
// mask. On hybrid processors the process is then scheduled as high QoS, which prefers the performance cores. An
// errs.Unsupported error when this Windows has no such switch (no SetProcessInformation before Windows 8, the
// information class rejected before Windows 10 1709).
func DisableExecutionThrottling(p Process) error {
	if procSetProcessInformation.Find() != nil {
		return errs.New(errs.Unsupported, "这个 Windows 版本没有 SetProcessInformation")
	}
	h, err := p.openRunning(processSetInformation, "关闭进程节流")
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	st := powerThrottlingState{Version: processPowerThrottlingCurrentVersion,
		ControlMask: processPowerThrottlingExecutionSpeed, StateMask: 0}
	if r, err := call(procSetProcessInformation, uintptr(h), processPowerThrottling, uintptr(unsafe.Pointer(&st)),
		unsafe.Sizeof(st)); r == 0 {
		if throttlingUnsupported(err) {
			return errs.Wrap(errs.Unsupported, err, "这个 Windows 版本不支持关闭进程节流: "+err.Error())
		}
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("关闭%s的进程节流失败: %v", p.label(), err))
	}
	return nil
}

// throttlingUnsupported: the error of a Windows that does not know the information class or the structure.
func throttlingUnsupported(err error) bool {
	const (
		errorInvalidFunction    = 1   // ERROR_INVALID_FUNCTION
		errorNotSupported       = 50  // ERROR_NOT_SUPPORTED
		errorInvalidParameter   = 87  // ERROR_INVALID_PARAMETER
		errorCallNotImplemented = 120 // ERROR_CALL_NOT_IMPLEMENTED
	)
	en, ok := err.(syscall.Errno)
	return ok && (en == errorInvalidParameter || en == errorNotSupported || en == errorCallNotImplemented ||
		en == errorInvalidFunction)
}

// ExecutionThrottling reads p's PROCESS_POWER_THROTTLING_STATE back (GetProcessInformation): controlled is true
// when the process decides its execution speed throttling itself, throttled when it asked for it. Not controlled
// means Windows decides (the default).
func ExecutionThrottling(p Process) (controlled, throttled bool, err error) {
	if procGetProcessInformation.Find() != nil {
		return false, false, errs.New(errs.Unsupported, "这个 Windows 版本没有 GetProcessInformation")
	}
	h, err := p.openRunning(processQueryLimitedInformation, "读取进程节流")
	if err != nil {
		return false, false, err
	}
	defer syscall.CloseHandle(h)
	st := powerThrottlingState{Version: processPowerThrottlingCurrentVersion}
	if r, err := call(procGetProcessInformation, uintptr(h), processPowerThrottling, uintptr(unsafe.Pointer(&st)),
		unsafe.Sizeof(st)); r == 0 {
		code := errs.Internal
		if throttlingUnsupported(err) {
			code = errs.Unsupported
		}
		return false, false, errs.Wrap(code, err, fmt.Sprintf("读取%s的进程节流失败: %v", p.label(), err))
	}
	return st.ControlMask&processPowerThrottlingExecutionSpeed != 0,
		st.StateMask&processPowerThrottlingExecutionSpeed != 0, nil
}

// CoreClass is one EfficiencyClass of the host's cores: how many cores and logical processors have it. A higher
// class is a faster core; a host without hybrid cores has a single class 0.
type CoreClass struct {
	Class   uint8
	Cores   int
	Logical int
}

// CoreClasses are the efficiency classes of the host's cores, lowest first (every processor group); nil when the
// topology is unreadable.
func CoreClasses() []CoreClass { return parseCoreClasses(processorTopology(relationProcessorCore)) }

// parseCoreClasses walks SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX entries of RelationProcessorCore (winnt.h):
// Relationship at +0, Size at +4, then PROCESSOR_RELATIONSHIP: Flags +8, EfficiencyClass +9, Reserved[20],
// GroupCount (WORD) +30, GroupMask[GroupCount] from +32, 16 bytes each (GROUP_AFFINITY: KAFFINITY Mask, WORD Group,
// WORD Reserved[3]). An entry that is cut short ends the walk.
func parseCoreClasses(raw []byte) []CoreClass {
	byClass := map[uint8]*CoreClass{}
	for off := 0; off+8 <= len(raw); {
		size := int(binary.LittleEndian.Uint32(raw[off+4:]))
		if size <= 0 || off+size > len(raw) {
			break
		}
		if binary.LittleEndian.Uint32(raw[off:]) == relationProcessorCore && size >= 32 {
			class := raw[off+9]
			n := int(binary.LittleEndian.Uint16(raw[off+30:]))
			logical := 0
			for g := 0; g < n && off+32+16*(g+1) <= off+size; g++ {
				logical += bits.OnesCount64(binary.LittleEndian.Uint64(raw[off+32+16*g:]))
			}
			c := byClass[class]
			if c == nil {
				c = &CoreClass{Class: class}
				byClass[class] = c
			}
			c.Cores++
			c.Logical += logical
		}
		off += size
	}
	if len(byClass) == 0 {
		return nil
	}
	out := make([]CoreClass, 0, len(byClass))
	for _, c := range byClass {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b CoreClass) int { return int(a.Class) - int(b.Class) })
	return out
}

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
func cacheTopology() []byte { return processorTopology(relationCache) }

// processorTopology is the raw GetLogicalProcessorInformationEx buffer of one LOGICAL_PROCESSOR_RELATIONSHIP, nil
// when unreadable.
func processorTopology(relation uintptr) []byte {
	if procGetLogicalProcessorInformationEx.Find() != nil {
		return nil
	}
	var n uint32
	procGetLogicalProcessorInformationEx.Call(relation, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	r, _, _ := procGetLogicalProcessorInformationEx.Call(relation, uintptr(unsafe.Pointer(&buf[0])),
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

// VolumeFileSystem is the file-system name of the volume that holds path ("NTFS", "ReFS", "exFAT", "FAT32", ...),
// via GetVolumePathNameW (path -> the volume's mount point) then GetVolumeInformationW. Compact uses it to run only
// on journaled file systems, where a write-through replace is a single transaction and the swap is atomic.
func VolumeFileSystem(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errs.Wrap(errs.Internal, err, fmt.Sprintf("路径 %s 不可用: %v", path, err))
	}
	p, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return "", errs.Wrap(errs.Internal, err, fmt.Sprintf("路径 %s 不可用: %v", path, err))
	}
	mount := make([]uint16, syscall.MAX_PATH+1)
	if r, e := call(procGetVolumePathNameW, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&mount[0])),
		uintptr(len(mount))); r == 0 {
		return "", errs.Wrap(errs.Internal, e, fmt.Sprintf("找不到 %s 所在的卷: %v", path, e))
	}
	fsName := make([]uint16, 64)
	if r, e := call(procGetVolumeInformationW, uintptr(unsafe.Pointer(&mount[0])), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&fsName[0])), uintptr(len(fsName))); r == 0 {
		return "", errs.Wrap(errs.Internal, e, fmt.Sprintf("读不到 %s 所在卷的文件系统: %v", path, e))
	}
	name := syscall.UTF16ToString(fsName)
	if name == "" {
		// seen once on a CI runner (ModAVD/ModAVD#142): success without a name would read "数据盘所在卷是 ，"
		return "", errs.Newf(errs.Internal, "读不到 %s 所在卷的文件系统：系统没给出名字", path)
	}
	return name, nil
}

// GetCompressedFileSize is the bytes the file at path really occupies on the volume (GetCompressedFileSizeW): for a
// sparse or NTFS-compressed file that is less than its length. For a plain file it is the length rounded up to the
// cluster.
func GetCompressedFileSize(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, err, fmt.Sprintf("路径 %s 不可用: %v", path, err))
	}
	if err := procGetCompressedFileSizeW.Find(); err != nil {
		return 0, errs.Wrap(errs.Internal, err, "读不到文件占用的磁盘空间: "+err.Error())
	}
	const invalidFileSize = 0xFFFFFFFF
	var high uint32
	low, _, e := procGetCompressedFileSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	// INVALID_FILE_SIZE is also a legal low word of a file larger than 4 GiB: only the last error tells them apart.
	if uint32(low) == invalidFileSize {
		if en, ok := e.(syscall.Errno); ok && en != 0 {
			if en == syscall.ERROR_FILE_NOT_FOUND || en == syscall.ERROR_PATH_NOT_FOUND {
				return 0, errs.Wrap(errs.NotFound, e, "找不到文件: "+path)
			}
			return 0, errs.Wrap(errs.Internal, e, fmt.Sprintf("读不到 %s 占用的磁盘空间: %v", path, e))
		}
	}
	return uint64(high)<<32 | uint64(uint32(low)), nil
}

// MoveFileReplaceThrough renames src over dst (MoveFileExW, REPLACE_EXISTING |
// WRITE_THROUGH): dst is replaced and both the rename and the pending writes are
// flushed to disk before it returns, so a crash leaves either the old file or the
// new one, never a torn mix. Used to swap in a freshly compacted disk image.
func MoveFileReplaceThrough(src, dst string) error {
	s, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "源路径不可用: "+src)
	}
	d, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "目标路径不可用: "+dst)
	}
	const moveReplaceExisting = 0x1
	const moveWriteThrough = 0x8
	r, e := call(procMoveFileExW, uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(d)),
		moveReplaceExisting|moveWriteThrough)
	if r == 0 {
		return errs.Wrap(errs.Internal, e, fmt.Sprintf("换文件失败（%s → %s）: %v", src, dst, e))
	}
	return nil
}

// OpenStableRead keeps the file from being written, renamed or deleted while
// a raw backing is being verified or an offline overlay is being copied.
func OpenStableRead(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

var getFileInformationByHandleEx = modkernel32.NewProc("GetFileInformationByHandleEx")

// FileChangeStamp identifies an unchanged file at its current path. An empty
// result disables caching on filesystems without a file ID or change time.
func FileChangeStamp(f *os.File) string {
	h := syscall.Handle(f.Fd())
	var id syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &id) != nil || id.FileIndexHigh == 0 && id.FileIndexLow == 0 {
		return ""
	}
	var basic struct {
		Creation, Access, Write, Change int64
		Attributes, Padding             uint32
	}
	ok, _ := call(getFileInformationByHandleEx, uintptr(h), 0 /* FileBasicInfo */, uintptr(unsafe.Pointer(&basic)), unsafe.Sizeof(basic))
	if ok == 0 || basic.Change == 0 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x-%x", id.VolumeSerialNumber, id.FileIndexHigh, id.FileIndexLow, basic.Creation, basic.Write, basic.Change)
}
