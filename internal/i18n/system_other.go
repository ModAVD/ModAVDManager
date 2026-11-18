//go:build !windows

package i18n

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// systemLocale reads LC_ALL / LC_MESSAGES / LANG; on macOS, where apps started from Finder have none of them, the
// first of AppleLanguages.
func systemLocale() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" && v != "C" && v != "POSIX" {
			if i := strings.IndexAny(v, ".@"); i >= 0 {
				v = v[:i]
			}
			return v
		}
	}
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "-g", "AppleLanguages").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.Trim(strings.TrimSpace(line), `(),"`)
				if line != "" {
					return line
				}
			}
		}
	}
	return ""
}
