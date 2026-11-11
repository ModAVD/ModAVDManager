# HTTP API of the modavd manager (`internal/ui` ↔ `internal/ui/web`)

[简体中文](API.md) | **English**

<!-- API.md and API.en.md match section by section: if you change one, change the other in the same PR. -->

An internal contract. The front end (`web/`) and the back end (`*.go`) are written against this document; change both sides together. Field names and structures are the same as the `data` of the CLI's `--json` output (`internal/emu/types.go`).

## Connection and security

- The server listens only on `127.0.0.1:<port>` (a random port by default). One server serves one root folder, fixed at startup.
- The launcher opens `/?t=<token>`: the server replies with `Set-Cookie: modavd_ui_<port>=<token>; HttpOnly; SameSite=Strict; Path=/` and a 302 redirect to `/`. The token is generated randomly (32 bytes) on every start. The cookie name includes the port because cookies are scoped by host, not by port, and all manager windows share one Edge profile; with a common name, a manager opened later would overwrite the cookie of the one opened earlier.
- Every `/api/*` request must carry this cookie; without it, or with a wrong one, the server answers 401.
- A request whose `Host` is not `127.0.0.1:<port>` or `localhost:<port>` always gets 403 (protection against DNS rebinding).
- Non-GET requests: `Origin` must be the server's own address (browsers send it), otherwise 403. The request body is JSON (`Content-Type: application/json`), except for uploading an APK and importing Google Play store files.
- Static files `/`, `/app.css`, `/js/*`, `/assets/*` do not need the cookie (the page has to load before it can send one), but the Host check still applies.

## Errors

HTTP 4xx / 5xx with the body `{"error":{"code":"…","message":"…","advice":"…","details":{…}}}`. `code` is an error code from `internal/errs`. `details` appears only when there is extra machine-readable information (currently only `boot_diag_dir`, the folder of the guest's state, when a boot fails; see the README). The `error` of a job (see below) has the same shape. Status codes:

| code | HTTP |
| --- | --- |
| `usage`, `invalid_argument` | 400 |
| `unauthorized` (not signed in: the cookie is missing or wrong) | 401 |
| `forbidden` (Host or Origin is not this server) | 403 |
| `not_found` | 404 |
| `busy`, `already_exists`, `port_conflict` | 409 |
| `not_ready`, `precheck_failed`, `unsupported` | 422 |
| `canceled` | 499 |
| `launch_failed`, `insufficient_memory`, `update_unavailable`, `update_rejected`, `proxy_unavailable` | 502 |
| `timeout` | 504 |
| anything else | 500 |

## Endpoints

| Method | Path | Request | Response |
| --- | --- | --- | --- |
| GET | `/api/info` | | `{"version","protocol","root":RootInfo}` |
| GET | `/api/instances` | | `ListResult` |
| GET | `/api/instances/{i}` | | `InstanceInfo` |
| POST | `/api/instances` | `{title?, index?, memory_mb?, cpu?, data_partition_gb?, fps?, resolution?}` | 202 `Job` (`kind` is `create`: it boots in the background, finishes the initial setup and shuts down again, which takes a few minutes. When `index` is not given, the server picks the number first, and `Job.index` is the new instance's; the result is an `InstanceInfo`). A new instance comes without the Google Play store (there is no `play_store` field; sending one returns 400). If you want it, use `playstore-install` below after the instance is created |
| PATCH | `/api/instances/{i}` | `{title?, memory_mb?, cpu?, fps?, resolution?, data_partition_gb?, diagnostics?: bool}` | `UpdateResult`; the diagnostics setting takes effect immediately, ordinary configuration at the next boot |
| GET | `/api/instances/{i}/diagnostics/preview` | | `{"index","files","bytes"}`: the number of files and bytes that would be cleaned up when diagnostics is turned off; it changes no configuration and deletes no files |
| DELETE | `/api/instances/{i}?confirm=1` | without `confirm=1` the server returns 400 `usage` | `DeleteResult` |
| POST | `/api/instances/{i}/start` | `{}` | 202 `Job`: always boots with a window (the manager never boots headless; headless boot is command line only). If the instance is already running headless, the job fails with code `busy` (no switching while running) |
| POST | `/api/instances/{i}/stop` | `{force?: bool, cancel_create?: bool}` | 202 `Job`; while the instance is being created this returns busy by default, and only an explicit `cancel_create: true` cancels; `force` only controls whether Android's normal shutdown is skipped. A canceled creation is marked `interrupted_create: true` in the result |
| POST | `/api/instances/{i}/install` | `multipart/form-data`, file field `apk` | 202 `Job` (the server stores the file in a temporary file and deletes it when the job ends) |
| POST | `/api/instances/{i}/disk-info` | `{}` | 202 `Job` (`kind` is `disk-info`): the usage of the data disk, which can be queried at any time (while the instance is running, `allocated_bytes` and `reclaimable_bytes` are not returned and `needs_shutdown` is `true`); the result is emu's `DiskInfoResult`. While a compaction is running or the lock is unavailable, it returns the file information it can read plus `warnings` and does not run qemu-img; when the host usage cannot be read, `on_disk_bytes` is omitted |
| POST | `/api/instances/{i}/disk-trim` | `{}` | 202 `Job` (`kind` is `disk-trim`): asks the running Android to reclaim space; `done` and 100% only mean that the trigger request has returned, it does not wait for or confirm that the guest's FITRIM has finished, and the job's duration is the duration of the request; if the instance has not finished booting or the emulator version does not support it, the job fails with code `unsupported`; the result is a `DiskTrimResult` |
| POST | `/api/instances/{i}/disk-compact` | `{}` | 202 `Job` (`kind` is `disk-compact`): compacts the data disk file of a stopped instance; if the estimated gain is too small, the rewrite is skipped; if the instance is not stopped, the job fails with code `busy`; the result is a `DiskCompactResult` |
| POST | `/api/instances/{i}/resume` | `{}` | 202 `Job` (`kind` is `create`): redoes the initial setup of an instance whose creation failed |
| POST | `/api/instances/{i}/playstore-install` | `{}` | 202 `Job` (`kind` is `playstore-install`): installs the five files in the root folder into this instance (same as `modavd playstore <i> install`: puts them into the bridge's writable overlay, hides the placeholder app, and cold boots once; a stopped instance is stopped again when done, a running one is booted back the way it was started; takes a few minutes); the result is a `PlayStoreResult`. Checks run first, and if one fails the error is returned immediately without starting a job: the root folder does not have the five files yet (or they are incomplete or corrupt) is 422 `not_ready` with `details.reason` `playstore_files_missing` (the page guides the user to tick it in "Complete components", or to import); the instance has no execution bridge is 422 `not_ready` with `details.reason` `no_bridge`; an arm64 guest is 422 `unsupported` (`unsupported_guest`); an instance being created is 409 `busy`; a missing instance is 404. If the guest script in the job finds that the system is not writable, that is also `not_ready` / `no_bridge`. When the job fails midway, the guest script withdraws the files it put in this time; if it cannot withdraw them cleanly (or the script was cut off before it finished), the instance's `play_store_partial` becomes `true`, and the page offers both "Complete" and "Remove". An instance that was stopped before is stopped again after a failure |
| POST | `/api/instances/{i}/playstore-uninstall` | `{}` | 202 `Job` (`kind` is `playstore-uninstall`): removes the store from the instance (removes the updates installed by Google Play, deletes the five files, and restores the placeholder app after a cold boot; same as `playstore <i> uninstall`); the result is a `PlayStoreResult`. It does not need the five files and does not check for the execution bridge; an instance with only part of the store installed is also removed with this |
| POST | `/api/instances/{i}/gles32-on` | `{}` | 202 `Job` (`kind` is `gles32-on`): an experimental feature that makes the instance report `ro.opengles.version` as 196610 (GLES 3.2), so that Google Play no longer judges games whose manifest requires GLES 3.2 as incompatible (same as `modavd gles32 <i> on`). It only changes the reported version; actual rendering is still 3.1. It edits `/vendor/build.prop` in the writable system image overlay that the execution bridge provides, and takes effect after one cold boot; a stopped instance is stopped again when done, a running one is booted back the way it was started, which takes a few minutes; after the cold boot the version reported by the guest is checked, and the job fails if it does not match. The result is a `GLES32Result`. Checks run first, and if one fails the error is returned immediately without starting a job: the instance has no execution bridge is 422 `not_ready` with `details.reason` `gles32_no_bridge`; an arm64 guest is 422 `unsupported`; an instance being created is 409 `busy`; a missing instance is 404. If the guest script in the job finds that the system is not writable, that is also `not_ready` / `gles32_no_bridge`. While it is on, `bridge <i> uninstall` refuses |
| POST | `/api/instances/{i}/gles32-off` | `{}` | 202 `Job` (`kind` is `gles32-off`): removes the line written above; after a cold boot the instance reports 3.1 again (same as `gles32 <i> off`); the result is a `GLES32Result`. The checks are the same as for `gles32-on` |
| GET | `/api/instances/{i}/playstore-gsfid` | | `{"index","decimal","hex"}`: the GSF ID of a running instance (same as `modavd playstore <i> gsfid --json`; the decimal form is what GSF records, the hex form is the same number, with negative numbers in two's complement); takes a few seconds. Used to register at https://www.google.com/android/uncertified/. If the instance is not running (according to the latest list) or a job is running on it, it is 409 `busy`; if there is no ID yet it is 422 `not_ready` with `details.reason` `no_gsf_id` (stay online for a minute or two and try again) |
| POST | `/api/instances/{i}/input` | `{type:"tap", x, y}` (the coordinates are in the pixels this instance displays (1280×720 or 1920×1080, with width and height swapped in portrait)) or `{type:"key", key:"back"\|"home"\|"recents"}` | `{}` |
| GET | `/api/instances/{i}/screenshot` | | `image/png` (`adb exec-out screencap -p`), `Cache-Control: no-store`; 409 if the instance is not running |
| GET | `/api/instances/{i}/log?kind=emulator\|logcat&lines=300` | | `text/plain; charset=utf-8`, the tail of the newest log; 404 if there is none |
| GET | `/api/instances/{i}/proxy` | | `ProxyView`, see below |
| PUT | `/api/instances/{i}/proxy` | `{enabled?: bool, server?: string, apps?: [string]}` | `ProxyView`: the items given are replaced with the new values, the ones not given stay unchanged; giving no item at all returns 400 `usage`. `server` may omit the protocol (it is detected) or be `auto` (take this computer's proxy settings); when it is turned on for the first time without an address, `auto` is used as well. If the address cannot be probed, 502 `proxy_unavailable`. If the instance is running, it takes effect immediately and returns within a few seconds |
| GET | `/api/instances/{i}/packages` | | `{"packages":[{"package","name","uid","system"}]}`: the third-party apps installed in the instance, plus "Google Play services" (`system: true`) if it is installed; `name` is the Chinese name of a recognized game, or "Google Play services", and `""` for the rest. Recognized games come first, then the rest sorted by package name. 409 `busy` if the instance is not running |
| GET | `/api/proxy/suggest` | | `{"server","notes"}`: this computer's proxy settings (same as `modavd proxy --server auto`), converted into the address as seen from inside the instance, with the protocol probed when the settings do not say it is SOCKS5; `notes` says where it was taken from. If no proxy is set, 502 `proxy_unavailable` |
| POST | `/api/proxy/probe` | `{server}` | `{"server","host_addr","kind","udp","udp_error","notes"}`: probes this address from this computer: whether it is a SOCKS5 or an HTTP proxy, and for SOCKS5 sends a DNS query through it to test UDP. `server` is converted into the address as seen from inside the instance (`127.0.0.1` and `localhost` become `10.0.2.2`) and the protocol is set to the one detected; `notes` says what was changed. If it cannot connect, 502 `proxy_unavailable` |
| POST | `/api/open` | `{what: "root"\|"logs"\|"instance"\|"collect-info", index?, job?}` | `{}`; opens in File Explorer. `collect-info` opens the folder holding the problem pack made by that finished collect-info job (`job` is the job id); it does not accept paths from the page |
| GET | `/api/components` | | `{"root","components":[…],"play_store":PlayStoreStatus}`: component status; the last item of `components` is the summary of the optional Google Play store (`id` is `play-store`, `optional: true`, `installed` when all five files are present), and `play_store` holds its details, see below |
| GET | `/api/components/license` | `?source=auto\|googledownloads_cn\|tencent\|google\|custom&mirror_urls=…` (repeatable; the saved download source is used when omitted) | `{"license":"full text","license_source":"the mirror it came from","license_sha256":"SHA-256 of the full text"}`: the license agreement fetched from this download source, the same one that is fetched when completing components; it does not download components |
| GET | `/api/components/source` | | The current component download source: `source` is `auto`, a built-in mirror ID or `custom`, plus `mirror_urls` and `configured` |
| PUT | `/api/components/source` | `{"source":"auto"\|"googledownloads_cn"\|"tencent"\|"google"\|"custom", "mirror_urls":[...]}` | Saves the current user's component download source; a custom address accepts only an HTTP(S) directory |
| POST | `/api/components/fetch` | `{"accept_license":true, "license_sha256":"…", "source"?:..., "mirror_urls"?:[...]}` (`accept_license` must be explicitly `true`; `license_sha256` is that of the license agreement the user has seen, taken from the endpoint above, for the same download source) | 202 `Job` (`kind` is `components`, a root job, `index` is `-1`). Without `source` / `mirror_urls`, the saved download source is used. If the license agreement the background job fetches does not match `license_sha256`, the job fails with `components_rejected` and downloads nothing. With `"play_store": true` it also completes the optional Google Play store: if the root folder does not have the five files yet, it downloads the Google Play system image (about 1.5 GB), extracts the five files, then deletes both the archive and the extracted `system.img`, keeping only the five; if they are already there (downloaded or imported) it skips this |
| POST | `/api/components/import-playstore` | `multipart/form-data`, file field `file`, one per file (1–8 files): the official Google Play system image archive `x86_64-34_r14.zip` from Google, or the five files themselves (any file names) | 202 `Job` (`kind` is `playstore-import`, a root job, `index` is `-1`): "Use my own files". It downloads nothing and does not ask for the license agreement. The server stores the upload in a temporary folder and deletes it when the job ends; the job recognizes each file by size; the archive is checked against the size and SHA-1 in Google's repository manifest, and each of the five files against its hard-coded size and SHA-256; the result is a `PlayStoreImportResult`. A bad upload is answered with 400 immediately: not multipart is `usage`, no `file` field is `usage`, an empty file, more than 8 files, or a single file bigger than the archive is `invalid_argument`. When a file is rejected the job fails with `components_rejected` and changes nothing, with `details.reason`: `unknown_file` (the size is neither the archive nor one of the five files), `mismatch` (the size matches but the hash does not), `incomplete` (only some were given and the root folder does not have the rest); another completion or import running is 409 `busy`; not enough space is 422 `precheck_failed` |
| POST | `/api/diag/collect` | `{out_dir?, stage_dir?, keep_stage?}` | 202 `Job` (`kind` is `collect-info`, a root job, `index` is `-1`) |
| GET | `/api/diag/report-config` | | `{"text":"…"}` |
| GET | `/api/check?index={i}&refresh=1` | both parameters may be omitted | The original fields of `CheckResult`, plus `instance_index`, `memory_mb` and `root_ready` (takes a few seconds: the hardware acceleration check and the Vulkan probe) |
| GET | `/api/jobs` | | `{"jobs":[Job]}`, newest first, at most 50 |
| GET | `/api/jobs/{id}` | | `Job` |
| POST | `/api/jobs/{id}/cancel` | | `Job` |
| GET | `/api/events` | | `text/event-stream`, see below |
| GET | `/api/update` | | `UpdateView`, see below (does not use the network) |
| POST | `/api/update/check` | `{}` | `UpdateView`: looks up the latest version from the configured update source (without a CDK); 409 `busy` while a check is in progress |
| POST | `/api/update/settings` | `{source?: "github"\|"mirrorchyan", cdk?: string}` | `UpdateView`; a `cdk` of `""` clears it; the CDK is stored encrypted and never returned |
| POST | `/api/update/apply` | `{}` | 202 `Job` (`kind` is `update`, a root job, `index` is `-1`): updates this root folder, see "Update" below; the result is `{handoff, version, stopped, message}` |

`{i}` is the instance number (0–9, except 5).

- JSON request bodies: unknown fields return 400 `usage` (a misspelled field name is visible on the page right away); an empty body means all defaults; a PATCH with no field at all returns 400 `usage`.
- Screenshot, input, listing apps and getting the GSF ID on an instance that is not running: 409 with code `busy`.
- `InstanceInfo` carries `diagnostics: bool`, and `diagnostics_since` when it is on. When PATCH changes diagnostics, `UpdateResult` carries `diagnostics_cleanup: {files, bytes}`, reporting only what was actually deleted. The preview for turning it off includes old logs of other ordinary instances that must be evicted after rejoining the ordinary root-wide 1 GiB pool; active files are outside the scope of both the preview and the deletion. The state on disk may change between the preview and the commit, so the page goes by the result of the commit. If turning it off partly fails, the switch is already saved as off, and submitting `diagnostics: false` again continues the cleanup and the guest restoration.
- When the diagnostics setting succeeds but the host logcat is collected by an external program, `UpdateResult.warnings` says that it was not taken over; the external process is not ended, and the call does not wait for it to switch before returning. The front end shows this notice.
- The `index` of the computer check selects the check target: it reads that instance's `config.memory_mb` for its next boot, returns 404 if it does not exist, and 400 `invalid_argument` if `index` is empty, repeated or an invalid number. When it is not given, the check uses the default 6144 MB and `instance_index` is `null`; when given, it returns that number. `memory_mb` is always the memory actually used by this check. The cache is keyed by that memory value; after an instance's memory is changed, the result of the old configuration is not reused, and `refresh=1` re-runs both the hardware acceleration and the graphics checks. `ready` includes this check's memory check, while `root_ready` looks only at the general blockers other than memory; on macOS, a general blocker that cannot be confirmed also counts as not passed. Insufficient memory for one check target should not disable other instances; at boot, the pre-check is still redone against the target instance's actual memory.

### Job

```json
{"id":"j12","kind":"start","index":8,"state":"running",
 "created_at":"2026-10-28T15:04:05+01:00","finished_at":"",
 "progress":{"step":"boot","message":"等安卓开机（已 18 秒）","done":2,"total":6},
 "result":null,"error":null,"warnings":[]}
```

- `kind`: `start` / `stop` / `install` / `create` / `components` / `collect-info` / `disk-info` / `disk-trim` / `disk-compact` / `playstore-install` / `playstore-uninstall` / `playstore-import` / `gles32-on` / `gles32-off` / `update` (since 0.3.0 there is no restart and no show / hide window).
- `state`: `running` / `done` / `failed` / `canceled`.
- `progress.step`: for boot, emu's `launch` / `boot` / `tune` / `ime` / `first_boot` / `vulkan_fix` / `done`; shutdown (including waiting for a canceled boot to stop) is `stop`, installation is `install`, and these two end with `done`; disk jobs are `disk_info` / `disk_trim` / `disk_compact` while running and `done` when finished (while running there is no percentage and `total` is 0, so the page shows an indeterminate progress bar; when finished, `done` = `total` = 1000); each round of installing the execution bridge is `bridge`; installing or removing the store is `playstore-install` / `playstore-uninstall` while running (no percentage) and `done` when finished; during an import, checking the files is `verifying` and extracting the image is `extracting` (`done` / `total` are bytes); creation is `create`, the steps above for the several boots in it (the end of a boot is `setup`), `create_stop`, and `done`, with the several boots merged into one progress that never goes backwards. `total` is always 1000.
- For an install job, `result.apk` is the file name the user uploaded, not the server's temporary path (the temporary file is deleted when the job ends).
- `progress` is `null` when there is no progress; `result` is a `StartResult` / `StopResult` / `InstallResult` / `DiskInfoResult` / `DiskTrimResult` / `DiskCompactResult` / `InstanceInfo` (creation) / `PlayStoreResult` / `PlayStoreImportResult` / `GLES32Result`, and `null` until the job completes; `error` is the error object when the job failed, otherwise `null`.

`DiskCompactResult` keeps a non-negative `freed_bytes`. `before_on_disk_bytes` and `after_on_disk_bytes` are the actual host usage before and after the compaction, and the new `on_disk_delta_bytes` equals the latter minus the former, where a positive value means the usage grew. `skipped=true` means the checks passed but the expected gain was too small, so the original disk was not rewritten; in that case the usage before and after is equal, and the change and the freed amount are both zero. The result also contains `reclaim_threshold_bytes` and an omittable `estimated_reclaimable_bytes`; see [the compaction budget and result contract](../emu/COMPACT.md) for details.
- Only one job can run on an instance at a time; a second one gets 409 `busy`. **Exception**: while a boot job is running, a stop can be sent (consistent with emu: a boot in progress can be stopped all the same).
- Canceling a boot only stops waiting; the emulator keeps booting and is not forcibly ended.
- `components`, `playstore-import` and `collect-info` are root folder jobs with `index` `-1`, and only one runs at a time; canceling is passed on to the context of the diagnostics back end. While `components` or `playstore-import` is running, boot, creation, and store install / uninstall return 409 `busy`, and the other way around (they change `sdk\`).

### Event stream `/api/events`

```
event: instances
data: <ListResult>          sent once right after connecting, then every 3 seconds, and also at each step of a job

event: job
data: <Job>                 whenever a job changes in any way

event: log
data: {"time":"…","level":"info|warn|error","msg":"…","job_id":"j12"}   job_id is "" for messages that do not belong to a job

event: update
data: <UpdateView>          when the check or the settings change, and after the update job has finished downloading

event: proxy
data: {"marks":[ProxyMark]} the badges on the per-app proxy cards, in full; sent once on connecting (if there have been any), then on every change

event: ping
data: {}                    every 15 seconds
```

### Per-app proxy `ProxyView`, `ProxyMark`

`ProxyView` is all the fields of `AppProxyResult` (the same as the `data` of `modavd proxy <i> --json`: `index`, `enabled`, `server`, `apps`, `dns`, `applied`, `notes`, `guest`, `guest_error`, `error`; `guest_error` is the reason when the instance is running but the guest cannot be queried), plus `mark`: a `ProxyMark` when the instance is running, has finished booting, is not booting or shutting down, and the setting is on with a non-empty list; otherwise `null`.

- `applied`: `""` for GET; for PUT it is `now` (already in effect in the guest), `failed` (the guest did not accept it, `error` says why), `booting` (booting; takes effect right after the boot completes), `next_boot` (not running), `off` (turned off; neither its program nor its rules remain in the guest).
- `ProxyMark` is `{"index","tone","text"}`:
  - `ok`: the program in the guest is running, the rules are in place, and the proxy is reachable.
  - `warn`: a few rules are missing and are being re-added; or the guest's latest connection through the proxy did not succeed but this computer can reach the proxy (the target itself may not be responding); or the guest could not be queried this time (adb or su did not respond).
  - `err`: the apps in the list are currently offline or connecting directly: the guest has no program running for it, the rules cannot be attached, or the proxy is unreachable (the guest's latest connection failed and this computer cannot reach it either).
  - `text` is a sentence for people to read.
- The badges on the cards: while a page is connected, the server queries every running instance that has this setting on and has finished booting once every 30 seconds (one root command), and queries immediately when an instance's state or settings change; instances that are booting, shutting down or doing post-boot setup are not queried. The results of GET and PUT also update the badges.
- An instance that is not running has no `ProxyMark`: the page shows "takes effect at next boot" according to `InstanceInfo.app_proxy` (a summary of the read-only configuration).

### Google Play store `PlayStoreStatus`, `PlayStoreImportResult`, `PlayStoreResult`

On Apple Silicon macOS, component download / import uses API35 Google Play ARM64 r9, archive `arm64-v8a-35_r09.zip`; installing and removing inside an instance still returns `unsupported`. Windows stays on API34 x86_64 r14. `PlayStoreStatus` adds `archive` (the name of the official archive that can be imported), `download_bytes` (the archive size) and `install_supported` (whether the current host already supports installing and removing in instances); the UI shows sizes, import hints and the capability boundary according to these fields. On macOS, `ready` also checks the ABI, API and image type in `source.properties`, which still does not replace the digest verification at import / download time.

The store is optional: the root folder keeps five files (`sdk\playstore\`; tick it when completing components to download them, or import your own files), and each instance gets them installed when asked; new instances do not come with it.

- `PlayStoreStatus` (`play_store` of `GET /api/components`, the same as `data.play_store` of `modavd components status --json`): `{"dir","ready","source","version","archive","download_bytes","install_supported","files":[{"name","guest","size","present"}]}`.
  - `ready`: all five files are present and the version in `source.properties` is right (only sizes are checked; the SHA-256 is checked when they are put in place and when they are installed into an instance). The instance's "Complete Google Play store" button uses it to decide whether to install directly or to guide the user to complete components first.
  - `source`: `download` (downloaded when completing components), `import` (imported from your own files), and `""` if there is none or the origin is unknown.
  - `files` always has five items, in the same order as the documentation site: `Phonesky.apk`, `PrebuiltGmsCore.apk`, `GoogleServicesFramework.apk`, `PartnerSetupPrebuilt.apk`, `play_store_fsi_cert.der`; `guest` is the path after installation into an instance, and `present` means this file exists with the right size.
- `PlayStoreImportResult` (the result of the `playstore-import` job, the same as `modavd components import-playstore --json`): `{"root","from","used","kept","skipped","set_aside","status":PlayStoreStatus}`.
  - `from`: `zip` (an archive was given) or `files` (the files themselves were given); `used`: the files that passed the check and were used (by the file name at upload); `kept`: the files not given this time that were already in the root folder and checked fine, so they were kept.
  - `skipped`: the root folder already had all five files and the given files were checked too, so nothing was changed.
  - `set_aside`: the new location of a previously incomplete `sdk\playstore` after being renamed (`sdk\playstore.incomplete-<time>`), or `""` if there is none.
- `PlayStoreResult` (the result of the `playstore-install` / `playstore-uninstall` jobs, the same as `modavd playstore <i> … --json`): `{"index","action","state","installed","files","restarted","stopped","warnings"}`; `state` is the situation in the guest after the job: `installed`, `absent` or `partial` (a leftover `.new` that has not been put in place also counts as `partial`).
- `InstanceInfo.play_store` (bool): the instance has the store installed (by the record in `mas-avd.json`: `true` after a successful install, `false` after removal; the guest is not queried). The buttons use it to show "Complete Google Play store" or "Remove Google Play store", and when installed, one more item "Copy GSF ID" (available after boot); an instance whose `bridge` is `off` cannot install it (it answers `no_bridge`).
- `InstanceInfo.play_store_partial` (bool): the last completion failed midway and could not be fully withdrawn (`mas-avd.json` records `"partial"`). Then `play_store` is `false`, and the page offers both "Complete" and "Remove"; `bridge <i> uninstall` still refuses.

### Reporting GLES 3.2 (experimental)

Some games require GLES 3.2 in their manifest, and the Play store judges ModAVD (which reports 3.1) as incompatible by `ro.opengles.version`. This switch only changes the version the instance reports to 3.2; actual rendering is still 3.1: games that use Vulkan work, while apps that really need GLES 3.2 rendering will install but fail to open. It is off by default, per instance, and needs the execution bridge (what it edits is the writable system image overlay the bridge brings).

- `GLES32Result` (the result of the `gles32-on` / `gles32-off` jobs, the same as `modavd gles32 <i> … --json`): `{"index","action","state","enabled","prop","restarted","stopped","notes","warnings"}`; `state` is the situation in the guest after the job: `on` (the line is there and the guest reports 196610), `off` (the line is not there and the guest does not report 196610), plus `pending-on` / `pending-off` when the cold boot has not finished; `prop` is the guest's `ro.opengles.version`; `notes` has one entry when the instance has the Play store installed: the store remembers the old device information, so go to Settings → Apps → Google Play Store → Storage and clear the data (the account stays signed in).
- `InstanceInfo.gles32` (bool): the record of the switch (`gles32` in `mas-avd.json`; the guest is not queried). The UI uses it to show "Turn on / off reporting GLES 3.2 (experimental)" under "More actions"; an instance whose `bridge` is `off` cannot turn it on (it answers `gles32_no_bridge`), but one that is already on can still be turned off.
- Choosing files on the page works like installing an APK: `<input type="file">` (multiple selection allowed), one `file` field appended per file, sent as `multipart/form-data` with `FormData`.

### Update `UpdateView`

All the fields of `update.Status` (the same as the `data` of `modavd update --json`: `root`, `updatable`, `not_updatable_reason`, `current_version`, `current_build`, `source`, `cdk_set`, `checked`, `check_error`, `latest_version`, `latest_source`, `update_available`, `required_automas`, `release_url`, `phase`, `prepared_version`, `last_result`, `recovered`, `updated`, `message`, `notices`), plus `checking`, `checked_at` (RFC 3339, `""` if no check has been made since this start), `error` (the error object of the last check, `null` if none), `can_install` (`POST /api/update/apply` can be tried: the manager knows how to reopen itself), `install_by_cli` (the opposite of `can_install`: it can only be installed with the command line), and `handoff` (the result that the command line left after the last click on "Update" handed over to it, `{at, ok, updated, version, message, error: {code, message, advice} | null}`; `null` if this manager was not the one reopened by that run, or if the result is older than 30 minutes).

- Only clicking "Check for updates" or "Update" uses the network; there is no automatic check.
- The update source and the CDK are settings of the current system user, used by both the manager and the command line when updating; AUTO-MAS's own updater uses AUTO-MAS's settings. Windows uses `%LOCALAPPDATA%\ModAVD\update-settings.json` and DPAPI; macOS uses `~/Library/Application Support/ModAVD/update-settings.json`, with the random AES-GCM wrapping key of the CDK kept in the current user's Keychain. When the keychain is locked or access is not authorized, saving fails, no authorization prompt is shown, and no plaintext key is saved.
- "Update" (`POST /api/update/apply`): the manager cannot replace itself while running, so it takes three steps.
  1. The job (`kind` is `update`, a root job) first downloads, checks against the manifest and stages the new version (same as `modavd update prepare`, with instances still running as usual); if it is already the latest, it stops there and the result has `handoff` `false`.
  2. It first checks for situations that would make the package swap fail, and if there is one, no instance is stopped and the job fails: another process is running this root folder's `ModAVD.exe` or `tools\modavd.exe` (for example `modavd mcp`); an instance is being created; the staged version is not newer than the installed one (which counts as already the latest). Then it shuts down all running instances normally (same as `stop --all`, but without canceling an instance being created).
  3. It uses `host.StartOrphan` to start the root folder's `tools\modavd.exe --root <root> --json update install --after-pid <manager pid> --reopen <manager program> --reopen-arg …` (output is appended to `logs\modavd-update.log`). The job counts as successful only if it is still running half a second later, with the result `{handoff: true, version, stopped, message}`; from then on every other job gets 409, and the manager exits after about 1.5 seconds. The command line waits for the manager to exit, swaps in the new version, writes the result to `.update\manager-result.json`, and reopens the manager with the original arguments (`--root`, plus `--port`, `--no-window` and `--dev`); the new manager reads that result at startup and puts it into `handoff`. If the swap cannot be done (for example a program is holding a file), the manager is reopened all the same, and the staged new version is kept so it need not be downloaded again next time.
- When the manager installs, it does not do the AUTO-MAS version check (the same as installing by hand with the command line); when `required_automas` is not empty, the page prompts the user to update AUTO-MAS first with AUTO-MAS.
- Refused before starting, with no job (422): not Windows, or the manager does not know how to reopen itself (`unsupported`); the root folder has no `tools\modavd.exe`, or its version differs from the manager's (`not_ready`; an older command line does not recognize these options). When another job is running, 409 `busy`; while an update job is running, other jobs also get 409.
- Native macOS update is done with the CLI's `update prepare/install --package ZIP --sha256 HEX`, selecting a local signed package; after staging, `install/recover` can run offline. Without a compatible `macos-arm64` remote asset, the check result is `update_unavailable`. The UI offers no install entry; for the package protocol see `internal/update/MACOS.md`.


## Lifecycle

- `ModAVD.exe` (GUI subsystem) starts the server and then opens a window in Edge's app mode: `msedge --app=<address> --user-data-dir=%LOCALAPPDATA%\ModAVD\ui-edge2 --disable-features=msImplicitSignin,msEdgeStartupBoost --disable-sync --disable-extensions`. If Edge cannot be found, the default browser is used.
  - The manager's own profile does not sign in, does not sync and does not load extensions: otherwise Edge would automatically sign in with the Windows account on the new profile, sync and install the user's extensions, and an extension's welcome page would open a full browser window.
  - Closing the window does not start "startup boost": otherwise Edge would start another `msedge --no-startup-window`, which carries no `--user-data-dir` and uses the user's own profile.
  - The page declares that it should not be translated (`translate="no"`), so Edge does not pop up "Translate this page" when its UI language is not Chinese.
  - Versions up to 0.3.0 used `ui-edge`, without these switches; it might already be signed in to the user's account, with bookmarks, history and passwords synced in, so a new folder is used.
  - After the new version opens its window, the old `ui-edge` is deleted; if Edge is still using it (an old manager is open), this is skipped and done at the next opening. If it cannot be deleted, this is only logged and opening is not affected.
- Only one server runs per root folder: double-clicking again reopens the existing window.
- Apple Silicon macOS opens the system browser by default; `--chrome-window` switches to a Chrome app window, using the separate profile `~/Library/Application Support/ModAVD/ui-chrome` with sync and extensions turned off. If Chrome is unavailable, it falls back to the default browser. The single-instance lock and the state are also in that user's `ModAVD` folder, opening folders is left to Finder, and the root folder can be located automatically from the enclosing `ModAVD.app`.
- After the window is closed (the Edge process exits, or no event stream has been connected for 60 seconds), the server exits once the running jobs have finished.
  - If the Edge process exits within 10 seconds of starting: it is taken as having handed the window over to an Edge already running on the same profile, which does not count as closing the window; after that the event stream rules apply.
  - After the window is really closed, the server exits if no event stream is connected within 5 seconds (it waits for jobs to finish first, if any).
  - `--dev` never exits because of idleness.

`resolution` accepts only 1080 and 720. A new instance defaults to 1080; they correspond to 1920×1080 / DPI 280 and 1280×720 / DPI 240 respectively; a change keeps the orientation and takes effect at the next boot. `InstanceInfo.config.resolution` is the tier recognized from the current configured width and height; after a change while running, it means the setting for the next boot.

Tap coordinates on the screen are converted and validated against the size of the current screenshot. When the tier is changed while running, `config` means the setting for the next boot, and the current picture and tap coordinates keep the old size until the restart.
