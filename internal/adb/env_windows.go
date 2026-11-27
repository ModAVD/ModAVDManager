//go:build windows

package adb

import (
	"os"
	"strings"
)

func normalizeEnvKey(key string) string { return strings.ToUpper(key) }

func consoleHome() string {
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return home
}
