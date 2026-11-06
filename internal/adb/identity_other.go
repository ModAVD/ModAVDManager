//go:build !windows

package adb

import (
	"os"
	"path/filepath"

	"modavd/internal/host"
	"modavd/internal/root"
)

// QemuBelongsTo is unavailable until the non-Windows process and file-lock
// implementation is provided; callers conservatively reject the process.
func QemuBelongsTo(*root.Root, host.Process, int) bool { return false }

func EmulatorDiscoveryDir() string {
	if d := os.Getenv("ANDROID_EMULATOR_HOME"); d != "" {
		return filepath.Join(d, "avd", "running")
	}
	return filepath.Join(os.TempDir(), "avd", "running")
}
