//go:build !windows

package cli

import "os"

// fileIsTerminal: a character device other than /dev/null (a tty). Without x/term this is the closest portable test;
// components fetch, the only user, only runs on Windows for now.
func fileIsTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}
