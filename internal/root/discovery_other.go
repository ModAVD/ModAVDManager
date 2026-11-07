//go:build !darwin || !arm64

package root

import "path/filepath"

func rootCandidates(exe string) []string {
	dir := filepath.Dir(exe)
	return []string{dir, filepath.Dir(dir)}
}
