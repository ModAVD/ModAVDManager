//go:build !windows

package components

import "os"

// moveDir is os.Rename (see move_windows.go for why Windows needs more).
func moveDir(src, dst string, _ bool) error { return os.Rename(src, dst) }
