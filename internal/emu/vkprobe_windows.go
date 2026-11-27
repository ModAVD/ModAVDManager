//go:build windows

package emu

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// winErrorText is Python's str(OSError): "[WinError 126] The specified module could not be found."
func winErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[WinError %d] %s", uint32(errno), errno.Error())
	}
	return err.Error()
}

const loadLibrarySearchSystem32 = 0x00000800

func probeVulkanDevices() vkProbeOutput {
	out := vkProbeOutput{Devices: []vkDevice{}}
	name, err := syscall.UTF16PtrFromString("vulkan-1.dll")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	h, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("LoadLibraryExW").Call(
		uintptr(unsafe.Pointer(name)), 0, loadLibrarySearchSystem32)
	if h == 0 {
		out.Error = winErrorText(callErr)
		return out
	}
	// Never unloaded (like vulkan_probe.py): some ICDs crash on unload, and the probe process exits right after.
	lib := syscall.Handle(h)

	procs := map[string]uintptr{}
	for _, fn := range []string{"vkCreateInstance", "vkEnumeratePhysicalDevices", "vkGetPhysicalDeviceProperties", "vkDestroyInstance"} {
		addr, err := syscall.GetProcAddress(lib, fn)
		if err != nil {
			// A vulkan-1.dll without the core entry points is no usable loader: reported as not loaded (vulkan_probe.py
			// dies with an AttributeError here, which only shows up as "no result").
			out.Error = "缺少 " + fn + "（" + winErrorText(err) + "）"
			return out
		}
		procs[fn] = addr
	}
	out.Loaded = true

	appName := []byte("modavd\x00")
	app := &vkApplicationInfo{
		sType:              vkStructureTypeApplicationInfo,
		pApplicationName:   &appName[0],
		applicationVersion: 1,
		pEngineName:        &appName[0],
		engineVersion:      1,
		apiVersion:         vkAPIVersion10,
	}
	info := &vkInstanceCreateInfo{sType: vkStructureTypeInstanceCreateInfo, pApplicationInfo: app}
	var instance uintptr
	r, _, _ := syscall.SyscallN(procs["vkCreateInstance"],
		uintptr(unsafe.Pointer(info)), 0, uintptr(unsafe.Pointer(&instance)))
	runtime.KeepAlive(info)
	runtime.KeepAlive(app)
	runtime.KeepAlive(appName)
	if res := int32(uint32(r)); res != 0 {
		out.Result = res
		out.Error = "vkCreateInstance"
		return out
	}
	defer syscall.SyscallN(procs["vkDestroyInstance"], instance, 0)

	var count uint32
	r, _, _ = syscall.SyscallN(procs["vkEnumeratePhysicalDevices"], instance, uintptr(unsafe.Pointer(&count)), 0)
	out.Result = int32(uint32(r))
	if out.Result != 0 || count == 0 {
		return out
	}
	handles := make([]uintptr, count)
	r, _, _ = syscall.SyscallN(procs["vkEnumeratePhysicalDevices"], instance,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&handles[0])))
	out.Result = int32(uint32(r))
	// uint64 words keep the buffer 8-aligned for the struct's 64-bit fields.
	buf := make([]uint64, vkPropertiesBufferBytes/8)
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), vkPropertiesBufferBytes)
	for _, dev := range handles[:min(int(count), len(handles))] {
		clear(buf)
		syscall.SyscallN(procs["vkGetPhysicalDeviceProperties"], dev, uintptr(unsafe.Pointer(&buf[0])))
		out.Devices = append(out.Devices, vkParseProperties(raw))
	}
	runtime.KeepAlive(buf)
	return out
}
