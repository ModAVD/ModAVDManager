//go:build !windows

package adb

import "os"

func normalizeEnvKey(key string) string { return key }

func consoleHome() string {
	home, _ := os.UserHomeDir()
	return home
}
