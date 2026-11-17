//go:build !windows

package emu

import (
	"os"
	"path/filepath"
)

// hostPicturesDir is ~/Pictures (macOS keeps the user's pictures there; Linux desktops too unless XDG says
// otherwise, which this does not read). An existing folder only, never created here.
func hostPicturesDir() (string, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err.Error()
	}
	dir := filepath.Join(home, "Pictures")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", dir + " 不存在"
	}
	return dir, ""
}
