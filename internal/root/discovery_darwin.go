//go:build darwin && arm64

package root

import (
	"path/filepath"

	"modavd/internal/host"
)

func rootCandidates(exe string) []string {
	dir := filepath.Dir(exe)
	candidates := []string{dir, filepath.Dir(dir)}
	if bundleRoot := host.ManagerBundleRoot(exe); bundleRoot != "" {
		candidates = append(candidates, bundleRoot)
	}
	return candidates
}
