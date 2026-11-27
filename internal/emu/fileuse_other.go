//go:build !darwin || !arm64 || !cgo

package emu

import (
	"errors"
	"io/fs"
	"os"
)

func platformFileInUse(path string) (busy, vanished bool) {
	err := os.Rename(path, path)
	if err == nil {
		return false, false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
		return false, true
	}
	return true, false
}
