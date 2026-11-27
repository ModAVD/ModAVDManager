//go:build darwin

package emu

import (
	"os"
	"path/filepath"
	"time"
)

const hideRebar = "0"

// MoltenVK exposes Apple unified memory as device-local and host-visible.
// The PCIe ReBAR workaround would hide those guest-visible mapping flags.
func emulatorGPUEnv(string) []string {
	return []string{"ANDROID_EMU_VK_HIDE_REBAR=" + hideRebar}
}

// Reserve a log for this launch. The process backend appends, so a name based
// only on seconds could reuse another launch's HostIME registration message.
func emulatorLogPath(dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, name+"-"+time.Now().Format("20060102-150405")+"-*.log")
	if err != nil {
		return "", err
	}
	path := filepath.Clean(f.Name())
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
