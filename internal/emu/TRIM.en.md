# Measuring when trim happens

[简体中文](TRIM.md) | **English**

<!-- TRIM.md and TRIM.en.md match section by section: if you change one, change the other in the same PR. -->

This page records the asynchronous contract of a trim request and how to run the measurement again. The results for particular versions are in [a comment on ModAVD/ModAVD#95](https://github.com/ModAVD/ModAVD/issues/95#issuecomment-6654569636).

## What each return value and log line means

| Entry point or signal | Meaning |
| --- | --- |
| `modavd disk <i> trim` succeeding, and `DiskTrimResult` | The manager confirmed that the instance has booted and that `vdc` announces discard support, and then `sm idle-maint run` over adb/su returned successfully. It does not wait for, or confirm, a successful FITRIM in the guest. The CLI keeps the wording "已触发磁盘回收" (disk reclaim triggered) |
| The 2 minutes of `diskTrimTimeout`, the 30 seconds of `autoTrimTimeout` | Timeouts for the request round trip; they do not limit how long the background maintenance runs |
| The seconds in the log of the automatic trim after boot | The host time around the `runIdleMaint` call, that is the adb/su request round trip, not how long the guest took to reclaim. `diskTrimAt` is still the time of the last automatic attempt |
| GUI `disk-trim` `done`, 100% and `finished_at` | The host request job returned. The time also includes the manager's checks and the job overhead. While running the page shows "已进行" (running for), after the request it shows "已触发" (triggered) and "触发请求用时" (request time) |
| MCP `trim_disk` succeeding | The same `DiskTrim` return, with no extra check of completion in the guest. `discard_max_bytes` is a declared capability, not the number of bytes reclaimed in this round |
| vold `Starting trim of /data` | vold starts this round's `/data` trim attempt; it does not prove that the discard was submitted |
| vold `Trimmed … bytes on /data in …ms` | The explicit success return of the `/data` FITRIM. The script requires exactly one start inside the window, the start and the return from the same process and thread, and the return logged after the start |
| vold `Trim failed on /data: …` | This `/data` trim failed. Do not ignore it because `sm` exited with 0 |
| vold `idle maintenance completed` | The maintenance thread ended; this may include other mount points and a failed `/data` trim. It cannot on its own show that `/data` succeeded |
| `/proc/diskstats` discard counters | The kernel's running totals for the device: completed discard requests, merges, 512-byte sectors and milliseconds. A change is supporting evidence; counters that stay still for a while do not prove the round is over |

The call chain is `internal/cli/commands.go`, `internal/ui/disk.go` or `internal/mcp/tools.go`
→ `internal/emu/disk.go:DiskTrim` → `runIdleMaint` → `Manager.su` → the private adb server.
The automatic trigger comes from `internal/emu/autotrim.go`. None of these manager paths reads a completion signal from the guest.
The script watches Android's behaviour through the guest logs of the released package; it needs no AOSP source.

A FITRIM return does not mean either that the host qcow2 file got smaller: the byte count in the log is not the number of bytes freed on the host.
With no matching explicit return, with missing logs or with overlapping rounds, the script outputs `unknown`; it does not infer success from counters or waiting time.

## Running it again

You need Windows, Python 3.10+, an existing released package with its components fetched, and a manager built from the source under test.
No AOSP download or sync is needed. First **physically copy** the released package and its SDK into a new root used only for this,
without copying an existing `avd`, instance configuration, or using a shared data disk. Write a separate configuration for the new root, for example:

```json
{"portBase":27400,"adbServerPort":27498,"scriptAdbServerPort":27499,"instances":{}}
```

First make sure that the whole port range is free and that no other test is using this environment. If only one environment is available, wait your turn.
The ports below are examples; check them again when you run it. The script requires all three port settings to be JSON integers and the two adb server ports to differ,
and none of them may take an instance port, the production range or the adb scan ports; the whole range of 100 ports must also stay clear of production and of the adb scan range.
Before sending any adb command the script checks TCP/UDP use of the whole range and of both server ports,
and allows only the instance's own qemu and this root's adb to listen on their assigned TCP ports; if it cannot verify a process's path, it refuses to go on.
This is only a snapshot at the start and does not replace having the test environment to yourself.

```powershell
$root = 'C:\Temp\modavd-trim-test'
$manager = (Resolve-Path '.\out\tools\modavd.exe').Path
& $manager --root $root create --index 0 --memory 4096 --cpu 4 --data-gb 16 --no-bridge
```

Creating the instance ends with a normal shutdown. To rule out the automatic request after boot, set `instances.0.diskTrimAt` to the current Unix seconds
in the `mas-avd.json` of **this dedicated root**, then start it, without changing other instances:

```powershell
$metaPath = Join-Path $root 'mas-avd.json'
$meta = Get-Content -LiteralPath $metaPath -Raw | ConvertFrom-Json -AsHashtable
$meta.instances['0'].diskTrimAt = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$meta | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $metaPath -Encoding utf8NoBOM
& $manager --root $root start 0 --headless --no-bridge

python -B -X utf8 scripts/measure_trim.py --root $root --manager $manager --command cli --write-mib 128 --exclusive-test-instance --output cli-work.json
python -B -X utf8 scripts/measure_trim.py --root $root --manager $manager --command cli --exclusive-test-instance --output cli-idle.json
python -B -X utf8 scripts/measure_trim.py --root $root --manager $manager --command sm --write-mib 128 --exclusive-test-instance --output sm-work.json
python -B -X utf8 scripts/measure_trim.py --root $root --manager $manager --command sm --exclusive-test-instance --output sm-idle-1.json
python -B -X utf8 scripts/measure_trim.py --root $root --manager $manager --command sm --exclusive-test-instance --output sm-idle-2.json
```

Use a new file name for each output. `--write-mib` defaults to 0, at most 256 MiB; when it is not zero, the script creates only the directory
`/data/local/tmp/modavd-trim-measure-<random>`, writes a real file from `/dev/urandom`,
runs fsync and sync, records `du`, then deletes the file and syncs again. At the end it cleans up only this file and the empty directory.
The script installs no apps, compacts no disks, swaps no packages and does not shut down.

`--exclusive-test-instance` states explicitly that this test has the instance to itself. The lock file in the root only keeps two measurement scripts from overlapping;
it cannot stop other managers or adb users. While sampling, do not let other sessions trim, compact, install on, swap the package of or shut down this instance.
Check the result after each round; if the explicit end signal is missing, first find out whether maintenance is still running before you trigger the next round.

The script reads major:minor of the `/data` root mount from mountinfo, follows the sysfs `slaves` recursively,
saves each layer's device name and discard limit, and filters diskstats by device number; it does not assume that `/data` sits directly on `vdc`.
logcat is read through `su` for `main,system` only, which avoids the kernel read permission errors of a non-root logcat; dmesg is collected through su separately.
Each round saves the boot ID, the SHA-256 of the kernel and the binaries, the vold logs with monotonic time, dmesg snapshots before and after,
and about 10 seconds of diskstats samples by default. The sampling loop sleeps 0.1 seconds, and the real interval also includes the cost of reading,
so the recorded guest uptime is what counts. Sampling has a fixed upper limit, so a lost host connection does not leave an endless loop behind.

`command.sent_at` / `returned_at` use host UTC, and `round_trip_seconds` uses the host monotonic clock.
CLI mode measures the CLI's checks and request as a whole and does not pretend to know the exact moment its internal `sm` was sent.
In `sm` mode, stdout also saves the guest's `SM_SENT`, `SM_RETURNED` and uptime around the actual `sm` call.
vold and diskstats use the same guest uptime as their time base; host wall-clock time is never subtracted from guest time directly.

The root field records only the directory name, not the absolute path. Command output and the full dmesg can still contain paths, user names or other local information:
check and redact the raw JSON before you publish it. When the test is over and you have confirmed that sampling has exited, shut down the dedicated instance and the private adb server normally.

## Automated checks

```powershell
python -B -I -X utf8 scripts/test_measure_trim.py
go test -count=1 -run 'TestDiskJobs|TestDiskTrim|TestToolDefinitions|TestToolCalls' ./internal/ui ./internal/cli ./internal/mcp
```

The parsing tests cover an explicit return, a zero-byte return, maintenance still ending after a real failure, missing logs, old logs, other mount points,
device numbers, a missing discard field, a process or thread mismatch, overlapping rounds, logs outside the window and counters that stay still.
The GUI HTTP test checks that the completion message only says "已触发" (triggered), the MCP test checks the tool's public description, and the existing CLI tests keep the exact original wording.
