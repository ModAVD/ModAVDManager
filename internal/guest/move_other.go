//go:build !windows

package guest

import "os"

// moveNoReplace renames from to to and fails when to exists (a hard link fails on an existing name; rename(2) would
// replace it). modavd is a Windows tool; this keeps the package building elsewhere.
func moveNoReplace(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}
