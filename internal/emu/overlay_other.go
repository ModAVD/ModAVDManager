//go:build !windows

package emu

import "io"

// RunBootOverlay: the boot screen is Windows only (overlay.go); nothing starts it elsewhere.
func RunBootOverlay(args []string, w io.Writer) int { return 4 }
