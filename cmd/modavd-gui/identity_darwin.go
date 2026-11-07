//go:build darwin && arm64

package main

import "modavd/internal/host"

func managerRootIdentity(path string) string { return host.ManagerRootIdentity(path) }
