# Compact budgets and results

`DiskCompact` validates the host filesystem, instance state, snapshots, backing
metadata, qcow2 header and `qemu-img check` before considering a skip. The check
opens the backing chain. Corruption, a missing backing, cancellation or a failed
check cannot become a successful skip. A leftover scratch image is still removed.

The current live allocation is `allocated-clusters * cluster size`. Host usage
comes from Windows compressed file size or macOS allocated filesystem blocks, so
sparse holes do not count as allocated space. Virtual capacity comes from the qcow2 header. These measure
different things. A tiny overlay can inherit data across a large virtual disk.

Windows compact supports NTFS/ReFS. Apple Silicon macOS uses the SDK's native
`qemu-img` and requires APFS. It rejects symlinked or hardlinked overlays, checks
the original file identity again before replacement, flushes the verified copy
with `F_FULLFSYNC`, and atomically renames it through a held parent directory
descriptor. APFS clones may share extents, so reported file allocation savings
are not a guarantee of the same volume-wide free-space increase. Tool cancellation
also terminates its process group so child tools cannot continue a canceled job.

The estimated saving is `max(0, host usage - live allocation)`, matching disk info.
It omits new qcow2 metadata and does not scan live clusters for zeroes. This is a
cheap estimate, not a promise of the exact saving. Compact skips rewriting when
the estimate is **less than** `max(16 MiB, floor(host usage / 100))`. The absolute
threshold avoids rewriting for a few metadata clusters. The relative threshold
avoids a large rewrite to save less than 1% of its current host usage. Equality
still rewrites. These thresholds are policy choices, not measured I/O costs.

Missing allocation information disables skipping and uses virtual capacity for
the space reservation. Qemu-img 2.12 omits `allocated-clusters` for an empty disk;
its qcow2 report with `total-clusters` identifies that legitimate zero. Counts
that overflow multiplication or the temporary-space reservation are rejected.

Convert and compare retain minimum budgets of 30 and 20 minutes. Beyond those
floors, the budget is five minutes plus the estimated I/O at **1 MiB/s aggregate**.
The working size is the maximum of virtual capacity, live allocation and host
usage for convert. It allows three passes, for reading source and backing and
writing the output. Compare allows two passes over the larger virtual capacity,
including inherited data. This conservative allowance covers large disks and
slow hosts without treating overlay allocation as the entire readable image.
Duration arithmetic saturates instead of wrapping. Unknown virtual capacity
disables the automatic deadline. The caller's context still cancels either
operation, and cancellation and timeout retain their existing error codes.
This does not measure ongoing throughput or guarantee success below 1 MiB/s.
For a 64 GiB disk, the convert budget is about 54.7 hours and compare about
36.5 hours. Large disks therefore effectively do not time out in normal use.
Cancel the operation when you want it to stop. Timeout messages show hours and
minutes for these long budgets, rather than thousands of minutes.

`DiskCompactResult` keeps the existing fields and adds these fields for CLI JSON,
GUI job results and MCP structured output:

| Field | Meaning |
| --- | --- |
| `before_on_disk_bytes` | Host allocation before compact |
| `after_on_disk_bytes` | Host allocation after compact, equal to before on skip |
| `freed_bytes` | Existing nonnegative saving, `max(0, before - after)` |
| `on_disk_delta_bytes` | Signed `after - before`; positive means increased host disk usage |
| `skipped` | Validation completed, but the image was not rewritten |
| `estimated_reclaimable_bytes` | Preflight estimate, omitted when allocation is unknown |
| `reclaim_threshold_bytes` | Minimum estimated saving that qualifies for rewriting |

The verified temporary image is flushed, then measured before the atomic
replacement. A failed flush or measurement therefore leaves the original intact.
CLI and GUI distinguish growth from savings; the GUI job offers cancellation
during a long compact.

Run the repeatable checks with an existing SDK and the pinned local Go toolchain:

```powershell
pwsh -NoProfile -File scripts/verify_compact.ps1 -SdkRoot C:\ModAVD\sdk -TestRoot C:\compact-check -Go C:\Go\bin\go.exe
```

The script copies only qemu-img and its sibling DLLs into its own test SDK. Tests
create independent roots and images, without starting an emulator or adb. The real
fixture preserves inherited backing data and overridden data, forces a useful
rewrite with 32 MiB of unreferenced trailing storage, then verifies that a repeat
skips rewriting. It checks complete guest-visible bytes, backing depth and bytes,
Windows file identity, missing backing and corrupt refcounts. The existing
integration fixture also receives reclaimable storage and asserts a real rewrite,
so Manager #56's backing-data test retains convert/check/compare coverage when
the branches are combined.

The script also runs `verify_compact_process.py`. A Go file overlay scales only
the budget calculation to three seconds, then exercises timeout classification,
scratch cleanup, lock release and original-image protection through the complete
compact pipeline. Independent fault injections must be caught by test failures:
duration overflow, lost cancellation, lost growth reporting and disabled skipping.
Another file overlay grows the temporary image as the flush completes, then
checks that the result reports that allocation. Measuring before the flush makes
this check fail.
