// Package vkfix builds the patched guest Vulkan encoder (bindlock2, includes khrfix) from the image's own
// /vendor/lib64/libvulkan_enc.so on the user's machine; the patched file is never distributed.
//
// Byte-for-byte port of the companion Vulkan fix implementation: khrfix jumps for
// vkUpdateDescriptorSetWithTemplateKHR, and lock wrappers around
// ResourceTracker::on_vkBindImageMemory(2) placed in the freed dynCheck function body.
//
//  1. khrfix (09-30, Star Rail normal mode): the two vkUpdateDescriptorSetWithTemplateKHR entries call the raw
//     encoder, which sends only 1 byte of the untyped pData. Each becomes a 5-byte jmp to the core entry, which
//     linearizes the template data first.
//  2. bindlock (10-16, Endfield crash): on_vkBindImageMemory(2) look up info_VkDeviceMemory / info_VkImage without
//     mLock; a rehash by another thread makes a valid handle disappear and the call returns
//     VK_ERROR_OUT_OF_DEVICE_MEMORY. The success exits of both implementations now return a marker (magic); two
//     wrappers in the dead dynCheck body take the lock, call the implementation (memory reads only, never blocks),
//     drop the lock and, on the marker, call the encoder with the original exit's arguments. The lock is never held
//     across a host round trip (version 1 did, and deadlocked against VulkanQueueSubmitWithCommands' global sequence
//     number in 3 of 10 cold boots).
package vkfix

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"modavd/internal/errs"
)

const (
	// The image's library (API 34 google_apis x86_64) and the patched one.
	OrigSHA256 = "898c0bbc38077e4b19d112f6c66e2970f1e015fc440fae091974b8f06e40fefb"
	FixSHA256  = "d5258af6244679fd5ce50245b7dcc1f645d971a04f7e64fc9872fcfb9faa7ecb"
	// Where the guest library lives, and the upload path vulkanfix.sh expects (u= in the script).
	GuestPath  = "/vendor/lib64/libvulkan_enc.so"
	UploadPath = "/data/local/tmp/vulkanfix-upload.so"
	// File name of the cached patched library in <root>\guest-patches.
	CacheName = "libvulkan_enc.khrfix-bindlock2.so"
)

// Virtual addresses, from bindlock.py (see the meaning there).
const (
	textDelta = 0x1000 // .text: vaddr 0x1fcee0, file offset 0x1fbee0

	khrCore            = 0x3D8FA0 // entry_vkUpdateDescriptorSetWithTemplate (the khrfix jump target)
	khrEntry           = 0x3DA5B0
	khrDyn, khrDynEnd  = 0x3B6ED0, 0x3B7000 // int3 padding after the body up to 0x3b7000
	lockPLT, unlockPLT = 0x3E7910, 0x3E7940 // pthread_mutex_lock@plt / pthread_mutex_unlock@plt
	encBind, encBind2  = 0x3E8300, 0x3E8320 // VkEncoder::vkBindImageMemory(2)@plt
	implBind           = 0x212090
	implBind2          = 0x2122D0
	fwdBind            = 0x212080
	fwdBind2           = 0x2122C0
	fwdBind2KHR        = 0x212570
	magic              = 0x4D415332 // 'MAS2'; the implementations otherwise only return -2

	// Success exits of the two implementations (original bytes up to the jmp to the encoder).
	exitBind  = 0x21228E
	exitBind2 = 0x21254A
)

var (
	fwdBytes      = mustHex("488b7f08e9") // mov 0x8(%rdi),%rdi; jmp rel32
	exitBindOrig  = mustHex("4889f7 4889ce 4c89c2 4c89c9 4d89d0 41b901000000 5b 415c 415d 415e 415f 5d e94e601d00")
	exitBind2Orig = mustHex("4889f7 488b7424f8 4489c2 4c89c9 41b801000000 5b 415c 415d 415e 415f 5d e9b35d1d00")
	// mov $magic,%eax; pop %rbx; pop %r12; pop %r13; pop %r14; pop %r15; pop %rbp; ret
	exitMagic = append(append([]byte{0xb8}, le32(magic)...), mustHex("5b 415c 415d 415e 415f 5d c3")...)
)

// mustHex is bytes.fromhex: whitespace between the byte pairs is ignored.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic("vkfix: bad hex constant " + s)
	}
	return b
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// rel32 is struct.pack("<i", v); the targets are fixed, so an overflow is a bug in this file.
func rel32(v int) []byte {
	if int(int32(v)) != v {
		panic(fmt.Sprintf("vkfix: rel32 out of range: %d", v))
	}
	return le32(uint32(int32(v)))
}

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// asm assembles at virtual address base.
type asm struct {
	base int
	buf  []byte
}

func (a *asm) here() int { return a.base + len(a.buf) }

func (a *asm) raw(s string) { a.buf = append(a.buf, mustHex(s)...) }

// call emits call rel32; the displacement is relative to the end of the 5-byte instruction.
func (a *asm) call(target int) {
	rel := rel32(target - (a.here() + 5))
	a.buf = append(append(a.buf, 0xe8), rel...)
}

func prologueLock(a *asm, frame int, save string) {
	a.raw("53 55 4154 4155 4156 4157")      // push rbx, rbp, r12, r13, r14, r15
	a.raw(fmt.Sprintf("4883ec%02x", frame)) // sub rsp, frame (rsp is 16-byte aligned for the calls)
	a.raw("488b5f08")                       // mov rbx, [rdi+8]   ; Impl*
	a.raw(save)                             // keep the arguments in callee-saved registers
	a.raw("488dbb60060000")                 // lea rdi, [rbx+0x660]  ; &Impl::mLock
	a.call(lockPLT)
}

// unlockCheck returns the index of the jne rel8 that epilogue fills in.
func unlockCheck(a *asm) int {
	a.raw("4189c5")         // mov r13d, eax      ; the implementation's result
	a.raw("488dbb60060000") // lea rdi, [rbx+0x660]
	a.call(unlockPLT)
	a.raw("4489e8")                               // mov eax, r13d      ; returned as is unless it is magic (-2 or magic)
	a.raw("3d" + hex.EncodeToString(le32(magic))) // cmp eax, magic
	a.raw("7500")                                 // jne .out           ; rel8 set by epilogue
	return len(a.buf) - 1
}

func epilogue(a *asm, frame, rel8At int) error {
	rel := len(a.buf) - (rel8At + 1) // .out is here; the encoder's result is already in eax
	if !(0 < rel && rel < 0x80) {
		return errs.Newf(errs.Unsupported, "jne 跳不到（%d）", rel)
	}
	a.buf[rel8At] = byte(rel)
	a.raw(fmt.Sprintf("4883c4%02x", frame)) // .out: add rsp, frame
	a.raw("415f 415e 415d 415c 5d 5b c3")   // pop r15, r14, r13, r12, rbp, rbx; ret
	return nil
}

func wrapperBind(base int) ([]byte, error) {
	// on_vkBindImageMemory(this, context, input_result, device, image, memory, memoryOffset[stack])
	// kept: r12=context r14=device r15=image rbp=memory; memoryOffset is at [rsp+0x50] ([rsp+8] on entry)
	a := &asm{base: base}
	prologueLock(a, 0x18, "4989f4 4989ce 4d89c7 4c89cd")
	a.raw("488b442450 48890424")                // mov rax,[rsp+0x50]; mov [rsp],rax ; 7th argument of the implementation
	a.raw("4889df 4c89e6 4c89f1 4d89f8 4989e9") // rdi=Impl rsi=context rcx=device r8=image r9=memory
	a.call(implBind)
	rel8 := unlockCheck(a)
	// as the original exit: vkBindImageMemory(enc=context, device, image, memory, memoryOffset, doLock=1)
	a.raw("4c89e7 4c89f6 4c89fa 4889e9 4c8b442450 41b901000000")
	a.call(encBind)
	if err := epilogue(a, 0x18, rel8); err != nil {
		return nil, err
	}
	return a.buf, nil
}

func wrapperBind2(base int) ([]byte, error) {
	// on_vkBindImageMemory2(this, context, input_result, device, bindingCount, pBindInfos): all in registers
	// kept: r12=context r14=device r15=bindingCount rbp=pBindInfos
	a := &asm{base: base}
	prologueLock(a, 0x08, "4989f4 4989ce 4d89c7 4c89cd")
	a.raw("4889df 4c89e6 4c89f1 4d89f8 4989e9") // rdi=Impl rsi=context rcx=device r8=bindingCount r9=pBindInfos
	a.call(implBind2)
	rel8 := unlockCheck(a)
	// as the original exit: vkBindImageMemory2(enc=context, device, bindingCount, pBindInfos, doLock=1)
	a.raw("4c89e7 4c89f6 4489fa 4889e9 41b801000000")
	a.call(encBind2)
	if err := epilogue(a, 0x08, rel8); err != nil {
		return nil, err
	}
	return a.buf, nil
}

func jmp(src, dst int) []byte { return append([]byte{0xe9}, rel32(dst-(src+5))...) }

// Patch returns the patched library. errs.Unsupported when the input's hash, any original byte sequence or the
// result's hash does not match.
func Patch(original []byte) ([]byte, error) {
	if digest := SHA256Hex(original); digest != OrigSHA256 {
		return nil, errs.Newf(errs.Unsupported, "客体驱动库 sha256 %s 不是预期版本", digest)
	}
	data := bytes.Clone(original)

	put := func(va int, blob, expect []byte, name string) error {
		off := va - textDelta
		if off < 0 || off+len(blob) > len(data) {
			// Python would grow the buffer here; with the input hash checked this cannot happen
			return errs.Newf(errs.Unsupported, "%s（0x%x）超出库文件范围", name, va)
		}
		if expect != nil {
			// a short slice at the end of the file reads as a mismatch, like Python's slicing
			got := data[off:min(off+len(expect), len(data))]
			if !bytes.Equal(got, expect) {
				return errs.Newf(errs.Unsupported, "%s（0x%x）原字节不符: %s", name, va, hex.EncodeToString(got))
			}
		}
		copy(data[off:], blob)
		return nil
	}

	// 1. khrfix: both KHR entries jump to the core entry
	if err := put(khrEntry, jmp(khrEntry, khrCore), mustHex("4157415641554154"),
		"entry_vkUpdateDescriptorSetWithTemplateKHR"); err != nil {
		return nil, err
	}
	if err := put(khrDyn, jmp(khrDyn, khrCore), mustHex("5541574156415541"),
		"dynCheck_entry_vkUpdateDescriptorSetWithTemplateKHR"); err != nil {
		return nil, err
	}

	// 2. the success exits of both implementations return magic (the rest of the exit becomes int3)
	for _, e := range []struct {
		va   int
		orig []byte
		name string
	}{
		{exitBind, exitBindOrig, "on_vkBindImageMemory 实现出口"},
		{exitBind2, exitBind2Orig, "on_vkBindImageMemory2 实现出口"},
	} {
		blob := append(bytes.Clone(exitMagic), bytes.Repeat([]byte{0xcc}, len(e.orig)-len(exitMagic))...)
		if err := put(e.va, blob, e.orig, e.name); err != nil {
			return nil, err
		}
	}

	// 3. both wrappers go into the dead dynCheck body (the rest becomes int3)
	cave1 := khrDyn + 5
	w1, err := wrapperBind(cave1)
	if err != nil {
		return nil, err
	}
	cave2 := cave1 + len(w1)
	w2, err := wrapperBind2(cave2)
	if err != nil {
		return nil, err
	}
	if cave2+len(w2) > khrDynEnd {
		return nil, errs.Newf(errs.Unsupported, "包装函数（%d + %d 字节）放不进 %d 字节", len(w1), len(w2), khrDynEnd-cave1)
	}
	blob := append(append(bytes.Clone(w1), w2...), bytes.Repeat([]byte{0xcc}, khrDynEnd-cave2-len(w2))...)
	if err := put(cave1, blob, nil, "包装函数"); err != nil {
		return nil, err
	}

	// 4. the three forwarding entries jump to the wrappers (which load this->mImpl themselves)
	for _, f := range []struct {
		va, target int
		name       string
	}{
		{fwdBind, cave1, "ResourceTracker::on_vkBindImageMemory"},
		{fwdBind2, cave2, "ResourceTracker::on_vkBindImageMemory2"},
		{fwdBind2KHR, cave2, "ResourceTracker::on_vkBindImageMemory2KHR"},
	} {
		if err := put(f.va, jmp(f.va, f.target), fwdBytes, f.name); err != nil {
			return nil, err
		}
	}

	if digest := SHA256Hex(data); digest != FixSHA256 {
		return nil, errs.Newf(errs.Unsupported, "改完的库 sha256 %s 不是预期值", digest)
	}
	return data, nil
}
