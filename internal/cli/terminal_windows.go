//go:build windows

package cli

import (
	"os"
	"syscall"
)

// fileIsTerminal: GetConsoleMode only succeeds on a console input handle. NUL is a character device too (Stat says
// ModeCharDevice), so the mode bits alone would take `< NUL` for a console.
func fileIsTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
