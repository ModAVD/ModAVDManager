// Package host selects the operating system implementation used by ModAVD.
// Windows calls internal/win, macOS with cgo calls internal/macos, and other
// builds expose the unsupported API. The platform packages do not call each other.
package host
