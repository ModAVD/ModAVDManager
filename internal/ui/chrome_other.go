//go:build !darwin || !arm64

package ui

import "modavd/internal/errs"

func launchChromeWindow(string, bool) (*Window, error) {
	return nil, errs.New(errs.Unsupported, "Chrome 管理器窗口只支持 Apple Silicon macOS")
}
