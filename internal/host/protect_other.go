//go:build !windows && (!darwin || !cgo)

package host

// Protect requires Windows DPAPI or the native macOS Keychain backend.
func Protect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }

// Unprotect requires Windows DPAPI or the native macOS Keychain backend.
func Unprotect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }
