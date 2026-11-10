# macOS 更新包约定

原生 Darwin/ARM64 构建使用 `modavd-macos-update-v1`，与 Windows/AUTO-MAS 的事务格式相互独立。只有 `update prepare` 或 `update install` 可以选择本地更新包，而且必须同时指定 `--package ZIP` 和 `--sha256 HEX`。SHA256 摘要须从可信的独立渠道取得。安装已暂存的更新包、恢复中断的事务，都不需要联网或读取已保存的凭据。

ZIP 根目录须直接包含 `modavd-update-manifest.json`，不得在外面再包一层目录。这份传输清单不会安装到目标目录。其 JSON 对象包含以下字段：

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

以上示例仅展示结构，并非可直接安装的完整文件清单。除传输清单自身外，包内每个非目录条目都必须在清单中恰好出现一次。普通文件的摘要根据文件内容字节计算；符号链接的摘要根据链接目标字符串的原始 UTF-8 字节计算。权限使用十进制 Unix 权限位表示，例如 493 对应八进制 0755。只接受普通文件和符号链接，目录由条目路径推导。可选字段 `xattrs` 将受支持的 `com.apple.cs.*` 签名扩展属性映射为属性值的 base64 编码；签名使用这些属性时，打包必须保留。隔离属性和无关的扩展属性不会复制。

上述四个更新单元均为必需项，其他允许的单元名称在 `nativeAllowedUnits` 中明确列出。客体数据、根目录设置、platform-tools 和 system-images 均不能作为更新单元。路径必须是规范化的 ASCII 路径，且不得存在大小写歧义。符号链接的整个解析过程必须保持在所属单元内，不得存在悬空目标、循环链接或穿过普通文件的路径。ZIP 内容与清单中的条目类型、权限、大小、摘要和链接目标必须一致。

以下入口必须是包含 ARM64 Mach-O 代码的普通文件，并设置可执行权限位：

- `ModAVD.app/Contents/MacOS/ModAVD`
- `tools/modavd`
- `tools/modavd-manager`
- `sdk/emulator/emulator`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64`
- `sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64-headless`

管理器打开系统浏览器，无须独立的 WebView 辅助程序。`ModAVD.app/Contents/Info.plist` 中的 `CFBundleExecutable` 必须为 `ModAVD`。`sdk/emulator/source.properties` 必须包含 `Pkg.BuildId=mas-<build>`，无界面 QEMU 二进制必须包含 `free-page-reporting`，随包安装的 `modavd-manifest.json` 中的版本必须与传输清单中的版本一致。每个 Mach-O 都使用 `codesign --verify --strict` 校验；应用和模拟器目录树还需使用 `--deep --strict` 校验。这些检查验证签名完整性，也接受 ad hoc 临时签名，但不证明发布者可信或已通过 Apple 公证。每个校验进程都有执行时限，整轮校验也有总时限。

各项上限为：20,000 个 ZIP 条目、4 GiB 解压后总量、16 MiB 传输清单、12 MiB 编码后的签名扩展属性，以及 1 MiB 事务日志。写入时必须保留至少 20 GiB 可用空间，无法确定剩余空间时停止操作。本地输入包先复制到更新器专用的临时文件，再校验摘要并解压。替换前会重新计算暂存内容的摘要，并重新校验代码签名，不会仅凭修改时间判断内容可信。

安装期间会持有受管理实例的启动锁，并扫描 SDK、应用和工具目录下正在运行的可执行文件路径。这些检查在 macOS 上属于协作式排他，管理器之外启动的程序仍可能与检查发生竞态。更新前须关闭所有实例、管理器窗口和 MCP 客户端。正在执行的更新器可以替换自身路径，原进程继续使用旧 inode 运行。每个单元重命名前后都会记录事务日志；恢复时，先校验由暂存区和当前安装目录中的单元共同组成的完整候选版本，再继续完成安装或还原原文件。回滚时，元数据撤销记录只恢复本事务修改的模拟器版本和更新历史，保留无关设置。遇到损坏或存在歧义的事务日志时，会保留现场供检查。

远端发布版本按 `macos-arm64` 选择资产，必须提供 SHA256 摘要和完整包大小；缺少兼容资产时返回 `update_unavailable`。按本格式发布更新包、分发签名和打包配置对齐，由后续发布打包阶段负责。
