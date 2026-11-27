//go:build darwin && cgo

package macos

/*
#include <sys/xattr.h>
#include <stdlib.h>
static ssize_t signature_listxattr(const char *path, void *buf, size_t size) {
    return listxattr(path, buf, size, XATTR_NOFOLLOW);
}
static ssize_t signature_getxattr(const char *path, const char *name, void *buf, size_t size) {
    return getxattr(path, name, buf, size, 0, XATTR_NOFOLLOW);
}
static int signature_setxattr(const char *path, const char *name, const void *buf, size_t size) {
    return setxattr(path, name, buf, size, 0, XATTR_NOFOLLOW);
}
*/
import "C"

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unsafe"
)

var signatureAttrs = map[string]bool{
	"com.apple.cs.CodeDirectory": true, "com.apple.cs.CodeRequirements": true,
	"com.apple.cs.CodeSignature": true, "com.apple.cs.CodeEntitlements": true,
}

// SignatureXattrs reads the generic code-signature attributes ZIP cannot retain.
// Other metadata is omitted. Unknown code-signature attributes fail closed.
func SignatureXattrs(p string) (map[string]string, error) {
	cp := C.CString(p)
	defer C.free(unsafe.Pointer(cp))
	n, err := C.signature_listxattr(cp, nil, 0)
	if n < 0 || n > 65536 {
		return nil, fmt.Errorf("读取签名属性列表失败: %s (%v)", p, err)
	}
	names := make([]byte, int(n)+1)
	read, err := C.signature_listxattr(cp, unsafe.Pointer(&names[0]), C.size_t(n))
	if read != n {
		return nil, fmt.Errorf("签名属性列表发生变化: %s (%v)", p, err)
	}
	out := map[string]string{}
	for _, name := range strings.Split(string(names[:n]), "\x00") {
		if !strings.HasPrefix(name, "com.apple.cs.") {
			continue
		}
		if !signatureAttrs[name] {
			return nil, fmt.Errorf("不支持的代码签名属性: %s", name)
		}
		cn := C.CString(name)
		n, err := C.signature_getxattr(cp, cn, nil, 0)
		if n < 0 || n > 1<<20 {
			C.free(unsafe.Pointer(cn))
			return nil, fmt.Errorf("代码签名属性不可读或太大: %s (%v)", p, err)
		}
		b := make([]byte, int(n)+1)
		read, err := C.signature_getxattr(cp, cn, unsafe.Pointer(&b[0]), C.size_t(n))
		C.free(unsafe.Pointer(cn))
		if read != n {
			return nil, fmt.Errorf("代码签名属性发生变化: %s (%v)", p, err)
		}
		out[name] = base64.StdEncoding.EncodeToString(b[:n])
	}
	return out, nil
}

func ValidateSignatureXattrs(attrs map[string]string) error {
	for name, value := range attrs {
		if !signatureAttrs[name] || len(value) > (1<<20)*4/3+4 {
			return fmt.Errorf("不允许的签名扩展属性: %s", name)
		}
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(b) > 1<<20 {
			return fmt.Errorf("签名扩展属性格式错误: %s", name)
		}
	}
	return nil
}

// RestoreSignatureXattrs accepts only the bounded code-signature allowlist.
func RestoreSignatureXattrs(p string, attrs map[string]string) error {
	if err := ValidateSignatureXattrs(attrs); err != nil {
		return err
	}
	cp := C.CString(p)
	defer C.free(unsafe.Pointer(cp))
	for name, value := range attrs {
		b, _ := base64.StdEncoding.DecodeString(value)
		cn := C.CString(name)
		var ptr unsafe.Pointer
		if len(b) > 0 {
			ptr = unsafe.Pointer(&b[0])
		}
		result, err := C.signature_setxattr(cp, cn, ptr, C.size_t(len(b)))
		C.free(unsafe.Pointer(cn))
		if result != 0 {
			return fmt.Errorf("不能恢复签名属性 %s: %w", name, err)
		}
	}
	return nil
}
