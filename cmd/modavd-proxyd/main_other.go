//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "modavd-proxyd 只在安卓实例里跑（linux/amd64），由 build.ps1 编好嵌进 modavd.exe")
	os.Exit(2)
}
