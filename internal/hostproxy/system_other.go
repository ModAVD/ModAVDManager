//go:build !windows

package hostproxy

// readSystem: only Windows has the Internet settings; elsewhere the environment decides.
func readSystem() settings { return settings{} }
