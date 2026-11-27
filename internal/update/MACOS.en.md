# macOS update package contract

[简体中文](MACOS.md) | **English**

<!-- MACOS.md and MACOS.en.md match section by section: if you change one, change the other in the same PR. -->

The native Darwin/ARM64 build uses `modavd-macos-update-v1`, independent of the Windows/AUTO-MAS transaction format. Only `update prepare` or `update install` can pick a local update package, and both `--package ZIP` and `--sha256 HEX` must be given. Get the SHA256 digest from a trusted, independent source. Installing a staged package and recovering an interrupted transaction need neither the network nor any saved credentials.

The ZIP root must contain `modavd-update-manifest.json` directly, with no extra directory around it. This transport manifest is not installed into the target directory. Its JSON object has these fields:

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

The example only shows the structure; it is not a complete file list you could install. Apart from the transport manifest itself, every non-directory entry in the package must appear in the manifest exactly once. A regular file's digest is computed from its content bytes; a symbolic link's digest is computed from the raw UTF-8 bytes of its target string. Modes are decimal Unix permission bits, for example 493 for octal 0755. Only regular files and symbolic links are accepted; directories follow from the entry paths. The optional field `xattrs` maps the supported `com.apple.cs.*` signature extended attributes to the base64 encoding of their values; when the signature uses these attributes, packaging must keep them. Quarantine attributes and unrelated extended attributes are not copied.

The four update units above are all required; the other allowed unit names are listed explicitly in `nativeAllowedUnits`. Guest data, root folder settings, platform-tools and system-images can never be update units. Paths must be normalized ASCII paths with no case ambiguity. A symbolic link must resolve entirely inside its own unit, with no dangling target, no loop and no path through a regular file. The ZIP contents must match the manifest entries in type, mode, size, digest and link target.

These entry points must be regular files containing ARM64 Mach-O code, with the executable bit set:

- `ModAVD.app/Contents/MacOS/ModAVD`
- `tools/modavd`
- `tools/modavd-manager`
- `sdk/emulator/emulator`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64-headless`

The manager opens the system browser and needs no separate WebView helper. `CFBundleExecutable` in `ModAVD.app/Contents/Info.plist` must be `ModAVD`. `Pkg.BuildId` in `sdk/emulator/source.properties` must match the transport manifest's `build`: when `build` is an integer N (the old form, 0.41 and earlier) it must be `Pkg.BuildId=mas-N`; when it is a version string (such as `"build": "0.42.0"`) it must be the same version, `Pkg.BuildId=0.42.0`. The headless QEMU binary must contain `free-page-reporting`, and the version in the bundled `modavd-manifest.json` must equal the version in the transport manifest. Every Mach-O is checked with `codesign --verify --strict`; the app and emulator directory trees are also checked with `--deep --strict`. These checks verify signature integrity and also accept ad hoc signatures; they do not prove that the publisher is trusted or that Apple notarized the package. Each check process has a time limit, and so does the whole round of checks.

The limits are: 20,000 ZIP entries, 4 GiB in total after extraction, a 16 MiB transport manifest, 12 MiB of encoded signature extended attributes, and a 1 MiB transaction journal. Writing must leave at least 20 GiB free; when the free space cannot be determined, the operation stops. A local input package is first copied to a temporary file of the updater's own, then its digest is checked and it is extracted. Before replacing anything, the digests of the staged content are computed again and the code signatures checked again; modification times alone are never taken as proof that the content can be trusted.

During installation the updater holds the start lock of the managed instances and scans for running executables under the SDK, app and tools directories. On macOS these checks are cooperative: a program started outside the manager can still race with them. Close all instances, manager windows and MCP clients before updating. The running updater can replace its own path; the old process keeps running on the old inode. The transaction journal is written before and after each unit is renamed. On recovery, the complete candidate version made up of the units in the staging area and in the current install is verified first, and then the installation is completed or the original files are restored. On rollback, the metadata undo record restores only the emulator version and update history this transaction changed, and keeps unrelated settings. A damaged or ambiguous transaction journal is left in place for inspection.

A remote release is chosen by its `macos-arm64` asset, which must come with a SHA256 digest and the full package size; with no compatible asset the result is `update_unavailable`. Publishing update packages in this format, distributing signatures and aligning the packaging configuration are left to a later release packaging stage.
