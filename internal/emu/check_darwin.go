//go:build darwin

package emu

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"modavd/internal/host"
	"modavd/internal/root"
)

// platformCheckItems validates image metadata as well as the platform row.
// A directory named arm64-v8a must not silently make an x86 image compatible.
func (m *Manager) platformCheckItems() []CheckItem {
	it := CheckItem{ID: "guest-architecture", Title: "客体架构", OK: checkOK(false), Blocking: true,
		Advice: "请使用 macos-arm64 版 ModAVD 和对应的 Apple Silicon 系统镜像"}
	p := root.CurrentPlatform()
	if !p.Supported() || p.GuestABI != "arm64-v8a" || p.GuestArch != "arm64" {
		it.Reason = "当前 macOS 管理器仅支持 Apple Silicon 和 arm64-v8a 客体"
		return []CheckItem{it}
	}
	it.Advice = fmt.Sprintf("请补齐%s；不要使用其他 Android 版本或 x86_64 镜像", p.SystemImageName)
	props := filepath.Join(m.Root.RuntimeSDK(), p.SystemImageDir, "source.properties")
	data, err := root.ReadFileShared(props)
	if err != nil {
		it.OK = nil
		it.Reason = fmt.Sprintf("查不了系统镜像架构（%s）: %v", props, err)
		return []CheckItem{it}
	}
	_, values := root.ReadKeyValues(data, false)
	abi := values["SystemImage.Abi"]
	if abi != p.GuestABI {
		if abi == "" {
			abi = "未声明"
		}
		it.Reason = fmt.Sprintf("系统镜像架构为 %s，Apple Silicon 需要 %s", abi, p.GuestABI)
		return []CheckItem{it}
	}
	level := values["AndroidVersion.ApiLevel"]
	if level != strings.TrimPrefix(p.TargetAPI, "android-") {
		if level == "" {
			level = "未声明"
		}
		it.Reason = fmt.Sprintf("系统镜像 API 为 %s，当前平台需要 %s", level, p.TargetAPI)
		return []CheckItem{it}
	}
	it.OK, it.Reason, it.Advice = checkOK(true), "Apple Silicon / arm64-v8a / "+p.TargetAPI, ""
	return []CheckItem{it}
}

func (m *Manager) graphicsItem(ctx context.Context, _ bool) CheckItem {
	// system_profiler reports host Metal support, not the emulator's Vulkan
	// loader or bundled graphics stack. Keep this diagnostic non-blocking.
	devices, err := host.MetalDevices(ctx)
	it := CheckItem{ID: "vulkan", Title: "图形加速"}
	if err != nil {
		it.Reason = "查不了宿主 Metal 图形支持: " + err.Error()
		return it
	}
	it.OK = checkOK(len(devices) > 0)
	if len(devices) == 0 {
		it.Reason = "macOS 没有报告支持 Metal 的显卡"
		it.Advice = "请在 Apple Silicon Mac 本机运行并检查 macOS 更新；模拟器图形能力仍需实机验证"
		return it
	}
	it.Reason = "宿主 Metal 显卡：" + strings.Join(devices, "、") + "；模拟器 Vulkan 和渲染能力仍需实机验证"
	return it
}
