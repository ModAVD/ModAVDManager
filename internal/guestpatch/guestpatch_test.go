package guestpatch

import (
	"strings"
	"testing"

	"modavd/internal/ndkfix"
	"modavd/internal/vkfix"
)

// TestForABI: only the verified x86_64 image gets patches, both of them; ARM64 (native ARM code, an unvalidated
// Vulkan HAL), an empty and an unknown ABI get none.
func TestForABI(t *testing.T) {
	ps := ForABI("x86_64")
	if len(ps) != 2 || ps[0].GuestPath != vkfix.GuestPath || ps[1].GuestPath != ndkfix.GuestPath {
		t.Fatalf("x86_64: %+v", ps)
	}
	for _, abi := range []string{"arm64-v8a", "", "x86", "X86_64", "mips"} {
		if ps := ForABI(abi); ps != nil {
			t.Errorf("%q: %+v", abi, ps)
		}
	}
}

// TestProfiles: each profile carries its own package's contract, and every guest path is one libfix.sh can take
// (absolute, no blanks: the script splits its arguments on them).
func TestProfiles(t *testing.T) {
	vk, ndk := Vulkan(), NDK()
	if vk.OrigSHA256 != vkfix.OrigSHA256 || vk.FixSHA256 != vkfix.FixSHA256 || vk.CacheName != vkfix.CacheName ||
		vk.UploadPath != vkfix.UploadPath || vk.Patch == nil {
		t.Errorf("vulkan: %+v", vk)
	}
	if ndk.OrigSHA256 != ndkfix.OrigSHA256 || ndk.FixSHA256 != ndkfix.FixSHA256 || ndk.CacheName != ndkfix.CacheName ||
		ndk.UploadPath != ndkfix.UploadPath || ndk.Patch == nil {
		t.Errorf("ndk: %+v", ndk)
	}
	seen := map[string]bool{}
	for _, p := range []Profile{vk, ndk} {
		for _, s := range []string{p.GuestPath, p.CachePath, p.UploadPath} {
			if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t'\"") || seen[s] {
				t.Errorf("%s: path %q", p.Name, s)
			}
			seen[s] = true
		}
		if len(p.OrigSHA256) != 64 || len(p.FixSHA256) != 64 || p.Name == "" || p.Lib == "" {
			t.Errorf("%s: %+v", p.Name, p)
		}
	}
	// the patch refuses anything but its image's library
	if out, err := ndk.Patch([]byte("x")); out != nil || err == nil {
		t.Error("ndk patch accepted junk")
	}
	if SHA256Hex(nil) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Error("SHA256Hex")
	}
}
