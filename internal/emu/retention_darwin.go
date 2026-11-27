//go:build darwin && arm64 && cgo

package emu

import (
	"errors"
	"io/fs"
	"os"

	"modavd/internal/host"
	"modavd/internal/logx"
)

func snapshotLog(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func unlinkRetained(candidate logFile, log *logx.Logger) unlinkResult {
	if err := host.UnlinkRetained(candidate.path, candidate.info, candidate.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return unlinkGone
		}
		log.Debugf("旧日志 %s 不能安全删除: %v", candidate.name, err)
		return unlinkFailed
	}
	return unlinkRemoved
}

// Diagnostics mutation remains gated; this stage only enables ordinary retention.
func holdDiagnosticDirectory(string, fs.FileInfo) (*os.File, error) { return nil, os.ErrPermission }

func platformFileInUse(path string) (bool, bool) { return host.FileInUse(path) }
