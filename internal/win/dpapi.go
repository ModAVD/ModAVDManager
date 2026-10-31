//go:build windows

package win

import (
	"errors"
	"syscall"
	"unsafe"
)

// crypt32 is loaded by full System32 path: unlike kernel32 it is not on the syscall package's system-DLL list, and a
// crypt32.dll next to the exe must never be picked up.
var (
	procGetSystemDirectoryW = modkernel32.NewProc("GetSystemDirectoryW")
	procLocalFree           = modkernel32.NewProc("LocalFree")

	modcrypt32             = syscall.NewLazyDLL(systemDir() + `\crypt32.dll`)
	procCryptProtectData   = modcrypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = modcrypt32.NewProc("CryptUnprotectData")
)

func systemDir() string {
	buf := make([]uint16, 260)
	n, _, _ := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) >= len(buf) {
		return `C:\Windows\System32`
	}
	return syscall.UTF16ToString(buf[:n])
}

type dataBlob struct {
	n uint32
	p *byte
}

// blob is b as a DATA_BLOB; nil for no bytes (passed as a null pointer: an empty blob is "the parameter is
// incorrect"). Converted to uintptr only in the Call expression, so it stays alive for the call.
func blob(b []byte) *dataBlob {
	if len(b) == 0 {
		return nil
	}
	return &dataBlob{n: uint32(len(b)), p: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	if b.p == nil || b.n == 0 {
		return []byte{}
	}
	out := make([]byte, b.n)
	copy(out, unsafe.Slice(b.p, b.n))
	procLocalFree.Call(uintptr(unsafe.Pointer(b.p)))
	return out
}

const cryptprotectUIForbidden = 0x1

// Protect encrypts data (not empty) for the current Windows user (DPAPI CryptProtectData): only this user on this
// machine can decrypt it. entropy is an extra secret the caller passes to Unprotect again.
func Protect(data, entropy []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("CryptProtectData: nothing to protect")
	}
	var out dataBlob
	in, ent := blob(data), blob(entropy)
	r, _, e := procCryptProtectData.Call(uintptr(unsafe.Pointer(in)), 0, uintptr(unsafe.Pointer(ent)), 0, 0,
		cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptProtectData: " + e.Error())
	}
	return out.bytes(), nil
}

// Unprotect decrypts what Protect returned (same user, same entropy).
func Unprotect(data, entropy []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("CryptUnprotectData: empty input")
	}
	var out dataBlob
	in, ent := blob(data), blob(entropy)
	r, _, e := procCryptUnprotectData.Call(uintptr(unsafe.Pointer(in)), 0, uintptr(unsafe.Pointer(ent)), 0, 0,
		cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptUnprotectData: " + e.Error())
	}
	return out.bytes(), nil
}
