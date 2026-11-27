// Package guestpatch lists the guest library patches modavd bind-mounts after boot (internal/emu/guestfix.go,
// internal/guest/scripts/libfix.sh): per library its path in the image, the hash it must have there, and the patched
// copy made from it on the user's machine (the patched files are never distributed).
package guestpatch

import (
	"crypto/sha256"
	"encoding/hex"

	"modavd/internal/ndkfix"
	"modavd/internal/vkfix"
)

// SHA256Hex is the lower-case hex sha256 the profiles and libfix.sh compare.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Profile keeps one verified guest library's byte contract together. An ABI alone does not make a different
// Android release compatible with a binary patch: the image's file has to have OrigSHA256.
type Profile struct {
	Name string // the patch, in messages: "客体 Vulkan 驱动补丁"
	Lib  string // the library, in messages: "客体驱动库"

	GuestPath  string // the image's library, where the patched copy is mounted
	CachePath  string // the guest's cached patched copy (relabelled like GuestPath, so adb cannot write it)
	UploadPath string // where the host pushes the patched copy when the guest has no valid cache
	CacheName  string // file name of the host's cached patched copy in <root>\guest-patches

	OrigSHA256 string
	FixSHA256  string
	Patch      func([]byte) ([]byte, error)
}

// guestCacheDir holds the guest caches, next to the scripts.
const guestCacheDir = "/data/local/tmp/"

// Vulkan is the guest Vulkan encoder fix (khrfix + bindlock2 + template rollover, internal/vkfix). Its upload name is
// AUTO-MAS's vulkanfix.sh's; the cache name and hash are not (AUTO-MAS's library lacks the template fix), so a library
// AUTO-MAS mounted reads as ALREADY with another hash and is left alone until the next boot.
func Vulkan() Profile {
	return Profile{Name: "客体 Vulkan 驱动补丁", Lib: "客体驱动库",
		GuestPath: vkfix.GuestPath, CachePath: guestCacheDir + vkfix.CacheName, UploadPath: vkfix.UploadPath,
		CacheName: vkfix.CacheName, OrigSHA256: vkfix.OrigSHA256, FixSHA256: vkfix.FixSHA256, Patch: vkfix.Patch}
}

// NDK is the ARM translation layer fix (WFE / WFI / SEV / SEVL as NOP, internal/ndkfix; ModAVD/ModAVD#131).
func NDK() Profile {
	return Profile{Name: "ARM 转译层补丁", Lib: "ARM 转译层库",
		GuestPath: ndkfix.GuestPath, CachePath: guestCacheDir + ndkfix.CacheName, UploadPath: ndkfix.UploadPath,
		CacheName: ndkfix.CacheName, OrigSHA256: ndkfix.OrigSHA256, FixSHA256: ndkfix.FixSHA256, Patch: ndkfix.Patch}
}

// ForABI selects the patches for the instance's guest ABI, independently of the manager's host architecture. Only
// the API34 x86_64 image's libraries are verified. An ARM64 guest runs ARM code natively (no translation layer), and
// the Mac API35 image has no /vendor/lib64/libvulkan_enc.so; its different Vulkan HAL must not inherit either the
// retired API34 ARM64 edits or the x86 patch. nil: nothing applies.
func ForABI(abi string) []Profile {
	switch abi {
	case "x86_64":
		return []Profile{Vulkan(), NDK()}
	default:
		return nil
	}
}
