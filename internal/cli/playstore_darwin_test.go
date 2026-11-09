//go:build darwin && arm64 && cgo

package cli

import (
	"strings"
	"testing"

	"modavd/internal/components"
)

func TestNativePlayStoreHelpAndImportNextStep(t *testing.T) {
	details := findCommand("playstore").details
	if !strings.Contains(details, "macOS ARM64") || !strings.Contains(details, "不需要执行桥") ||
		strings.Contains(details, "reason 为 no_bridge") || strings.Contains(details, "再冷启动一次") {
		t.Fatalf("native playstore help: %s", details)
	}
	if help := playStoreFetchHelp(); !strings.Contains(help, "API35 ARM64 r9") || strings.Contains(help, "尚未开放") {
		t.Fatalf("native fetch help: %s", help)
	}
	if help := findCommand("components").details; !strings.Contains(help, "playstore <实例编号> install") || strings.Contains(help, "尚未开放") {
		t.Fatalf("native components help: %s", help)
	}
	var out strings.Builder
	formatImportPlayStore(&out, &components.PlayStoreImportResult{From: "files", Status: components.PlayStoreStatus{
		Dir: "sdk/playstore", Ready: true, InstallSupported: true,
	}})
	if text := out.String(); !strings.Contains(text, "modavd playstore <实例编号> install") || strings.Contains(text, "尚未支持") {
		t.Fatalf("native import next step: %s", text)
	}
}
