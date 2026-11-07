# macOS update package contract

Native Darwin/ARM64 builds use `modavd-macos-update-v1`, independently of the Windows/AUTO-MAS transaction format. Local packages are selected only by `update prepare` or `update install` with both `--package ZIP` and `--sha256 HEX`. Obtain that SHA256 through a trusted independent channel. Installation of a staged package and interrupted-transaction recovery need no network or saved credentials.

A ZIP contains `modavd-update-manifest.json` at its payload root, without an enclosing directory. The transport manifest is not installed. Its object has these fields:

```json
{
  "schema": 1,
  "format": "modavd-macos-update-v1",
  "version": "v0.37.0",
  "build": 37,
  "platform": "macos-arm64",
  "units": ["sdk/emulator", "ModAVD.app", "tools", "modavd-manifest.json"],
  "files": {
    "tools/modavd": {"type": "file", "mode": 493, "size": 123, "sha256": "..."},
    "sdk/emulator/resources/Current": {"type": "symlink", "target": "versions/A", "sha256": "..."}
  }
}
```

The example is structural; it is not an installable inventory. Every non-directory payload entry must appear exactly once. File hashes cover bytes; symlink hashes cover the exact UTF-8 target bytes. Modes are decimal Unix permission bits (493 is 0755). Only ordinary files and symlinks are accepted. Directories are inferred. `xattrs` optionally maps the supported `com.apple.cs.*` signature attributes to base64 bytes; packaging must capture them when a signature uses them. Quarantine and unrelated attributes are not copied.

The four units above are required. Other allowed unit names are explicitly listed in `nativeAllowedUnits`; guest data, root settings, platform-tools and system-images are never units. Paths must be normalized ASCII and case-unambiguous. Links must resolve entirely within their own unit, with no dangling targets, cycles or traversal through files. ZIP and manifest must agree on type, mode, size, hash and link target.

These ordinary files must contain ARM64 Mach-O code and have executable bits:

- `ModAVD.app/Contents/MacOS/ModAVD`
- `tools/modavd`
- `tools/modavd-manager`
- `sdk/emulator/emulator`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64-headless`

The manager opens the system browser; no separate WebView helper is required. `ModAVD.app/Contents/Info.plist` must name `ModAVD` as `CFBundleExecutable`. `sdk/emulator/source.properties` must have `Pkg.BuildId=mas-<build>`, the headless QEMU binary must contain `free-page-reporting`, and the installed `modavd-manifest.json` version must match the transport version. Every Mach-O is checked with `codesign --verify --strict`; the app and emulator trees additionally use `--deep --strict`. This checks integrity, including ad hoc signatures; it does not authenticate a trusted publisher or notarization. Each verification process is time-bounded, and the verification pass has a total deadline.

Limits are 20,000 ZIP entries, 4 GiB unpacked bytes, a 16 MiB transport manifest, 12 MiB encoded signature attributes, and a 1 MiB transaction journal. Writes retain at least 20 GiB free space and stop if free space cannot be determined. Local input is copied privately before digest verification and extraction. Staged content is hashed and signed code reverified before replacement; modification time alone is never trusted.

Installation holds managed instance start locks and scans running executable paths under the SDK, app and tools. These checks are advisory on macOS: a program launched outside the manager can race them. Close all instances, manager windows and MCP clients first. The executing updater itself can replace its pathname while its original inode continues running. Each unit is journaled before/after renames; recovery verifies the complete candidate across staged and live units, then finishes it or restores originals. The metadata undo record restores this transaction's emulator version and history on rollback while preserving unrelated settings. Corrupt or ambiguous journals retain the scene for inspection.

Remote release selection uses `macos-arm64`, requires a SHA256 and full-package size, and refuses missing assets as `update_unavailable`. Publishing packages in this format, distribution signing and packaging-profile alignment belong to the later release-packaging stage.
