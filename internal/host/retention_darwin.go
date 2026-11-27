//go:build darwin && arm64 && cgo

package host

import (
	"os"

	"modavd/internal/macos"
)

func FileInUse(path string) (bool, bool) { return macos.FileInUse(path) }
func UnlinkRetained(path string, expected os.FileInfo, directory bool) error {
	return macos.UnlinkRetained(path, expected, directory)
}
