// modavd: command-line manager for ModAVD instances (create, delete, modify, list, start, stop) and an MCP server
// over stdio (`modavd mcp`). It works on a ModAVD root directory: <root>\sdk, <root>\avd, <root>\mas-avd.json.
package main

import (
	"os"

	"modavd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
