# ModAVDManager

[简体中文](README.md) | **English**

<!-- README.md and README.en.md match section by section: if you change one, change the other in the same PR. -->

The management program for [ModAVD](https://github.com/ModAVD/ModAVD): the graphical manager `ModAVD.exe`, and the CLI and MCP server `modavd.exe`. Written in Go, standard library only. It currently supports Windows x64 only; the platform-specific parts are already split into a platform layer. For the plans for other platforms, see [ModAVD#27](https://github.com/ModAVD/ModAVD/issues/27).

> **Version note (`main` and Releases)**: This document describes the implementation on the repository's `main`. `main` is currently at version `0.3.0`, the latest standalone Release is `v0.1.0` (matching ModAVD `0.36.0`), and `v0.2.0` has been drafted but not published. The background initialization on create, the `update` command and exit code 9, all new in `0.2.0`, are not in the `v0.1.0` download package. If you use a published package, go by the notes of the matching Release and the version inside the package.

Usage instructions and guidance for contributing are on the ModAVD documentation site: [The manager and the CLI](https://modavd.github.io/en/components/manager), [Development: the manager and the CLI](https://modavd.github.io/en/develop/manager).

- [What this is](#what-this-is)
- [How it relates to ModAVD](#how-it-relates-to-modavd)
- [Command line](#command-line)
- [MCP](#mcp)
- [The manager ModAVD.exe](#the-manager-modavdexe)
- [Building](#building)
- [Testing](#testing)
- [Contributing](#contributing)
- [License](#license)

## What this is

- ModAVD is an Android emulator based on the AOSP Android Emulator plus a set of patches. It supports Windows today, and more platforms are planned. This repository holds the programs that manage it: create, change and delete instances; start and shut down; the computer check before starting; install APKs and the arm64 execution bridge; view the screen and the logs.
- It manages one ModAVD root folder, that is, the folder the ModAVD release package unpacks to (it contains `mas-avd.json`, `sdk\` and `avd\`). The instance files and `mas-avd.json` are written the same way as in [AUTO-MAS](https://github.com/AUTO-MAS-Project/AUTO-MAS), and the tests compare against AUTO-MAS code byte for byte.
- The manager `ModAVD.exe` is for people; the CLI `modavd.exe` and its MCP server are for scripts, AUTO-MAS and AI assistants.

## How it relates to ModAVD

- This public repository is the basis for developing and releasing the manager and the CLI. Its version number is maintained independently, and it has its own GitHub Releases.
- The ModAVD package still ships `ModAVD.exe` and `tools\modavd.exe`; each ModAVD release pins the manager version and hash it uses.
- The standalone download works from any folder: pass `--root` or set `MODAVD_ROOT` to point it at the ModAVD root folder, and check the download against the hashes below. Do not unpack it into the ModAVD root folder over the files from the package.

| Artifact | Source | What it does |
| --- | --- | --- |
| `<root>\tools\modavd.exe` | `main.go`, `internal/cli` | The CLI; `modavd mcp` is the MCP server |
| `<root>\ModAVD.exe` | `cmd/modavd-gui`, `internal/ui` | The graphical manager (local server + Edge app window) |

Both programs share the implementation under `internal/`:

| Package | Contents | AUTO-MAS code compared against |
| --- | --- | --- |
| `root` | Root folder layout, `mas-avd.json`, port rules, component checks | `constants.py`, `components.py` |
| `avd` | Adding, changing and removing instance files (`mas_<i>.ini`, `config.ini`) | `instances.py` |
| `jsonord` | Order-preserving JSON reading and writing, in the same format as Python's `json.dumps(indent=2, ensure_ascii=False)` | — |
| `emu` | Start, post-boot tuning, first-boot initialization, execution bridge, shutdown, status, computer check | `manager.py`, `precheck.py` |
| `adb` | Private adb server, re-registration, emulator console | `host.py` |
| `win` | Processes, command lines, ports, launching detached from the process tree, file locks | `host.py`, `windows/process.py` |
| `vkfix`, `apk`, `guest` | Guest Vulkan driver patch, APK manifest, scripts pushed into the guest | `vulkanfix.py`, `res/avd/guest/` |
| `cli`, `mcp`, `ui` | The CLI, MCP, and the manager's HTTP server and pages | — |

`internal/guest/scripts/*.sh` and the scripts under AUTO-MAS `res/avd/guest/` are the same files; change both together.

## Command line

```
modavd [--root <dir>] [--json] [--quiet] [--verbose] <command> [args]
```

| Command | Description |
| --- | --- |
| `list` | All instances and their status |
| `info <i>` / `status <i>` | Details of one instance / its status |
| `create [--index N] [--title T] [--memory 6G] [--cpu N] [--data-gb N] [--fps 30\|60] [--resolution 1080\|720] [--no-bridge] [--timeout 1200]`, `create --resume N` | Create an instance, then start it once in the background, headless, to finish initialization, and shut it down (see below). The arm64 execution bridge is included by default; `--no-bridge` records that this instance does not want the bridge, so it is not installed on later starts either. New instances never get the Google Play Store; add it afterwards with `playstore <i> install`. `--resume N` redoes the initialization of an instance whose creation failed |
| `set <i> [--title T] [--memory M] [--cpu N] [--fps F] [--resolution 1080\|720] [--data-gb N] [--diagnostics on\|off]` | Change settings. Diagnostics apply immediately; other settings take effect at the next start. `--data-gb` can only be increased, and game data is untouched: the emulator (from 0.36) grows the disk image at start and grows the file system online after boot. A disk created before 0.36 at 16 GB or smaller can only be grown to just below the next backup group (a 16 GB disk to about 30 GB) |
| `proxy <i> [on\|off\|test] [--server URL\|auto] [--add PKG] [--remove PKG] [--apps PKG]` | Per-app proxy: the listed apps go through a proxy, the others direct (see below) |
| `delete <i> --yes` | Delete a stopped instance, together with its game data |
| `start <i> [--window \| --headless] [--timeout 300] [--no-bridge]` | Start (headless by default, with the window for `--window`), wait until boot finishes and the post-boot setup is done, and before returning confirm that files can be written to `/sdcard` (normally it is ready long before this; it waits up to 60 more seconds and only warns at the deadline). If the instance is shutting down (for example, right after a click on the emulator window's ×), it first waits for the shutdown to finish (up to 30 seconds) and then starts it. If the instance has no execution bridge yet, install it afterward; `--no-bridge` skips that this time. No switch while it runs: if the instance is already running and `--window` / `--headless` asks for the other way, it fails (exit code 5) without taking it over or restarting it; to switch, `stop` and then `start`. With neither, it is taken over as it runs |
| `stop <i> [--force] [--cancel-create]`, `stop --all [--force]` | Shut down: an app in the foreground is first sent home and given 2 s to save; then sync and ask Android for a clean shutdown (the foreground app is paused and stopped first), and if that fails, sync + kill; `--force` is sync + force-kill. Creating instances are refused unless `--cancel-create` is given. `--all` also stops creating instances and marks their results with `interrupted_create: true` |
| `bridge <i> install\|uninstall\|verify` | Install, uninstall or check the arm64 execution bridge; it is installed automatically at start, so you normally do not run it by hand (see below) |
| `playstore <i> install\|uninstall\|status` | Add the Google Play Store to an instance, remove it, or check it (see below) |
| `disk <i> compact` | Compact a stopped instance's data disk, or skip rewriting when estimated savings are too small. Verify before atomically replacing the original, and report host usage before and after, including growth. Large disks have budgets of many hours and effectively do not time out in normal use; cancel to stop an operation. Requires NTFS/ReFS, no snapshots and room for the temporary image. See [budgets and result fields](internal/emu/COMPACT.md) |
| `check [--memory M]` | Computer check before starting |
| `report-config [--no-clipboard]` | Copy the computer configuration and the game info from the running instances (read-only) |
| `collect-info [--out-dir D] [--stage-dir D] [--keep-stage] [--no-explorer]` | Collect a problem report and zip it (read-only). Ctrl+C before ZIP completion returns `canceled`, deletes the unfinished ZIP and removes staging; `--keep-stage` preserves staging and includes its path in the error |
| `components [status]`, `components source [auto\|googledownloads_cn\|tencent\|google]`, `components fetch [--accept-license] [--mirror ID] [--mirror-url URL] [--keep-downloads] [--play-store]`, `components import-playstore <file or folder>...` | View, choose, or fetch the Google components (adb and the system image; see "Fetching the Google components"); `--play-store` and `import-playstore` add the optional Google Play Store (see below) |
| `window <i> show\|hide`, `install <i> <apk>`, `shell <i> <command>`, `adb <i> -- <args>` | Window, install an APK, guest command, adb passthrough |
| `update [status\|check\|prepare\|install\|recover] [--automas-version V]` | Update the whole ModAVD package in the root folder, using the update source from "Settings" (`设置`) (see "Updating ModAVD") |
| `servers`, `version`, `mcp`, `help [command]` | |

Apple Silicon macOS supports `report-config` and `collect-info`, including hardware, macOS, HVF, Metal and component versions. Game lists are queried only through an existing private ADB server and running instances with verified ownership; no service is started. The package contains an allowlist of local instance settings and at most eight emulator logs, capped at 2 MiB each and 20 MiB of text in total. Common credential fields, paths and device identifiers are filtered. macOS does not collect screenshots, system dumps or other applications' logs. Review the package before sharing it. Clipboard copying and Finder reveal can be disabled with `--no-clipboard` and `--no-explorer`.

- Write `<i>` as `3` or `mas_3` (0-9, except 5). Memory is written as `6144`, `6144M` or `6G`, with only four tiers: 3 / 4 / 5 / 6 GB. Cores are 2 / 4 / 6, and the frame rate is 30 / 60.
- Root folder: `--root`; if not given, the environment variable `MODAVD_ROOT`; otherwise the folder containing `modavd.exe`, or its parent.
- From 0.3.0 there is no `restart`: `stop`, then `start`. An old call gets a usage error (exit code 2) that says so.

**The arm64 execution bridge**:

- It applies only to the games on the built-in list; other apps keep using the original translation layer. The bridge is installed in the instance's own system / vendor overlay and needs an emulator from 0.36 on.
- Every time modavd (the CLI, MCP) and the manager start an instance, they check: if the instance has no bridge yet, they install it after the post-boot setup is done, with one or two shutdowns and cold boots in between; if it already has the bridge but the bridge's files differ from the ones in this package (the ModAVD package was updated), they replace them the same way after the setup, followed by one cold boot. This happens only when they started the emulator this time; when taking over an instance that is already running, nothing is touched. Because the update takes a cold boot, it is put off to a later start when, once the post-boot setup is done, an app is already in the foreground or an APK is being installed. If installing or updating fails it is only a warning, the instance keeps running, and it is tried again at the next start. Instances that AUTO-MAS starts by itself do not go through this step: to get the new files, shut the instance down, then start it once with ModAVD.exe or `modavd start`, or run `bridge <i> install`.
- Before moving a root, cold-start each existing bridge instance once at the original location with the updated manager or `modavd start`, then shut it down. Move the whole root, including each instance’s `modavd-bridge-backings.json`. If the old path is missing at the next boot, the manager verifies the size and SHA-256 of the new SDK’s system and vendor images before reconnecting them. If you already moved an older instance without this record, keep its overlays, restore the root and original images at their old location, then repeat the move.
- To keep an instance without the bridge, pass `--no-bridge` on create, or run `bridge <i> uninstall` later. Both record `instances.<i>.bridge` as `false` in `mas-avd.json`, so later starts no longer install it automatically; `bridge <i> install` again brings it back. The `--no-bridge` of `start` applies to that one run only.
- `bridge` calls `tools\modavd-bridge.ps1` in the ModAVD package. `install`: shuts down and starts again if needed, and installs; if it is already installed, it updates only when the payload has changed. `uninstall`: removes the bridge, the boot marker, and the GL transport mode (asg) that was set when the bridge was installed; uninstall before going back to an earlier version. If the instance was off before, it is shut down again afterward; if it was running, its cold boots bring it back the way it ran, with its window or headless. `verify`: a read-only check; the instance must be running.
- The `bridge` field of `info` / `list` / `status` is `off` (marked as not wanting the bridge), `pending` (will be installed at the next start that modavd launches), `installed` (the bridge is present in the running guest) or `unknown`. It only queries a guest that has finished booting. For an instance that is off or still booting, the boot marker stays once the bridge is installed and is also there when installation failed, so when first-boot initialization has been done and the marker is present, the value is `unknown`; to know the exact result, check after the instance has started.
- While the bridge is being installed (including the shutdowns and cold boots in it), `busy` is `bridging`; during this time `delete` and another `bridge` return instance busy (exit code 5). Callers should treat any `busy` value they do not recognize as busy.
- An instance that was not started through modavd or the manager (another program launches the emulator directly) does not get the bridge automatically; to install it, run `bridge <i> install` once.

**Google Play Store** (`components fetch --play-store`, `components import-playstore`, `playstore`, ModAVD#125):

On Apple Silicon macOS, downloads and imports use Android 15 Google Play `arm64-v8a` r9: the official `arm64-v8a-35_r09.zip` archive (about 1.8 GB). After license consent, download it or import your own archive/five files; their pinned sizes and hashes are checked before publication to `sdk/playstore/`. These files are separate from the Windows API34 x86_64 profile described below. macOS currently only prepares the component; installation into an instance and removal remain unavailable, and no execution bridge is required to prepare the files.

- The instances' Google APIs image has Google Play services but no store. The store is five files of Google's official `system-images;android-34;google_apis_playstore;x86_64` r14, the same release as the instances' image: the store, the Google-signed Play services, GSF and Partner Setup, and the store's fs-verity certificate. The root folder keeps only these five files (`sdk\playstore\`), and each instance gets them when asked; new instances never do.
- The root gets the five files one of two ways, both optional (`components status` and `check` never count them as required):
  - Download: `components fetch --play-store` (in the manager, 「补全 Google Play 商店服务」 in the component dialog) downloads that image (about 1.5 GB) after the license agreement, reads the five files out of its `system.img` read-only, each checked against its built-in size and SHA-256, and deletes the unpacked `system.img` and the downloaded zip (the zip stays with `--keep-downloads`). Skipped when the files are there already.
  - Your own files: `components import-playstore <file or folder>...` (in the manager, 「使用我自己的文件」). Give Google's zip `x86_64-34_r14.zip` (any name; checked against the size, 1,510,752,654 bytes, and SHA-1 of Google's repository, then read like a download) or the five files themselves (any names; each checked against its size and SHA-256, the values in the [documentation](https://modavd.github.io/en/guide/playstore); a folder gives the files in it with those names). A file that is neither, a hash that does not match or a missing file is refused (exit code 9, `components_rejected`; `error.details.reason` with `--json` is `unknown_file`, `mismatch` or `incomplete`) and nothing changes. Your files are only read. Nothing is downloaded and no license is asked.
  - `components status` lists whether the five files are there and where they came from (`play_store.source`: `download` or `import`).
- The store alone is not enough: the image's own Play services and GSF are signed with the test certificate, so their signature does not match the store's and Google's components refuse to work. They are replaced by the copies of the same version from the Play image.
- It goes into the writable system overlay that the execution bridge brings, so the instance needs the bridge: without it `install` fails with `not_ready` (`details.reason` `no_bridge`), and without the root's five files with `not_ready` (`playstore_files_missing`), both before the instance is touched. An instance with the store refuses `bridge <i> uninstall` until `playstore <i> uninstall`. The image's own placeholder with the same package name (LicenseChecker) is hidden, and brought back on uninstall.
- `install` puts the files in and boots cold once. `uninstall` disables the store, removes the updates Google Play installed and the files that were put in, boots cold once, then brings back and enables the placeholder. An instance that was off is shut down again afterwards, also when the action fails (a shutdown that fails then is only a warning); a running one comes back the way it ran. `status` is read-only and needs the instance running.
- When `install` fails half way (the overlay running out of space, say), it takes back the files this run put in (the `.new` copies not yet in place too) and brings back the placeholder; the files that replaced the image's own and the placeholder are fully back after the next cold boot. What cannot be taken back makes `status` read `partial` and the instance is recorded as partly installed: `uninstall` removes it, another `install` completes it.
- The device is not Play Protect certified. If signing in says so, get the instance's GSF ID with `playstore <i> gsfid` (in the manager: 「复制 GSF ID」 in the instance's 「更多操作」 menu), given in decimal and hex (`decimal`, `hex` with `--json`), and register it at <https://www.google.com/android/uncertified/>. The instance must be running; without an ID yet it fails with `not_ready` (`details.reason` `no_gsf_id`): wait a minute or two with the network on and try again. Games that check Play Integrity will most likely fail (the image is a userdebug build with test keys).
- An instance that never had the store is not touched, and `mas-avd.json` gets no extra key. Once it is installed, `instances.<i>.playStore` is `true`, `false` after uninstall, and `"partial"` after an install that could not take back all it put in; `play_store` (installed) and `play_store_partial` (partly installed) in `info` / `list` follow it.

**Per-app proxy** (`proxy`):

- The TCP, UDP and DNS of the listed apps (by package name) go through the configured proxy; every other app goes direct as before. No VPN is involved: a root daemon in the guest (`modavd-proxyd`, carried in by `modavd.exe`) takes their traffic by the apps' uids with policy routing and a transparent proxy, so neither the apps nor the system see a VPN.
- `modavd proxy <i>` shows the switch, the address and the list; while the instance runs, it adds the state in the guest (whether the rules are in place, whether the proxy answers, which listed apps are not installed yet).
- `on` turns it on, `off` turns it off (the address and the list are kept); `--server` changes the address, `--add` / `--remove` edit the list, `--apps` replaces it (each can be given several times or comma-separated; `--apps ""` empties the list); they can be combined with `on`. The list holds at most 64 apps.
- Write the address as `socks5://IP:port` or `http://IP:port`; `127.0.0.1` and `localhost` are stored as `10.0.2.2`, which is how the instance reaches this computer; without a scheme, the port is probed on this computer. `--server auto` (also the first `on` without an address) takes this computer's proxy setting: first `ALL_PROXY`, `HTTPS_PROXY`, `HTTP_PROXY`, then the proxy set manually in the Windows "Internet Options" (its `socks=` entry first), and probes whether it is SOCKS5 or HTTP. Only IPv4 addresses and localhost are taken.
- A proxy program's mixed port (7890, 7897 and the like in Clash-style programs) comes out as SOCKS5 and carries TCP, UDP and DNS. With an HTTP proxy only TCP and DNS go through; the listed apps' UDP does not work.
- **When the proxy is down, the listed apps have no network**; they never quietly fall back to direct. DNS asks `1.1.1.1` over TCP through the proxy, and the listed apps get no IPv6 (they use IPv4).
- A change takes effect at once while the instance runs (`applied` is `now`), otherwise at the next boot (`next_boot`). A boot through modavd, MCP or the manager puts the rules in place as the first step after Android is up; after the guest reboots on its own, the boot watch puts them back. An instance AUTO-MAS boots by itself, without modavd, has none.
- `test` probes the configured address from this computer: which protocol it speaks and whether UDP works through it. A proxy that cannot be reached is exit code 9 (`proxy_unavailable`).
- For overseas games that sign in with Google, also list Google Play services (`com.google.android.gms`).
- The setting is stored in `mas-avd.json` under `instances.<i>.appProxy` (`enabled`, `server`, `apps`) and goes away with the instance. `app_proxy` in `info` / `list` is its summary (read from the configuration only, no adb; `null` when never set). For a running instance that has one set, `collect-info` also takes the guest daemon's state and the last 200 lines of its log.

**Machine-readable output** (`--json`):

- stdout has exactly one line: `{"ok":true,"data":…}` or `{"ok":false,"error":{"code","message","advice","details"}}`. The fields of `data` are in `internal/emu/types.go`, in snake_case, and all fields are always present.
- Progress logs go to stderr, one JSON object per line: `{"time","level","msg"}`. Callers must not merge stderr into stdout.
- `protocol` in `version --json` is the contract version; it is incremented only when a command or a field is removed or changes meaning. From 0.3.0 it is 2: `restart` is gone, `start` takes `--headless`, a running instance asked to start the other way (window / headless) fails with `busy` instead of being taken over, and the fields `window` and `creation_progress` were added; up to 0.2.0 it was 1.
- **Ports** (from 0.3.1): `port_base` in `list` and `check` is the port range the root folder actually uses (see `portBase` under "Conventions" below), and `status` now also gives `serial`, `adb_address` and `ports` (`list` and `info` always had them, for stopped instances too). Scripts that connect to an instance should take these fields instead of computing 20000 + 10 × index. An older modavd without `port_base` only uses the 20000 range.
- **`details` of an error** is optional, and all values are strings. These additions preserve the existing `code`, `message` and protocol. Clients must tolerate missing fields and unknown keys or values. Manager HTTP responses and failed jobs carry the same fields. MCP `isError` text appends one `error.details: {…}` line after the existing error, where `{…}` is JSON.
  - `reason` distinguishes instance-operation `busy` errors: `running` (instance not stopped), `locked` (another operation holds a lock), `starting` (stop found a start that has not launched qemu), `stopping` (start waited for shutdown but qemu stayed), `window_mismatch` (different window mode), `port_in_use` (ports occupied), or `port_other_root` (the same-numbered instance of another root occupies the ports). Other `busy` errors may omit this field.
  - `reason=locked` also has `lock`: `start`, `stop`, `boot`, `bridge`, `create`, `guest`, `disk`, `metadata` or `config`. `disk` is the lock for compact or a shared disk probe; `metadata` locks the root JSON, and `config` locks per-instance configuration and metadata writes.
  - A failed boot wait or after-boot setup in `start` carries `instance_status` and `qemu_running`. The former is a known lifecycle status (such as `offline`, `starting` or `create_failed`), `not_found` or `unknown`; the latter is `"true"`, `"false"` or `"unknown"`. An unfinished boot wait knows `starting`; later setup may restart Android, so its boot state can be `unknown`. The same process snapshot decides whether to collect guest diagnostics, before that collection which can take up to 30 seconds. It does not guarantee the state stays unchanged until a client reads it.
  - A failed `create` also carries `instance_exists` (`"true"` / `"false"` / `"unknown"`), and fills `instance_status` and `qemu_running` after cleanup, overriding stale state from an inner `start`. `instance_exists` only says that instance files or metadata currently occupy the index, including incomplete leftovers and pre-existing instances. It **does not establish ownership by this create**. This definition also applies to an existing index, a concurrent create or invalid arguments; never delete automatically based on this field alone. An unselected index or an unreadable fact gives `unknown`. Failed initialization reads `create_failed`; another creator's lock is left alone, and its status can be `unknown`.
  - `boot_diag_dir` names the directory where guest state was saved after a boot timeout, failed after-boot setup and similar failures. Error wrappers and creation cleanup retain it.
  - When a boot fails while the emulator is still running, `start` and `create` (also those started over MCP or by the manager) save the guest's `logcat -d -b all`, `dmesg`, `getprop`, `ps -A`, `/proc/meminfo` and the first 300 lines of `dumpsys activity processes` into `logs\mas_<i>-<time>-bootfail\` before they return; `summary.txt` in the same directory says what came of each item.
  - Each item has its own time limit, and the whole takes at most 30 seconds; when an item times out or fails, what was read so far is kept.
  - Nothing is saved after a cancel (Ctrl+C, an MCP cancel), when the instance is being shut down during the boot, or when the emulator is gone.
  - Each instance keeps its newest 3 such directories, and their size counts toward the 1 GiB total of `logs\`. `collect-info` puts the newest 3 into the problem package, and stops taking more once they add up to 100 MB.
  - These files may contain account information and stay on this computer. In text mode, the last line of the error names the directory.
- Running instances gain `started_at_ms` (Unix milliseconds); the original `started_at` stays. `window` (from 0.3.0) is `true` while it runs with the emulator window, `false` when headless or not running. `stop --all` returns `ok:true` and the result list even if some instances failed, and exits with the exit code of the first failure.
- **Create (from 0.2.0)**: after creating the files, `create` starts the instance once in the background, headless, and finishes the post-boot setup, the host IME, first-boot initialization and the execution bridge (with one or two cold boots in between). Then it syncs first, shuts down cleanly and confirms that qemu has exited; only then is creation complete. This usually takes a few minutes (`--timeout` covers the whole process, default 1200 seconds; at the deadline it shuts down cleanly first and then records a failure). After that, the first `start` is as fast as usual. Instances created before 0.2.0 are unaffected and still finish this at their first `start`.
  - During this, `status` in `status` / `list` / `info` is `creating` and `busy` is `creating` (`running` / `booted` of that boot may be `true`; go by `status`), and `creation_progress` (from 0.3.0) is the percentage done (0–100, counted by the steps done, not by time); `start`, `set`, `bridge`, `delete` and a `create` with the same index return instance busy (exit code 5). A single-instance `stop` also returns busy, even with `--force`. To cancel, use `stop <i> --cancel-create` or MCP `stop_instance` with `cancel_create: true`. It still syncs and shuts Android down normally; the creator receives cancellation and records `create_failed`, and the stop result has `interrupted_create: true`. `stop --all` allows cancellation automatically and marks each result; the field is `false` for instances that are not being created.
  - A failure (including the create process being killed) is `create_failed`; `creation_error` gives the reason and `creation_log` is the log of that boot. After that, `start`, `set` and `bridge` return `not_ready` (exit code 4). Use `create --resume N` to redo the initialization (an emulator the failed one left running is shut down cleanly first), or `delete N --yes` to delete the instance.
  - The new fields `creation` (`""` / `creating` / `failed`), `creation_error` and `creation_log` are always present. The state is kept in keys such as `instances.<i>.creationState` in `mas-avd.json` and removed when creation is done. Callers (AUTO-MAS) should treat any `status` value they do not recognize as busy.
  - Progress: the CLI prints each step to stderr, each line starting with 「新建中 NN%」 (creating, NN %); MCP's `create_instance` sends progress notifications (when a progressToken is given); the manager shows 「新建中 NN%」 and a progress bar on the card (also for a creation started elsewhere), offers to cancel it under "More" (`更多`), and closing the window lets it finish in the background.

**Exit codes**: 0 success, 1 internal error, 2 bad usage or arguments / license agreement not accepted, 3 instance does not exist, 4 root folder not ready / check failed / not supported, 5 instance busy / already exists / port in use, 6 timeout, 7 start failed, 8 canceled, 9 cannot check or download during an update or component fetch / the download failed verification, or the per-app proxy cannot be reached. Without `--json`, `shell` returns the guest command's exit code and `adb` returns adb's exit code; for `bridge` without `--json`, it is 1 if the bridge is still not installed after `install` or still present after `uninstall`, and `verify` returns the script's exit code.

**Conventions** (the same as AUTO-MAS):

- adb only uses the root folder's private server (`adbServerPort` in `mas-avd.json`, default 20050) and never touches 5037.
- Ports: the console port of instance i is 20000 + 10 × i, adb is 1 more and gRPC 2 more; the index is 0–9 without 5.
  - A root folder for development or testing can move to its own port range with `"portBase": 21000` in `mas-avd.json` (a key of modavd's own; AUTO-MAS does not know it). Its instances then do not fight over ports with the instances of the same index in the root folder in daily use on the same computer (for example the one AUTO-MAS manages), and are not taken for them.
  - `portBase` must be a multiple of 100 between 1100 and 49000, excluding 5500. Other adb servers scan the 5500 range automatically; ranges at 49100 and above overlap Windows' default ephemeral ports. Existing configurations in those ranges still support status and shutdown, but block new starts. Shut down all instances before changing the range. The default remains 20000. With another range, both `adbServerPort` and `scriptAdbServerPort` must be set, and neither can be 20049 or 20050 or fall on an instance port of the 20000 range.
  - For malformed values, `status`, `list` and `info` use the 20000 range. Previously accepted 5500 and 49100–65400 configurations retain their original range for status and shutdown. Both cases refuse starting and creating (`invalid_argument`), and `check` shows a 「端口段」 (port range) row saying why.
  - Shut down every instance of the root folder before you change it. Do not add a root folder that sets `portBase` to AUTO-MAS.
- The emulator, the adb server and logcat are all launched through an intermediate process that exits immediately, so they are not in modavd's process tree: if the caller ends modavd (Ctrl+C, the MCP client disconnects), the emulator is not taken down with it.
- A separate `tools\modavd.exe` process writes logcat to `logs\logcat-mas_<i>-<time>.txt` and continues after the start command or manager exits. When that boot's qemu exits, the capture process ends its own adb client and exits. At 256 MiB it rotates at the next newline and writes the same adb stream to a new file. Closed segments follow the existing limits of 10 per instance and 1 GiB across `logs\`; the active file is excluded.
- After a successful start, each instance also gets a boot guardian that is likewise outside the process tree and exits when qemu exits. After the guest restarts on its own (qemu keeps running), the guardian redoes the post-boot tuning and the Vulkan patch, and never launches a new emulator.
- Starts and shutdowns of the same instance queue on `<root>\avd\.mas_<i>.{start,boot,stop}.lock`: a start waits for the previous start and shutdown to finish; a shutdown can interrupt an instance that is starting.

**Host scheduling in the post-boot tuning**:

- qemu runs at above-normal priority.
- Pinning only happens on processors whose L3 caches come in groups of different sizes (X3D parts such as the 7950X3D, Ryzen AI 300, and so on); other processors are not pinned:
  - The group with the largest free share (free logical processors ÷ the group's logical processors) is chosen, the bigger L3 on a tie; a single instance gets the big-L3 group.
  - When no group has room for the instance's vCPUs, it is not pinned.
  - "Free" means after the vCPUs of every qemu on the machine, including those of other root folders and those started by AUTO-MAS. Those processes are only read, never changed.
- While the guest does many small disk writes (downloads, for example), it runs one busy loop per vCPU (`mas-idlepoll.sh`) so the vCPUs do not halt, which makes downloads faster.
  - The number of loops never exceeds what the host has left: logical processors minus the vCPUs of every qemu (this instance's included; counted within its group when pinned).
  - So on machines with fewer than twice as many logical processors as the instance has vCPUs, even a single instance gets fewer loops: a default 6-core instance gets 2 on an 8-thread machine and none on 6 threads or fewer, and downloads are slower than with all loops (measured on a 7950X3D, 1–2 loops do about as well as none).
  - This is a deliberate trade-off: it leaves processors for qemu's own threads, Windows and other programs.
Two display presets are available: 1080p is 1920×1080 / DPI 280; 720p is 1280×720 / DPI 240. New instances default to 1080p. 720p uses fewer resources. The manager forms, CLI `--resolution 1080|720` and MCP `resolution` share these presets. Existing landscape or portrait 1920×1080 instances stay at 1080p; other configurations, including missing dimensions, stay at 720p. Changing the preset updates width, height and DPI together, preserves orientation, and takes effect at the next cold boot. DPI is not a separate option. No runtime `wm size` or `wm density` is used. Update the resolution in your automation tool as well.

AUTO-MAS currently prefers the packaged CLI, which preserves the selected preset. Old roots without a usable CLI fall back to the Python launch path, which still forces 720p. Update the packaged CLI before using 1080p there. AUTO-MAS creation and display settings still need matching two-preset support; use the ModAVD manager to set the preset meanwhile.

## Diagnostic mode

Enable diagnostic mode in the instance settings, or run `modavd set <i> --diagnostics on`. It defaults to off. Files stay on this computer and are not uploaded automatically. Logs and screenshots can contain account information. `info` and `list` show the switch; JSON includes `diagnostics` and, while enabled, `diagnostics_since`. The switch and first enable time are stored as `instances.<i>.diagnostics` and `diagnosticsSince` in `mas-avd.json`. Enabling again preserves that time.

- Emulator output, logcat and archived dumps are kept for 30 days, together with the latest 20 failed-boot snapshots, under a separate 4 GiB history limit for this instance. They do not consume the ordinary instances' shared 1 GiB pool. Bridge logs are retained during diagnostics. Active files are excluded from deletion and history limits; logcat still rotates at a newline after 256 MiB.
- When the volume has less than 20 GiB free, cleanup falls back to ordinary rules and warns once. Diagnostic retention resumes when space recovers. Unreadable configuration skips deletion and produces a warning.
- Guest main, system and kernel buffers become 32 MiB each; other buffers stay at 16 MiB. This adds at most 48 MiB of guest memory. Host capture includes kernel and dmesg. Running instances switch immediately; stopped instances apply the setting at their next boot.
- If AUTO-MAS or another program already captures logcat for the same server port and serial, no additional capture is started and the external process is left running. Diagnostic settings report that host capture was not adopted. When diagnostic configuration is temporarily unreadable, capture and guest buffers continue in ordinary mode with one warning per failure episode. History deletion still requires valid configuration and deletes nothing on a read failure.
- A running capture keeps this boot's verified SDK and adb target. Damaged configuration cannot redirect it to default ports. If no target can be established initially, the operation warns and skips the affected capture or guest change instead of guessing defaults.
- Before each start, a separate short-lived background task copies new ModAVD crash dumps since the last successful scan into `logs\mas_<i>-crashdumps\`. Startup does not wait for the copy. Dumps in a shared crashpad database cannot be attributed precisely to one instance, so the archive can include another instance's new dumps. Crashpad's own retention stays unchanged.
- For each diagnostic instance, `collect-info` selects the latest 30 emulator logs and logcats, with a separate 1 GiB copy budget shared with archived dumps. Failed-boot snapshots have another 300 MiB limit. Dumps are deduplicated by content; oversized binary files are skipped whole. Text is bounded to the selected snapshot length, with growth or budget truncation reported in summary. Running diagnostic instances also contribute all logcat buffers, dmesg, meminfo and activity processes. Summary records the switch and enable time. Ordinary instances keep the shared 300 MiB log budget. Ordinary failed-boot collection keeps its existing rule: include the newest directory whole, then stop taking more after reaching 100 MiB.

`modavd set <i> --diagnostics off` immediately reduces closed emulator logs, logcats and bridge logs to the latest 10 each, keeps 3 failed-boot snapshots and deletes this instance's dump archive. Remaining files return to the ordinary root-wide 1 GiB pool, which can also evict older logs from other ordinary instances. Guest buffers return to 16 MiB and host capture returns to the ordinary buffers. The command reports actual deleted items and bytes. Repeat it after a failure to finish cleanup or guest restoration. The manager previews cleanup and asks for confirmation before disabling.

## MCP

```
modavd mcp --root D:\ModAVD
```

It supports both MCP 2026-07-28 (stateless, `server/discover`) and 2025-11-25 / 2025-06-18 (`initialize` handshake). The root folder is fixed at startup and is not a tool parameter. Tools: `list_instances`, `get_instance`, `check_host`, `create_instance`, `update_instance`, `delete_instance`, `start_instance` (with progress), `stop_instance`, `set_window_visible`, `shell`, `install_apk`, `disk_info` (data disk size), `trim_disk` (reclaim space while running; the emulator must support discard), `compact_disk` (shrink the data disk; the instance must be stopped), `get_app_proxy`, `set_app_proxy` (per-app proxy).

Only `list_instances` exposes diagnostic fields. MCP tools do not accept a diagnostic switch; disabling deletes logs and must be done in the manager or CLI.

`disk <i> trim`, the GUI trim job, and `trim_disk` return after requesting asynchronous guest maintenance.
Success does not confirm that guest FITRIM finished or succeeded. The automatic trim log measures only the
adb/su command round trip. Stable discard counters alone cannot establish completion.

To register it in Claude Code (in PowerShell, `--` needs quotes, or it gets swallowed):

```powershell
claude mcp add --transport stdio --scope user modavd '--' D:\ModAVD\tools\modavd.exe mcp --root D:\ModAVD
```

Or in the project's `.mcp.json`:

```json
{"mcpServers": {"modavd": {"command": "D:\\ModAVD\\tools\\modavd.exe", "args": ["mcp", "--root", "D:\\ModAVD"]}}}
```

### Tested in Claude Code

On 2026-11-03, Claude Code 2.1.317 (the command-line version) was connected to modavd 0.3.0 with `claude -p --mcp-config <file> --strict-mcp-config`, which loads modavd for that one call only. Five runs in all:

| What was done | Result |
| --- | --- |
| Seven tools called in order in one run: `list_instances`, `get_instance`, `create_instance` (3 GB, 4 cores), `update_instance`, `start_instance`, `stop_instance`, `delete_instance` | The first six succeeded; `delete_instance` was blocked under `-p` (see below) |
| Only `start_instance`, and claude exited normally once the instance had booted (run twice) | The emulator was still running, `list` showed it online, and its parent process chain had neither claude nor modavd in it; a later `modavd stop` shut it down normally |
| `MCP_TOOL_TIMEOUT` set shorter than a boot, so `start_instance` timed out halfway and claude exited | The emulator finished booting, but the setup after boot was not done; running `modavd start <i>` once more completed it |
| The claude process killed 8 seconds after the emulator came up | `modavd mcp` exited by itself within 5 seconds, and the emulator finished booting |

- Two protocol versions were negotiated, 2025-11-25 and 2026-07-28; both work.
- `delete_instance` carries the `anthropic/requiresUserInteraction` mark, so under `-p` it is always blocked and the instance is not deleted. That is what the mark is for. To delete an instance from an unattended script, use the command line `delete <i> --yes`.
- If a create started over MCP is disconnected halfway, it cannot finish and is recorded as failed; redo it with `create --resume N`.
- Not yet checked in Claude Code:
  - the tools `check_host`, `shell`, `install_apk` and `set_window_visible` have not been called;
  - there was no dedicated run with `MCP_PROTOCOL_NEGOTIATION=legacy`;
  - whether the `notifications/progress` of `start_instance` increase one by one was not checked: the `-p` output only shows Claude Code's own heartbeat every 30 seconds.

## The manager ModAVD.exe

Double-click `ModAVD.exe` in the package root: it starts a local server that listens on 127.0.0.1 only, then opens a window in Edge's app mode (`msedge --app`, built into Win10 / 11); if Edge is not found, it uses the default browser. The pages are HTML / CSS / JS embedded in the exe and need no network; it goes online only when you click "Check for updates" (`检查更新`) in "Settings" (`设置`).

- Features: instance cards (with a screen thumbnail when the instance is online), start and shut down (with progress; it always starts with the window, headless starts are for the CLI only; there is no restart and no showing / hiding of windows), create, settings, delete, computer check, activity panel, screen viewer (a click is a tap; Back / Home / Recent tasks), logs, install APK, open folder; light / dark / follow system.
- Headless instances show “无头” in the card subtitle, with an explanation on hover; they can be shut down in the manager. Hiding a window after a windowed start does not make the instance headless.
- Security: each launch uses a random token, which is exchanged for an HttpOnly cookie (its name includes the port, so managers for two root folders running at the same time do not overwrite each other); it validates Host (against DNS rebinding) and Origin; strict CSP.
- One root folder gets one server; double-clicking again just reopens the window. The state file is in `%LOCALAPPDATA%\ModAVD\`, and the log is at `<root>\logs\modavd-ui.log`.
- After the window is closed, it waits for the running tasks to finish before exiting.
- Options: `--root`, `--port`, `--no-window` (start only the server), `--dev` (exposes a fake backend under `/dev/`, for adjusting page styles).
- The front end / back end API is in `internal/ui/API.md` (in Chinese). The pages are in `internal/ui/web/`; the icons are fan art (see `internal/ui/web/assets/README.md` and [NOTICE](NOTICE)) and are not licensed under the code license.

## Updating ModAVD

`modavd update` updates the whole ModAVD package in the root folder (the emulator, components and scripts, and also `ModAVD.exe` and `tools\modavd.exe` themselves). It does the same thing as AUTO-MAS's "ModAVD one-click update" (`魔改 AVD 一键更新`), and uses the same `<root>\.update\` folder and state file: if an update is interrupted partway on either side, the other can finish it or roll it back. Under the current integration agreement, AUTO-MAS keeps its own updater for now; whether and when it switches to calling this command is up to the AUTO-MAS project's own maintenance, and this is not a feature or promise already delivered by this repository.

- It does only the step you explicitly ask for; it never goes online automatically and never downloads in the background:
  - `modavd update` (`status`) looks at local state only; `check` looks up the latest version;
  - `prepare` downloads, unpacks, verifies file by file against the `modavd-manifest.json` in the package, and stages the result; instances can be running;
  - `install` runs `prepare` first if needed, then swaps in `sdk\emulator` and the other files in the package. All instances, manager windows and MCP clients must be closed first; if any program is using the files, it changes nothing (exit code 5), and the staged new version is kept so the next run can swap it in directly;
  - `recover` finishes or rolls back a swap that was left unfinished last time (it also runs first at the start of every step).
- The update source is chosen in the manager's "Settings" (`设置`): GitHub (the latest Release of `ModAVD/ModAVD`, the full package) or MirrorChyan (downloads need a CDK, and it can provide delta packages; on errors it downloads from GitHub instead and says so in the result's `notices`). The CDK is encrypted with Windows DPAPI and stored in `%LOCALAPPDATA%\ModAVD\update-settings.json`; only this Windows user can decrypt it, and it is not written to logs and does not appear on the command line or in API responses. For now the manager can only change the settings and check for updates; the "Update" (`更新`) button is not available.
- Only files from the package are swapped: `avd\`, `mas-avd.json` (only `components.emulator` is changed and an `updateHistory` entry is recorded), `sdk\platform-tools`, `sdk\system-images`, `logs\` and so on are never touched; `sdk\emulator` is replaced as a whole. A root folder that sets `sdkRoot` in `mas-avd.json` is not updated.
- `--automas-version` is the version of the calling AUTO-MAS: if the first line of the new version's announcement requires a higher AUTO-MAS, it does not install.
- Exit codes: being unable to check (offline, rate-limited, a GitHub error) is 9 (`update_unavailable`), a downloaded package that fails verification is also 9 (`update_rejected`), and a program in use or an update already in progress is 5.
- Downloads share resumption, response validation and timeouts with `components fetch`; see the next section. The defaults are 20 seconds for connecting and receiving headers and 30 seconds for a stalled body read; `update` has no timeout flags yet. GitHub keeps `.part` files for resumption. MirrorChyan URLs are treated as single-use addresses, with each attempt starting from zero. The proxy rules are also shared.

## Fetching the Google components

The ModAVD release package does not include adb (platform-tools 37.0.1) or the Android 14 Google APIs x86_64 system image (r14, about 1.5 GB, about 4.2 GB unpacked): they belong to Google, are governed by the Android Software Development Kit License Agreement, and cannot be redistributed. `modavd components fetch` downloads them from official channels and installs them into `sdk\`. The manager's "补全组件" button and this command share the same logic; the first interactive run asks for a download source and remembers it:

- First it fetches the full license text from `repository2-3.xml` on a mirror and displays it (stating which mirror it came from and which components will be downloaded), and continues only if you type `y`; otherwise nothing is downloaded and `downloads\` gets no extra files. `--accept-license` skips the prompt, but the full text is still fetched and displayed, and if it cannot be fetched it still exits; with `--json` it is required, otherwise the command always reports `usage` (exit code 2), before it even looks at the root folder and whether or not components are missing (up to 0.3.0 it succeeded when nothing was missing). The license text and the prompt are not affected by `--quiet`.
- Mirrors are tried in this order: googledownloads.cn → Tencent Cloud mirror → dl.google.com. When something is to be downloaded, a line "下载源：…" (download source) first says which ones this run uses and why (default / `--mirror` / `--mirror-url`). `--mirror` uses only one of them (case-insensitive); `--mirror-url` uses your own directory URL in place of those three (it can be given several times or comma-separated, and the URLs are tried in order). A mirror from which the license manifest could not be fetched is moved to the end when downloading.
- Component fetches and updates share the download implementation: data goes into `.part` files and interrupted transfers resume with `Range`. A 200 must match the expected size; a 200 answering a range request truncates the old data and resets progress to zero. A 206 must match the start, end and total size; a 416 keeps the existing partial file. Requests carry `Accept-Encoding: identity`, reject compressed responses, allow at most 10 redirects and use `ModAVDManager/<version>` as User-Agent. Components use `downloads\`; a complete local `.part` is verified first, and a bad one is downloaded again from the first mirror. Files are renamed only after size and SHA-1 verification. HTTP 4xx, a size mismatch, compressed content or a timeout switches to the next mirror immediately (the `.part` is kept so the next mirror can continue it); other errors are retried up to 3 times on the same mirror, waiting 3 seconds between tries.
- It unpacks to `sdk\.staging\`, and moves the result into place only after the whole package is unpacked and verified (key files, the SHA-256 pinned for the three adb files, the version); a missing `source.properties` is written in. A previously incomplete directory is renamed `<dir>.incomplete-<time>` and kept, with `-2`, `-3` and later suffixes on collisions. If a later component fails, the JSON error's `advice` also lists the moved directories. Delete them yourself once you are sure you do not need them. Ctrl+C stops it at any time and leaves no half-installed component.
- Components that are already complete are skipped; by default the zip is deleted after a successful install, and `--keep-downloads` keeps it.
- It refuses first if the path has non-ASCII characters, if the folder is not a ModAVD root folder, if `mas-avd.json` sets `sdkRoot`, or if there is not enough space (the download plus the unpacked size, plus 500 MB spare). `--test-free-space-mb` is for tests only and pretends that only that many MB are free.
- Timeouts are shared with `update`. `components fetch` accepts `--connect-timeout` (connecting, the TLS handshake and receiving the response headers; 1-600 seconds, default 20), `--stall-timeout` (not a single byte received; 1-600 seconds, default 30), `--manifest-timeout` (fetching the manifest as a whole; 1-1800 seconds, default 40). Proxy: if `HTTPS_PROXY` / `HTTP_PROXY` is set, it follows the environment variables; otherwise it uses a proxy set manually in the Windows "Internet Options". `NO_PROXY` applies to both; setting it alone does not disable the system proxy. Automatic configuration scripts (PAC) are not supported; in that case set `HTTPS_PROXY`.
- It writes only `downloads\`, `logs\fetch-components.log` (and the `.lock` beside it; only one fetch runs at a time per root folder), the two component directories under `sdk\` (with `--play-store` and `import-playstore` also `sdk\playstore`), and `sdk\.staging\`. It does not touch the adb server, does not write the registry, and does not need administrator rights. The log records only the start and end of each download; progress is shown but not logged.
- Only Windows has a component manifest; macOS and Linux return `unsupported` (exit code 4).
- Exit codes: not accepting the license agreement is 2 (`license_declined`); a path with Chinese characters or not enough space is 4 (`precheck_failed`), and a folder that is not a ModAVD root folder or has `sdkRoot` set is also 4 (`not_ready`); a fetch already in progress is 5 (`busy`); failing to download the license or the components from any mirror is 9 (`components_unavailable`), and a downloaded zip whose SHA-1 is wrong on every mirror, or that fails verification after unpacking, is also 9 (`components_rejected`); being unable to read the free space is 1.
- `components status` (the default) and the components entry of `check` give the same result, and the rule for judging is the same as in AUTO-MAS (the key files being present counts as installed). `fetch` is stricter about what counts as "complete" (following the script: the key files must be non-empty, and for adb the pinned hash and version number are also verified), so it can happen that `status` shows a component as installed while `fetch` still reinstalls it, in which case the old directory is renamed and kept.

## Building

You need Go 1.26.5 (the `toolchain` line in `go.mod`) and PowerShell 7 (`pwsh`). In the repository root:

```powershell
pwsh -NoProfile -File build.ps1          # -> out\ModAVD.exe, out\tools\modavd.exe
pwsh -NoProfile -File build.ps1 -Test    # run go vet and go test first
pwsh -NoProfile -File build.ps1 -Out <dir> -Go <go.exe of go1.26.5>
```

- Which go: `-Go`; if not given, the environment variable `MODAVD_GO`; otherwise the go on PATH. If the version differs from the `toolchain` in `go.mod`, it reports an error and never quietly builds with another version.
- It uses `-trimpath -buildvcs=false` and does not use `-X` to write the version number: the same source gives byte-identical results on any machine and in any directory.
- It uses the standard library only, with no third-party dependencies, and builds offline too (`GOPROXY=off`).
- `.gitattributes` makes all text files in the repository LF (binary files such as icons excepted): `go:embed` embeds the bytes in the working tree, and an exe built from CRLF files would differ.

## Testing

```powershell
go vet ./...
go test ./...
```

Tests run on Windows only. Optional environment variables:

| Variable | Purpose |
| --- | --- |
| `MODAVD_TEST_TMP` | Temporary files of the tests go under this folder, one subfolder per package (`adb`, `win`, `emu-ops`, `report`); if not set, Go's default temp folder (under `%TEMP%`) is used. Set it if you do not want test data on drive C. |
| `MODAVD_TEST_PYTHON` | The Python 3 used for the comparison tests; if not set, `python` on PATH is used, and if none is found, those comparison tests are skipped. |
| `MODAVD_TEST_IME_APK_DIR` | The folder holding the host IME APKs (`modavd-ime.apk`, `modavd-ime-testapp.apk`); if not set, the tests that read these two APKs are skipped. |
| `MODAVD_TEST_LIVE` | Set to `1` to run the tests that touch the real environment: looking at the emulators currently running on this machine, and starting a real adb server. They touch instances in use on this machine, so do not set it normally. |

- Some tests in `internal/avd`, `internal/root` and `internal/jsonord` run the original AUTO-MAS logic with Python 3 (such as `internal/avd/testdata/instances_ref.py`) and compare it with the Go output byte for byte, so Python 3 must be installed.
- A few tests only read existing files under `D:\ModAVD` on this machine (`mas-avd.json`, instance configs, component APKs) and are skipped if they are absent. `MODAVD_TEST_METADATA` can point to a different `mas-avd.json`, `MODAVD_TEST_SDK` to a different SDK that has `platform-tools\adb.exe`, and `MODAVD_TEST_VKLIBS` to a folder holding the original and the patched `libvulkan_enc.so` (the byte-for-byte comparison for the Vulkan fix).
- CI (`.github/workflows/ci.yml`) runs on `windows-2025`: format check, `go vet`, `go test`, `build.ps1`, the privacy scan and the author check. It runs only on PRs or when triggered manually.

## Standalone download and hashes

- Each version's GitHub Release attaches `ModAVDManager-<version>-win-x64.zip` and `SHA256SUMS.txt`; the latter is the same file as `release/<version>.SHA256SUMS.txt` in the repository. v0.1.0 is the first standalone version, and its two exes are byte-identical to those in the ModAVD 0.36.0 package.
- The zip has no top-level folder. It contains `ModAVD.exe`, `tools\modavd.exe`, `LICENSE`, `NOTICE`, `LICENSES\Go-LICENSE.txt`, and `modavd-manager.json`, the marker for standalone download packages (the version number and the SHA-256 of each of the other files).
- To verify: run `Get-FileHash .\ModAVDManager-0.1.0-win-x64.zip -Algorithm SHA256` and compare the result with its line in `SHA256SUMS.txt`; compare the two unpacked exes the same way.
- To build the same bytes yourself: Go 1.26.5, `GOTOOLCHAIN=local`, `pwsh -NoProfile -File build.ps1 -Out out`, then `go run ./internal/tools/mkzip -dir out -version <version>` to make the zip.
- When using it standalone, pass `--root` or set `MODAVD_ROOT`; otherwise it will say it cannot find the ModAVD root folder. Do not unpack the zip into the ModAVD root folder over the files from the package.
- The standalone manager does not update itself; download the new version and replace it by hand. The one in the ModAVD package is updated with ModAVD (see "Updating ModAVD").

## Contributing

- Report problems and suggest features in the [Issues](https://github.com/ModAVD/ModAVD/issues) of the ModAVD repository, and pick "Manager and CLI"; this repository only takes PRs.
- There is only one maintainer, and replies can take a few days. Open an issue in the ModAVD repository before changing code, saying what you want to change and why; a large PR that was not discussed first may not be accepted.
- Commits carry your own name or a pseudonym, with your own GitHub noreply address as the email (`<id>+<login>@users.noreply.github.com`, shown in your GitHub email settings).
- Machine-readable output (`--json`, exit codes, MCP tools) is a contract for callers such as AUTO-MAS: you may add fields; if you change a meaning or remove a command or a field, also increment `Protocol` in `internal/buildinfo`.
- The README exists in Chinese (`README.md`) and English (`README.en.md`), matching section by section: if you change one, change the other in the same PR. If you can write only one language, say so in the PR and the maintainer will fill in the other.
- The other conventions (how to write workflows, attribution and licensing, AI tools, image changes) are the same as in ModAVD's [CONTRIBUTING.en.md](https://github.com/ModAVD/ModAVD/blob/main/CONTRIBUTING.en.md).
- For the environment, hardware, how to verify, and model suggestions when using AI tools, see [Development: the manager and the CLI](https://modavd.github.io/en/develop/manager) on the documentation site.

## License

- The code is released under the GNU Affero General Public License, version 3 or later (AGPL-3.0-or-later); see [LICENSE](LICENSE). Copyright 2026 qiyinxi.
- If you distribute this program (modified or not), or put a modified version online for others to use, you must provide the corresponding source under the same license. Other programs that merely call `modavd.exe`, the MCP server or the manager's local interface are not affected.
- The exceptions are in [NOTICE](NOTICE):
  - The program icons (`cmd/modavd-gui/modavd.ico`, `cmd/modavd-gui/rsrc_windows_amd64.syso`, and the images under `internal/ui/web/assets/`) are fan art of Eyjafjalla from *Arknights*; the copyright in the character belongs to Shanghai Hypergryph, and it is outside this repository's license;
  - `internal/avd/testdata/instances_ref.py` is copied from AUTO-MAS (AGPL-3.0-or-later), is used only for tests, runs in a separate process, and is not compiled into the program.
- The built exes contain the Go standard library and runtime (BSD-3-Clause).
