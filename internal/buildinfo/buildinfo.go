// Package buildinfo holds the version of this modavd binary.
package buildinfo

// Version of modavd itself, kept in the source rather than set with -ldflags -X: release builds must come out byte
// for byte the same for the same source (Mirror 酱 ships per-file deltas, and a version stamped per package would
// change the exe in every release). The ModAVD package version comes from the root at run time.
var Version = "0.6.0"

// Protocol is the version of the machine-readable contract: the commands, the JSON written by `--json`, the exit codes
// and the MCP tool schemas. Callers such as AUTO-MAS check it (`modavd version --json`) before relying on a field.
// Add fields freely; bump it only when a command, a field or a meaning changes or goes away.
//
//	1: up to 0.2.0.
//	2: 0.3.0. restart is gone (a usage error that says to stop, then start); start takes --headless, and a start that
//	   names the other way than the instance runs (window / headless) fails with busy instead of adopting it; the
//	   MCP start_instance's window has no default (omitted adopts either way); new fields window and
//	   creation_progress.
const Protocol = 2
