//go:build darwin && cgo

package macos

/*
#include <mach/mach.h>
#include <mach/mach_error.h>
#include <stdint.h>

static kern_return_t available_memory(uint64_t *bytes) {
    vm_statistics64_data_t stats;
    mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
    vm_size_t page_size;
    mach_port_t host = mach_host_self();
    kern_return_t result = host_page_size(host, &page_size);
    if (result == KERN_SUCCESS)
        result = host_statistics64(host, HOST_VM_INFO64, (host_info64_t)&stats, &count);
    mach_port_deallocate(mach_task_self(), host);
    if (result == KERN_SUCCESS) {
        // Inactive pages can be reclaimed. Purgeable pages overlap the VM
        // queues, so adding purgeable_count would count some pages twice.
        *bytes = ((uint64_t)stats.free_count + stats.inactive_count) * page_size;
    }
    return result;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// AvailableMemoryMB returns free and reclaimable inactive physical pages in
// MiB. Compressed memory and swap are excluded from the boot memory budget.
func AvailableMemoryMB() (uint64, error) {
	var available C.uint64_t
	if code := C.available_memory(&available); code != C.KERN_SUCCESS {
		return 0, fmt.Errorf("读取 macOS 可用内存: %s (%d)", C.GoString(C.mach_error_string(code)), code)
	}
	return uint64(available) >> 20, nil
}

// DiskFree returns the filesystem bytes available to the caller on the volume
// containing path, using its nearest existing ancestor when path is new.
func DiskFree(path string) (uint64, error) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("读取磁盘路径 %s: %w", path, err)
	}
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("读取磁盘路径 %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, fmt.Errorf("找不到 %s 所在的磁盘: %w", path, err)
		}
		dir = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("读取 %s 所在磁盘的剩余空间: %w", dir, err)
	}
	blockSize := uint64(stat.Bsize)
	if blockSize == 0 || stat.Bavail > math.MaxUint64/blockSize {
		return 0, fmt.Errorf("%s 所在磁盘返回了无效的空间信息", dir)
	}
	return stat.Bavail * blockSize, nil
}
