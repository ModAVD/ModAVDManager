//go:build !darwin || !arm64

package main

import (
	"path/filepath"
	"strings"
)

func managerRootIdentity(path string) string { return strings.ToLower(filepath.Clean(path)) }
