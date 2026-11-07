//go:build !windows && (!darwin || !arm64 || !cgo)

package emu

import "os"

// DiskCompact returns requireWindows before reaching this platform-specific check.
func checkCompactOverlayLinks(string, os.FileInfo) error { return requireWindows() }
