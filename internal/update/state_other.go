//go:build !windows

package update

import (
	"os"
	"path/filepath"
)

func isReparsePoint(st os.FileInfo) bool { return st.Mode()&os.ModeSymlink != 0 }
func removeLink(p string) bool           { return os.Remove(p) == nil }

// fileInUse: a loaded library or an open file never stops a rename or an unlink here.
func fileInUse(string) bool { return false }

func resolvePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
