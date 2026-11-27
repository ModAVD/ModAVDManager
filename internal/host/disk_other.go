//go:build !windows && (!darwin || !arm64 || !cgo)

package host

func AllocatedFileBytes(string) (uint64, error) { return 0, errUnsupported }
func VolumeFileSystem(string) (string, error)   { return "", errUnsupported }
func ReplaceFileDurable(string, string) error   { return errUnsupported }
