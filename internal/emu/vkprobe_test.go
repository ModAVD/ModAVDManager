//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func vkTestInt64(v int64) *int64 { return &v }
func vkTestInt(v int) *int       { return &v }

func TestVulkanItemFromProbe(t *testing.T) {
	cases := []struct {
		name   string
		probe  vkProbe
		ok     *bool
		reason string
	}{
		{"probe error", vkProbe{ProbeError: "探测超时（30 秒）"}, nil, "查不了 Vulkan：探测超时（30 秒）"},
		{"no loader", vkProbe{Error: "找不到模块"}, checkOK(false), "这台电脑没有 Vulkan 运行库（vulkan-1.dll 加载失败：找不到模块）"},
		{"no loader, no reason", vkProbe{}, checkOK(false), "这台电脑没有 Vulkan 运行库（vulkan-1.dll 加载失败：未知）"},
		{"create failed", vkProbe{Loaded: true, Result: vkTestInt64(-9), Error: "vkCreateInstance"}, checkOK(false),
			"没有支持 Vulkan 的显卡驱动（vkCreateInstance 返回 -9）"},
		{"enumerate failed, no name", vkProbe{Loaded: true, Result: vkTestInt64(-3)}, checkOK(false),
			"没有支持 Vulkan 的显卡驱动（Vulkan 返回 -3）"},
		{"gpu", vkProbe{Loaded: true, Result: vkTestInt64(0), Devices: []vkProbeDevice{
			{Name: "NVIDIA GeForce RTX 4070", Type: vkTestInt(2), APIVersion: "1.4.303"},
			{Name: "llvmpipe", Type: vkTestInt(4), APIVersion: "1.3.0"},
		}}, checkOK(true), "可用显卡：NVIDIA GeForce RTX 4070（独立显卡，Vulkan 1.4.303）、llvmpipe（CPU，Vulkan 1.3.0）"},
		{"incomplete still lists", vkProbe{Loaded: true, Result: vkTestInt64(5), Devices: []vkProbeDevice{
			{Name: "Intel", Type: vkTestInt(1), APIVersion: "1.3.1"},
		}}, checkOK(true), "可用显卡：Intel（集成显卡，Vulkan 1.3.1）"},
		{"software only", vkProbe{Loaded: true, Result: vkTestInt64(0), Devices: []vkProbeDevice{
			{Name: "SwiftShader", Type: vkTestInt(4), APIVersion: "1.3.0"},
		}}, checkOK(false), "Vulkan 只有软件实现（SwiftShader（CPU，Vulkan 1.3.0）），没有可用的显卡"},
		{"unknown type counts as hardware", vkProbe{Loaded: true, Devices: []vkProbeDevice{
			{Name: "X", Type: vkTestInt(9), APIVersion: "1.0.0"},
		}}, checkOK(true), "可用显卡：X（未知，Vulkan 1.0.0）"},
		{"no devices", vkProbe{Loaded: true, Result: vkTestInt64(0)}, checkOK(false), "Vulkan 没有枚举到任何显卡"},
	}
	for _, c := range cases {
		it := vulkanItemFromProbe(c.probe)
		if it.ID != "vulkan" || it.Blocking {
			t.Errorf("%s: id/blocking wrong: %+v", c.name, it)
		}
		if (it.OK == nil) != (c.ok == nil) || (it.OK != nil && *it.OK != *c.ok) {
			t.Errorf("%s: ok = %v, want %v", c.name, it.OK, c.ok)
		}
		if it.Reason != c.reason {
			t.Errorf("%s: reason\n got %s\nwant %s", c.name, it.Reason, c.reason)
		}
		if it.OK != nil && !*it.OK && it.Advice != vulkanAdvice {
			t.Errorf("%s: a failure needs the driver advice", c.name)
		}
	}
}

func TestParseProbeOutput(t *testing.T) {
	out := []byte("noise\r\n{\"loaded\": true, \"result\": 0, \"devices\": [{\"name\": \"GPU\", \"type\": 2, \"apiVersion\": \"1.3.0\", \"vendorId\": 4318}], \"error\": \"\"}\r\n\r\n")
	p := parseProbeOutput(out, 0, nil)
	if p.ProbeError != "" || !p.Loaded || p.Result == nil || *p.Result != 0 || len(p.Devices) != 1 || p.Devices[0].Name != "GPU" || *p.Devices[0].Type != 2 {
		t.Fatalf("parsed = %+v", p)
	}
	p = parseProbeOutput([]byte("not json\n"), 3, []byte("panic: boom\n"))
	if p.ProbeError != "探测进程退出码 3，没有结果：panic: boom" {
		t.Fatalf("garbage = %q", p.ProbeError)
	}
	if p := parseProbeOutput(nil, -1, nil); p.ProbeError != "探测进程退出码 -1，没有结果" {
		t.Fatalf("empty = %q", p.ProbeError)
	}
}

func TestVulkanCacheTTL(t *testing.T) {
	gpu := vkProbe{Loaded: true, Result: vkTestInt64(0), Devices: []vkProbeDevice{{Name: "GPU", Type: vkTestInt(2), APIVersion: "1.3.0"}}}
	cases := []struct {
		name     string
		probe    vkProbe
		ttl      time.Duration
		remember bool
	}{
		{"usable", gpu, vulkanCacheFor, true},
		{"timeout", vkProbe{ProbeError: "探测超时（30 秒）", TimedOut: true}, vulkanTimeoutCacheFor, true},
		{"no vulkan clears", vkProbe{Error: "x"}, 0, true},
		{"software only clears", vkProbe{Loaded: true, Devices: []vkProbeDevice{{Name: "SwiftShader", Type: vkTestInt(4)}}}, 0, true},
		{"no result keeps", vkProbe{ProbeError: "探测进程退出码 1，没有结果"}, 0, false},
	}
	for _, c := range cases {
		ttl, remember := vulkanCacheTTL(c.probe)
		if ttl != c.ttl || remember != c.remember {
			t.Errorf("%s: got %v %v, want %v %v", c.name, ttl, remember, c.ttl, c.remember)
		}
	}
}

func TestWinErrorText(t *testing.T) {
	if got := winErrorText(syscall.Errno(126)); !strings.HasPrefix(got, "[WinError 126] ") || len(got) <= len("[WinError 126] ") {
		t.Fatalf("errno = %q", got)
	}
	if got := winErrorText(errors.New("x")); got != "x" {
		t.Fatalf("other = %q", got)
	}
}

func TestVkParseProperties(t *testing.T) {
	b := make([]byte, vkPropertiesBufferBytes)
	binary.LittleEndian.PutUint32(b[0:], 1<<22|3<<12|289) // 1.3.289
	binary.LittleEndian.PutUint32(b[8:], 0x10de)
	binary.LittleEndian.PutUint32(b[16:], 2)
	copy(b[20:], "NVIDIA GeForce RTX 4070\x00garbage")
	d := vkParseProperties(b)
	if d.Name != "NVIDIA GeForce RTX 4070" || d.Type != 2 || d.APIVersion != "1.3.289" || d.VendorID != 0x10de {
		t.Fatalf("parsed = %+v", d)
	}
	if got := vkVersionText(0); got != "0.0.0" {
		t.Fatalf("vkVersionText(0) = %q", got)
	}
}

// hostHasVulkan: the probe tests below need a Vulkan driver with a device (the dev machine has an NVIDIA GPU; CI
// runners have no GPU and vkCreateInstance fails). Only a well-formed "no driver / no device" answer counts as
// absent; anything unparsable returns true so the test runs and reports it.
func hostHasVulkan() bool {
	var buf bytes.Buffer
	RunVulkanProbe(&buf)
	var out vkProbeOutput
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &out); err != nil {
		return true
	}
	return out.Loaded && out.Result == 0 && len(out.Devices) > 0
}

// TestRunVulkanProbe runs the probe in this process (skipped on a host without a Vulkan driver).
func TestRunVulkanProbe(t *testing.T) {
	if !hostHasVulkan() {
		t.Skip("no Vulkan driver with a device on this host")
	}
	var buf bytes.Buffer
	if code := RunVulkanProbe(&buf); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	line := strings.TrimSpace(buf.String())
	t.Logf("probe: %s", line)
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", buf.String())
	}
	var out vkProbeOutput
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(line, `"devices":[`) {
		t.Fatalf("devices must be an array: %s", line)
	}
	if !out.Loaded || out.Result != 0 || len(out.Devices) == 0 {
		t.Fatalf("expected a loaded loader with devices: %+v", out)
	}
	hardware := false
	for _, d := range out.Devices {
		if d.Name == "" || d.APIVersion == "" || d.APIVersion == "0.0.0" {
			t.Errorf("device not filled in: %+v", d)
		}
		if d.Type != vkDeviceTypeCPU {
			hardware = true
		}
	}
	if !hardware {
		t.Fatalf("no hardware device found: %+v", out.Devices)
	}
	it := vulkanItemFromProbe(parseProbeOutput(buf.Bytes(), 0, nil))
	if it.OK == nil || !*it.OK {
		t.Fatalf("judged as %+v", it)
	}
	t.Logf("item: %s", it.Reason)
}

// TestVulkanProbeHelperProcess is the child of TestRunVulkanProbeChild (the test binary standing in for
// `modavd __vkprobe`); it does nothing in a normal run.
func TestVulkanProbeHelperProcess(t *testing.T) {
	if os.Getenv("MODAVD_TEST_VKPROBE_CHILD") != "1" {
		t.Skip("helper process only")
	}
	os.Exit(RunVulkanProbe(os.Stdout))
}

func TestRunVulkanProbeChild(t *testing.T) {
	if !hostHasVulkan() {
		t.Skip("no Vulkan driver with a device on this host")
	}
	saved := vulkanProbeArgv
	defer func() { vulkanProbeArgv = saved }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vulkanProbeArgv = func() ([]string, error) {
		return []string{exe, "-test.run=^TestVulkanProbeHelperProcess$"}, nil
	}
	t.Setenv("MODAVD_TEST_VKPROBE_CHILD", "1")
	p := runVulkanProbeChild(context.Background())
	if p.ProbeError != "" || !p.Loaded || len(p.Devices) == 0 {
		t.Fatalf("child probe = %+v", p)
	}
	it := vulkanItemFromProbe(p)
	if it.OK == nil || !*it.OK {
		t.Fatalf("judged as %+v", it)
	}

	// A child that prints nothing useful.
	vulkanProbeArgv = func() ([]string, error) {
		return []string{exe, "-test.run=^$"}, nil
	}
	if p := runVulkanProbeChild(context.Background()); p.ProbeError == "" || !strings.HasPrefix(p.ProbeError, "探测进程退出码 0，没有结果") {
		t.Fatalf("no result = %+v", p)
	}
	// A child that cannot start.
	vulkanProbeArgv = func() ([]string, error) {
		return []string{`C:\no-such-dir-modavd\modavd.exe`, VulkanProbeArg}, nil
	}
	if p := runVulkanProbeChild(context.Background()); p.ProbeError == "" {
		t.Fatalf("missing exe = %+v", p)
	}
}
