package host

import (
	"os"
	"path/filepath"
	"runtime"
)

// UserSettingsDir is shared by update and component source settings. Windows
// retains its LOCALAPPDATA convention; macOS uses Application Support in the
// current user's home directory, independently of any ModAVD installation.
func UserSettingsDir() string {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
	} else {
		base, _ = os.UserConfigDir()
		if base == "" {
			return ""
		}
	}
	return filepath.Join(base, "ModAVD")
}
