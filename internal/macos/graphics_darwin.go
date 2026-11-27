//go:build darwin && cgo

package macos

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// MetalDevices lists the GPUs system_profiler reports as supporting Metal.
// This host diagnostic does not establish the emulator's Vulkan capabilities.
func MetalDevices(ctx context.Context) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	data, err := exec.CommandContext(cctx, "/usr/sbin/system_profiler", "SPDisplaysDataType", "-json").Output()
	if err != nil {
		if cctx.Err() != nil {
			err = cctx.Err()
		}
		return nil, fmt.Errorf("读取 macOS 图形信息: %w", err)
	}
	var output struct {
		Displays []struct {
			Name   string `json:"_name"`
			Model  string `json:"sppci_model"`
			Metal  string `json:"spdisplays_metal"`
			Family string `json:"spdisplays_mtlgpufamilysupport"`
		} `json:"SPDisplaysDataType"`
	}
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, fmt.Errorf("解析 macOS 图形信息: %w", err)
	}
	if len(output.Displays) == 0 {
		return nil, fmt.Errorf("macOS 没有返回显卡信息")
	}
	devices := []string{}
	for _, gpu := range output.Displays {
		if !metalSupported(gpu.Metal) && !metalSupported(gpu.Family) {
			continue
		}
		name := gpu.Model
		if name == "" {
			name = gpu.Name
		}
		if name == "" {
			name = "未命名显卡"
		}
		devices = append(devices, name)
	}
	slices.Sort(devices)
	return slices.Compact(devices), nil
}

func metalSupported(value string) bool {
	// Match known positive symbols; "unsupported" also contains "supported".
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "supported", "spdisplays_supported", "spdisplays_metal1", "spdisplays_metal2", "spdisplays_metal3", "spdisplays_metal4":
		return true
	}
	return false
}
