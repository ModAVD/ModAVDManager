//go:build !windows

package adb

// adbAnswers: elsewhere the listener check stays the system's (macOS also verifies who owns the port).
var adbAnswers = func(int) bool { return false }
