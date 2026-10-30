//go:build windows

package emu

// The Vulkan check: which GPUs Vulkan can use on this host. Port of AUTO-MAS app/utils/windows/vulkan_probe.py (the
// probe) and precheck.py probe_vulkan / vulkan_item_from_probe (the judgement).
//
// The probe runs in a child process (`modavd __vkprobe`): vulkan-1.dll loads the GPU drivers' ICDs into the process,
// and a broken driver may crash or hang it. Read only: vkCreateInstance / vkEnumeratePhysicalDevices /
// vkGetPhysicalDeviceProperties / vkDestroyInstance, nothing is installed or changed.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/win"
)

// VulkanProbeArg is the hidden subcommand the CLI routes to RunVulkanProbe.
const VulkanProbeArg = "__vkprobe"

const (
	vkStructureTypeApplicationInfo    = 0
	vkStructureTypeInstanceCreateInfo = 1
	vkAPIVersion10                    = 1 << 22 // VK_API_VERSION_1_0: every loader accepts it
	// VkPhysicalDeviceProperties is ~824 bytes with its limits; a generous buffer, only the head is read.
	vkPropertiesBufferBytes = 4096
	vkDeviceTypeOffset      = 16
	vkDeviceNameOffset      = 20
	vkDeviceNameSize        = 256
	// VkPhysicalDeviceType: 4 = CPU (a software implementation such as SwiftShader / lavapipe); the others
	// (integrated, discrete, virtual GPU, other) count as hardware.
	vkDeviceTypeCPU = 4
	// LoadLibraryExW flag: only System32, where the Vulkan loader is installed, so a vulkan-1.dll planted next to
	// modavd.exe or in the working directory is never loaded.
	loadLibrarySearchSystem32 = 0x00000800
)

var vkDeviceTypeNames = map[int]string{0: "其他", 1: "集成显卡", 2: "独立显卡", 3: "虚拟显卡", 4: "CPU"}

// vkProbeOutput is the JSON line of the probe (same fields as vulkan_probe.py).
type vkProbeOutput struct {
	Loaded  bool       `json:"loaded"`
	Result  int32      `json:"result"`
	Devices []vkDevice `json:"devices"`
	Error   string     `json:"error"`
}

type vkDevice struct {
	Name       string `json:"name"`
	Type       int32  `json:"type"`
	APIVersion string `json:"apiVersion"`
	VendorID   uint32 `json:"vendorId"`
}

// VkApplicationInfo / VkInstanceCreateInfo with C layout (Go aligns these fields the same way on amd64 and 386).
type vkApplicationInfo struct {
	sType              uint32
	pNext              uintptr
	pApplicationName   *byte
	applicationVersion uint32
	pEngineName        *byte
	engineVersion      uint32
	apiVersion         uint32
}

type vkInstanceCreateInfo struct {
	sType                   uint32
	pNext                   uintptr
	flags                   uint32
	pApplicationInfo        *vkApplicationInfo
	enabledLayerCount       uint32
	ppEnabledLayerNames     uintptr
	enabledExtensionCount   uint32
	ppEnabledExtensionNames uintptr
}

// RunVulkanProbe is the hidden subcommand `modavd __vkprobe`: load vulkan-1.dll, enumerate physical devices, print
// one JSON line {"loaded":bool,"error":str,"result":int,"devices":[{"name","type","apiVersion"}]} and return the
// exit code. Run in a child process so a crashing / hanging driver cannot take modavd down.
//
// Devices also carry "vendorId" like vulkan_probe.py. The exit code is 0 whenever the line was written (the JSON says
// what was found), 1 when it could not be written.
func RunVulkanProbe(w io.Writer) int {
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false) // device names as written (json.dumps(ensure_ascii=False))
	if err := enc.Encode(probeVulkanDevices()); err != nil {
		return 1
	}
	if _, err := w.Write(line.Bytes()); err != nil {
		return 1
	}
	return 0
}

// winErrorText is Python's str(OSError) of a Win32 error: "[WinError 126] The specified module could not be found."
func winErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[WinError %d] %s", uint32(errno), errno.Error())
	}
	return err.Error()
}

func vkVersionText(raw uint32) string {
	return fmt.Sprintf("%d.%d.%d", (raw>>22)&0x7F, (raw>>12)&0x3FF, raw&0xFFF)
}

// vkParseProperties reads the head of VkPhysicalDeviceProperties.
func vkParseProperties(b []byte) vkDevice {
	name := b[vkDeviceNameOffset : vkDeviceNameOffset+vkDeviceNameSize]
	if n := bytes.IndexByte(name, 0); n >= 0 {
		name = name[:n]
	}
	return vkDevice{
		Name:       strings.ToValidUTF8(string(name), "�"),
		Type:       int32(binary.LittleEndian.Uint32(b[vkDeviceTypeOffset:])),
		APIVersion: vkVersionText(binary.LittleEndian.Uint32(b[0:])),
		VendorID:   binary.LittleEndian.Uint32(b[8:]),
	}
}

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

// ---- parent side ---------------------------------------------------------------------------------------------------

// vkProbe is a probe result as judged by vulkanItemFromProbe. ProbeError set = the probe could not run or gave no
// result (AUTO-MAS {"probeError": ...}).
type vkProbe struct {
	ProbeError string
	TimedOut   bool
	Loaded     bool
	Error      string
	Result     *int64
	Devices    []vkProbeDevice
}

type vkProbeDevice struct {
	Name       string
	Type       *int
	APIVersion string
}

// parseProbeOutput reads the last non-empty stdout line of the child.
func parseProbeOutput(stdout []byte, exitCode int, stderr []byte) vkProbe {
	var last string
	for _, line := range strings.Split(strings.ToValidUTF8(string(stdout), "�"), "\n") {
		if t := strings.TrimRight(line, "\r"); t != "" {
			last = t
		}
	}
	var raw struct {
		Loaded  bool   `json:"loaded"`
		Error   string `json:"error"`
		Result  *int64 `json:"result"`
		Devices []struct {
			Name       string `json:"name"`
			Type       *int   `json:"type"`
			APIVersion string `json:"apiVersion"`
		} `json:"devices"`
	}
	if last == "" || json.Unmarshal([]byte(last), &raw) != nil {
		msg := fmt.Sprintf("探测进程退出码 %d，没有结果", exitCode)
		if t := tail(strings.ToValidUTF8(string(stderr), "�"), 300); t != "" {
			msg += "：" + t
		}
		return vkProbe{ProbeError: msg}
	}
	p := vkProbe{Loaded: raw.Loaded, Error: raw.Error, Result: raw.Result}
	for _, d := range raw.Devices {
		p.Devices = append(p.Devices, vkProbeDevice{Name: d.Name, Type: d.Type, APIVersion: d.APIVersion})
	}
	return p
}

var vulkanCache struct {
	sync.Mutex
	until time.Time
	res   vkProbe
}

// probeVulkan runs `<this exe> __vkprobe` (30 s, no console window). Cached like AUTO-MAS _remember_vulkan: a usable
// result for 10 minutes, a timeout for 1 minute (a hanging driver should not cost every check and boot 30 s);
// "no Vulkan" clears the cache, so a freshly installed driver shows up at once. A probe that could not run or gave
// no result leaves the cache as it was (probe_vulkan returns before _remember_vulkan then).
func (m *Manager) probeVulkan(ctx context.Context, useCache bool) vkProbe {
	if useCache {
		vulkanCache.Lock()
		cached, until := vulkanCache.res, vulkanCache.until
		vulkanCache.Unlock()
		if time.Now().Before(until) {
			return cached
		}
	}
	res := runVulkanProbeChild(ctx)
	if ctx.Err() != nil {
		return res
	}
	if ttl, remember := vulkanCacheTTL(res); remember {
		vulkanCache.Lock()
		vulkanCache.res, vulkanCache.until = res, time.Now().Add(ttl)
		vulkanCache.Unlock()
	}
	return res
}

// vulkanCacheTTL: how long res is cached (0 = clear the cache); remember=false leaves the cache untouched.
func vulkanCacheTTL(res vkProbe) (ttl time.Duration, remember bool) {
	switch {
	case res.TimedOut:
		return vulkanTimeoutCacheFor, true
	case res.ProbeError != "":
		return 0, false
	}
	if ok := vulkanItemFromProbe(res).OK; ok != nil && *ok {
		return vulkanCacheFor, true
	}
	return 0, true
}

// vulkanProbeArgv is the probe's command line: this executable with VulkanProbeArg (tests point it at the test
// binary).
var vulkanProbeArgv = func() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, VulkanProbeArg}, nil
}

func runVulkanProbeChild(ctx context.Context) vkProbe {
	argv, err := vulkanProbeArgv()
	if err != nil {
		return vkProbe{ProbeError: err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, vulkanProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() == nil && cctx.Err() != nil {
		return vkProbe{ProbeError: fmt.Sprintf("探测超时（%.0f 秒）", vulkanProbeTimeout.Seconds()), TimedOut: true}
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return vkProbe{ProbeError: err.Error()}
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return parseProbeOutput(stdout.Bytes(), code, stderr.Bytes())
}

// vulkanItemFromProbe judges a probe result (AUTO-MAS vulkan_item_from_probe). Never blocking.
func vulkanItemFromProbe(r vkProbe) CheckItem {
	const title = "显卡 Vulkan"
	if r.ProbeError != "" {
		return CheckItem{ID: "vulkan", Title: title, OK: nil, Reason: "查不了 Vulkan：" + r.ProbeError}
	}
	if !r.Loaded {
		why := r.Error
		if why == "" {
			why = "未知"
		}
		return CheckItem{
			ID: "vulkan", Title: title, OK: checkOK(false),
			Reason: fmt.Sprintf("这台电脑没有 Vulkan 运行库（vulkan-1.dll 加载失败：%s）", why),
			Advice: vulkanAdvice,
		}
	}
	if r.Result != nil && *r.Result != 0 && len(r.Devices) == 0 {
		what := r.Error
		if what == "" {
			what = "Vulkan"
		}
		return CheckItem{
			ID: "vulkan", Title: title, OK: checkOK(false),
			Reason: fmt.Sprintf("没有支持 Vulkan 的显卡驱动（%s 返回 %d）", what, *r.Result),
			Advice: vulkanAdvice,
		}
	}
	hardware := 0
	listed := make([]string, len(r.Devices))
	for k, d := range r.Devices {
		typeName := "未知"
		if d.Type != nil {
			if n, ok := vkDeviceTypeNames[*d.Type]; ok {
				typeName = n
			}
		}
		if d.Type == nil || *d.Type != vkDeviceTypeCPU {
			hardware++
		}
		listed[k] = fmt.Sprintf("%s（%s，Vulkan %s）", d.Name, typeName, d.APIVersion)
	}
	all := strings.Join(listed, "、")
	if hardware > 0 {
		return CheckItem{ID: "vulkan", Title: title, OK: checkOK(true), Reason: "可用显卡：" + all}
	}
	reason := "Vulkan 没有枚举到任何显卡"
	if len(r.Devices) > 0 {
		reason = fmt.Sprintf("Vulkan 只有软件实现（%s），没有可用的显卡", all)
	}
	return CheckItem{ID: "vulkan", Title: title, OK: checkOK(false), Reason: reason, Advice: vulkanAdvice}
}
