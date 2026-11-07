//go:build darwin && arm64 && cgo

package emu

import (
	"os"

	"modavd/internal/errs"
	"modavd/internal/host"
)

func checkCompactOverlayLinks(path string, info os.FileInfo) error {
	if err := host.CheckCompactFile(path, info); err != nil {
		return errs.Wrap(errs.Unsupported, err, err.Error()).WithAdvice("请先备份数据，再将覆盖层改为独立普通文件后整理")
	}
	return nil
}
