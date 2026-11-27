//go:build darwin && arm64 && cgo

package host

import (
	"context"
	"os"

	"modavd/internal/macos"
)

func AllocatedFileBytes(path string) (uint64, error) { return macos.AllocatedFileBytes(path) }
func VolumeFileSystem(path string) (string, error)   { return macos.VolumeFileSystem(path) }
func ReplaceFileDurable(src, dst string) error       { return macos.ReplaceFileDurable(src, dst) }
func CheckCompactFile(path string, expected os.FileInfo) error {
	return macos.CheckCompactFile(path, expected)
}
func RunBound(ctx context.Context, exe string, args ...string) ([]byte, []byte, *os.ProcessState, error) {
	return macos.RunBound(ctx, exe, args...)
}
