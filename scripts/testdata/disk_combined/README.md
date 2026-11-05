# Disk combination regression

Run these fixtures against a local integration tree containing Manager PRs
#56, #59, #60, #63, #64 and #65. They test the contracts shared by those PRs.
The existing PRs remain the delivery points for their implementations.

```powershell
python -B -I -X utf8 scripts/verify_disk_combined.py `
  --source C:\work\manager-integration `
  --sdk C:\test-sdk `
  --out C:\test-results\disk-combined-1 `
  --go C:\go1.26.5\bin\go.exe `
  --mutations
```

Use a new output directory for every run. The runner copies qemu-img and its DLLs
from the existing SDK and creates its own test roots. It never downloads files,
fetches repositories, starts adb or an emulator, or writes to the selected source.
The selected source must contain all six PRs. The runner fails when a required
test is absent or skipped.

The Go files live in `testdata` because main does not yet contain their
dependencies. Go's file overlay adds them to the selected emu test package for
this run. They reuse the existing PR fixtures and assertions. Normal main builds
and tests do not compile these files.

The runner checks:

- Real qcow2 backing data and overlay data through convert, check, compare and
  replacement, including #56's original BackingData test.
- Full DiskCompact calls stopped at convert or compare, with separate Windows
  parent and child processes. Start, delete, bridge and another compact must
  report disk busy within one second. Info must return safe sizes and warnings
  without launching qemu-img.
- Parent termination, cancellation and five-second injected budgets. It checks
  child handles, exclusive file access, original bytes and identity, all locks,
  scratch cleanup, actual successful retry, complete guest content and repeat skip.
- Low-benefit real images with hardlinks or symlinks. The link error must precede
  both lock contention and skip, preserving aliases and existing scratch files.
- Compressed real images for which the policy must skip expansion, and the
  existing controlled-process growth cases and CLI, HTTP/GUI, JSON and MCP result
  contracts.
- Interleaved trim, degraded info, growth and skipped GUI notifications, including
  activity timing, cancellation controls and stale job snapshots.

Normal image operations use the supplied qemu-img. Interruption tests use a real
Windows helper process named qemu-img.exe. It forwards normal stages to the
supplied binary and holds real file handles at the selected interruption stage.
This makes the lifecycle checks deterministic without requiring a very slow disk.
The original disk remains a real qcow2 and is checked with the supplied qemu-img
before retry, then exported and compared byte for byte after retry.

`--mutations` removes the link check, disk lock and kill-on-close flag in separate
Go overlays. Each must fail a specific interaction assertion. A compiler error or
an unrelated cleanup failure does not count as detection. Source files remain
unchanged. Logs, overlays, hashes and test outcomes stay in the output directory.

This does not verify Android discard completion, a physical slow USB disk,
power loss, native Linux/macOS behavior, or a desktop GUI window. Increased
allocation results use the existing controlled-process cases, not a claim that
the real compressed-image fixture grows. That fixture correctly skips.
