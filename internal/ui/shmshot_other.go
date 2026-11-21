//go:build !windows

package ui

import "errors"

// shmFrame: the screenshot library is Windows only; elsewhere thumbnails stay on adb.
func shmFrame(rootDir, serial string) ([]byte, error) {
	return nil, errors.New("shared-memory screenshots are not available on this platform")
}
