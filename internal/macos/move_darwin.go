//go:build darwin && cgo

package macos

/*
#include <stdio.h>
#include <stdlib.h>
#include <errno.h>
static int modavd_rename_no_replace(const char *src, const char *dst) {
    if (renamex_np(src, dst, RENAME_EXCL) == 0) return 0;
    return errno;
}
*/
import "C"

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// RenameNoReplace atomically moves a file or directory on the same volume,
// refusing any destination that already exists, including a symbolic link.
func RenameNoReplace(src, dst string) error {
	if strings.IndexByte(src, 0) >= 0 || strings.IndexByte(dst, 0) >= 0 {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EINVAL}
	}
	a, b := C.CString(src), C.CString(dst)
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(b))
	if code := C.modavd_rename_no_replace(a, b); code != 0 {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.Errno(code)}
	}
	return nil
}
