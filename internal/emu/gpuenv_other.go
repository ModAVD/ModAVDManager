//go:build !darwin

package emu

import (
	"path/filepath"
	"time"
)

const hideRebar = "1"

func emulatorGPUEnv(shaderCache string) []string {
	return []string{
		"__GL_SHADER_DISK_CACHE=1",
		"__GL_SHADER_DISK_CACHE_PATH=" + shaderCache,
		"__GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1",
		"ANDROID_EMU_VK_HIDE_REBAR=" + hideRebar,
	}
}

func emulatorLogPath(dir, name string) (string, error) {
	return filepath.Join(dir, name+"-"+time.Now().Format("20060102-150405")+".log"), nil
}
