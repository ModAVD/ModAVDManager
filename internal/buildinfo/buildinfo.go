// Package buildinfo holds the version of this modavd binary.
package buildinfo

// Version of modavd itself, kept in the source rather than set with -ldflags -X: release builds must come out byte
// for byte the same for the same source (Mirror 酱 ships per-file deltas, and a version stamped per package would
// change the exe in every release). The ModAVD package version comes from the root at run time.
var Version = "0.2.0"

// Protocol is the version of the machine-readable contract: the JSON written by `--json`, the exit codes and the
// MCP tool schemas. Callers such as AUTO-MAS check it (`modavd version --json`) before relying on a field.
// Add fields freely; bump it only when a field changes meaning or goes away.
const Protocol = 1
