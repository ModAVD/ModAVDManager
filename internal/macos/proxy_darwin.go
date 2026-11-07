//go:build darwin && cgo

package macos

import (
	"context"
	"os/exec"
	"strings"
)

// SystemProxy reads the active macOS proxy dictionary without changing network
// services. Scoped per-interface dictionaries do not override the global one.
func SystemProxy(ctx context.Context) (map[string]string, []string, error) {
	b, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return nil, nil, err
	}
	values, exceptions := parseSystemProxy(string(b))
	return values, exceptions, nil
}

func parseSystemProxy(text string) (map[string]string, []string) {
	values := map[string]string{}
	var exceptions []string
	depth := 0
	inExceptions := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "{") {
			if depth == 1 && strings.HasPrefix(line, "ExceptionsList : <array>") {
				inExceptions = true
			}
			depth++
			continue
		}
		if line == "}" {
			if depth == 2 {
				inExceptions = false
			}
			depth--
			continue
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if depth == 1 {
			values[key] = value
		}
		if depth == 2 && inExceptions {
			exceptions = append(exceptions, value)
		}
	}
	return values, exceptions
}
