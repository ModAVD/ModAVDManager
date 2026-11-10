//go:build !windows

package adb

// adbProbe: elsewhere the system table and the platform's ownership check decide (macOS verifies who owns the port).
var adbProbe = func(int) probeResult { return probeSkip }
