//go:build !windows

package emu

func winErrorText(err error) string { return err.Error() }

func probeVulkanDevices() vkProbeOutput {
	return vkProbeOutput{Loaded: false, Devices: []vkDevice{}, Error: "当前平台暂不支持 Vulkan 探测"}
}
