//go:build !windows

package main

// writeText is Windows only (ModAVD/ModAVD#258): elsewhere the manager shows its boxes as before.
func writeText(string, bool) bool { return false }
