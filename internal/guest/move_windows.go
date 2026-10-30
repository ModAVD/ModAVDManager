//go:build windows

package guest

import (
	"os"
	"syscall"
)

// moveNoReplace renames from to to and fails when to exists. os.Rename would replace it (MoveFileEx with
// MOVEFILE_REPLACE_EXISTING), and that is what fails while another process pushes the target; MoveFileW without
// flags never touches an existing file and works on any local file system (a hard link would not on FAT RAM disks).
func moveNoReplace(from, to string) error {
	f, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	t, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if err := syscall.MoveFile(f, t); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
