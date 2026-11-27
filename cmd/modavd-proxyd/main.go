//go:build linux

// Command modavd-proxyd is the guest half of the per-app proxy (internal/appproxy). build.ps1 builds it for
// linux/amd64 and embeds it in modavd.exe and ModAVD.exe (internal/guest); modavd pushes it to
// /data/local/tmp/modavd-proxy in the instance and runs it as root.
package main

import (
	"os"

	"modavd/internal/appproxy"
)

func main() { os.Exit(appproxy.Main(os.Args[1:])) }
