# ModAVDManager

**简体中文** | [English](README.en.md)

<!-- README.md 和 README.en.md 逐节对应：改了其中一份，请在同一个 PR 里把另一份也改了。 -->

[ModAVD（魔改 AVD）](https://github.com/ModAVD/ModAVD) 的管理程序：图形管理器 `ModAVD.exe`，以及命令行和 MCP 服务 `modavd.exe`。Go 写，只用标准库。目前只支持 Windows x64，平台相关的部分已经拆成平台层，其他平台的安排见 [ModAVD#27](https://github.com/ModAVD/ModAVD/issues/27)。

> **版本提示（`main` 与 Release）**：本文按仓库 `main` 的实现编写。当前 `main` 的版本是 `0.3.0`，最新独立 Release 是 `v0.1.0`（对应 ModAVD `0.36.0`），`v0.2.0` 已起草、还没发布；`0.2.0` 起的新建后台初始化、`update` 命令和退出码 9 都不在 `v0.1.0` 下载包里。使用已发布包时，请以对应 Release 的说明和包内版本为准。

使用说明和参与开发的说明在 ModAVD 文档站：[管理器和命令行](https://modavd.github.io/components/manager)、[开发：管理器和命令行](https://modavd.github.io/develop/manager)。

- [这是什么](#这是什么)
- [和 ModAVD 的关系](#和-modavd-的关系)
- [命令行](#命令行)
- [MCP](#mcp)
- [管理器 ModAVD.exe](#管理器-modavdexe)
- [构建](#构建)
- [测试](#测试)
- [参与](#参与)
- [许可证](#许可证)

## 这是什么

- ModAVD 是基于 AOSP Android Emulator 加一批补丁的安卓模拟器，目前支持 Windows，以后计划支持更多平台。本仓库是管理它的程序：新建、修改、删除实例，开机、关机，开机前的电脑检查，装 APK 和 arm64 执行桥，看画面和日志。
- 管的是一个 ModAVD 根目录，也就是 ModAVD Release 包解压出来的文件夹（里面有 `mas-avd.json`、`sdk\`、`avd\`）。实例文件和 `mas-avd.json` 的写法与 [AUTO-MAS](https://github.com/AUTO-MAS-Project/AUTO-MAS) 一致，测试里拿 AUTO-MAS 的代码逐字节对照。
- 给人用的是管理器 `ModAVD.exe`；给脚本、AUTO-MAS 和 AI 助手用的是命令行 `modavd.exe` 和它的 MCP 服务。

## 和 ModAVD 的关系

- 本公开仓库是管理器和命令行程序的开发与发版依据，版本号独立维护，并单独提供 GitHub Release。
- ModAVD 整包仍会带着 `ModAVD.exe` 和 `tools\modavd.exe`，每次 ModAVD 发版会钉住使用的管理器版本与哈希。
- 单独下载包可在任意目录使用；给 `--root` 或设置 `MODAVD_ROOT` 指向 ModAVD 根目录，并按下面的哈希核对下载内容。不要把它解压到 ModAVD 根目录覆盖整包里的文件。

| 产物 | 源码 | 做什么 |
| --- | --- | --- |
| `<根>\tools\modavd.exe` | `main.go`、`internal/cli` | 命令行；`modavd mcp` 是 MCP 服务 |
| `<根>\ModAVD.exe` | `cmd/modavd-gui`、`internal/ui` | 图形管理器（本地服务 + Edge 应用窗口） |

两个程序共用 `internal/` 下的实现：

| 包 | 内容 | 对照的 AUTO-MAS 代码 |
| --- | --- | --- |
| `root` | 根目录布局、`mas-avd.json`、端口规则、组件检查 | `constants.py`、`components.py` |
| `avd` | 实例文件（`mas_<i>.ini`、`config.ini`）的增删改 | `instances.py` |
| `jsonord` | 保序读写 JSON，写法和 Python `json.dumps(indent=2, ensure_ascii=False)` 一样 | — |
| `emu` | 开机、开机后调优、首次初始化、执行桥、关机、状态、电脑检查 | `manager.py`、`precheck.py` |
| `adb` | 私有 adb server、重新登记、模拟器控制台 | `host.py` |
| `win` | 进程、命令行、端口、脱离进程树的启动、文件锁 | `host.py`、`windows/process.py` |
| `vkfix`、`apk`、`guest` | 客体 Vulkan 驱动补丁、APK 清单、推进客体的脚本 | `vulkanfix.py`、`res/avd/guest/` |
| `cli`、`mcp`、`ui` | 命令行、MCP、管理器的 HTTP 服务和页面 | — |

`internal/guest/scripts/*.sh` 和 AUTO-MAS `res/avd/guest/` 下的脚本是同一份，改的时候两边要一起改。

## 命令行

```
modavd [--root <目录>] [--json] [--quiet] [--verbose] <命令> [参数]
```

| 命令 | 说明 |
| --- | --- |
| `list` | 全部实例和状态 |
| `info <i>` / `status <i>` | 一台实例的详细信息 / 状态 |
| `create [--index N] [--title T] [--memory 6G] [--cpu N] [--data-gb N] [--fps 30\|60] [--resolution 1080\|720] [--no-bridge] [--timeout 1200]`、`create --resume N` | 新建，并在后台无头开机一次把初始化做完再关机（见下）。默认带 arm64 执行桥；`--no-bridge` 记下这台不要桥，以后开机也不装。`--resume N` 重做新建失败的那台的初始化 |
| `set <i> [--title T] [--memory M] [--cpu N] [--fps F] [--resolution 1080\|720] [--data-gb N]` | 修改，下次开机生效。`--data-gb` 只能改大，游戏数据不动：模拟器（0.36 起）开机时扩磁盘镜像，开机后在线扩文件系统。0.36 之前按 16 GB 及以下建的盘只能扩到下一个备份组之前（16 GB 约到 30 GB） |
| `proxy <i> [on\|off\|test] [--server URL\|auto] [--add 包名] [--remove 包名] [--apps 包名]` | 按应用走代理：名单里的应用走代理，其余直连（见下） |
| `delete <i> --yes` | 删除已关机的实例，连同游戏数据 |
| `start <i> [--window \| --headless] [--timeout 300] [--no-bridge]` | 开机（默认无头，`--window` 带窗口），等开完并做完开机后的设置，返回前确认 `/sdcard` 能写进文件（一般这时早已就绪；最多再等 60 秒，到点只警告）；实例正在关机时（比如刚点了模拟器窗口的 ×）先等它关完（最多 30 秒）再开机；实例还没有执行桥时接着装上，`--no-bridge` 这次不装。运行中不换方式：实例已经在运行时，给了 `--window` / `--headless` 而它不是这种方式，就报错（退出码 5），不接管也不重启，要换先 `stop` 再 `start`；两个都不给就照原样接管 |
| `stop <i> [--force] [--cancel-create]`、`stop --all [--force]` | 关机：前台开着应用时先按 HOME，等 2 秒让它保存；再 sync，让安卓正常关机（前台应用先收到暂停、停止），不成再 sync + kill；`--force` 是 sync + 强杀。新建中默认拒绝，`--cancel-create` 显式取消；`--all` 也关闭新建中的实例，结果标记 `interrupted_create: true` |
| `bridge <i> install\|uninstall\|verify` | 装、卸、检查 arm64 执行桥；开机时会自动装，一般不用手动跑（见下） |
| `disk <i> trim` | 磁盘回收：在开着的实例里对数据盘触发一次 FITRIM；成功只表示触发请求已返回，不等待或确认客体回收完成；模拟器太旧、不支持回收时返回不支持（退出码 4） |
| `disk <i> info` | 看数据盘的大小：逻辑大小、覆盖层在宿主上实际占的空间；关机且取得启动锁后才运行只读的 qcow2 元数据检查，给出已分配和估计可回收大小。整理中或锁不可用时，仅返回可读取的文件头、宿主占用和原因；不可读的数值省略，不修复镜像 |
| `disk <i> compact` | 整理已关机实例的数据盘；预计收益不足时跳过重写；转换后先校验再原子替换，原盘在替换成功前不动；报告前后占用和增减；大盘预算可达数十小时，正常整理基本不会超时，停止请取消；仅支持 NTFS/ReFS，拒绝快照或重写空间不足，详见[预算与结果契约](internal/emu/COMPACT.md) |
| `check [--memory M]` | 开机前的电脑检查 |
| `report-config [--no-clipboard]` | 复制电脑配置和开着实例里的游戏信息（只读） |
| `collect-info [--out-dir D] [--stage-dir D] [--keep-stage] [--no-explorer]` | 收集问题信息并打包（只读）；ZIP 完成前的 Ctrl+C 返回 `canceled`，删除未完成 ZIP 并清理暂存目录；`--keep-stage` 则保留暂存目录并在错误中给出路径 |
| `components [status]`、`components fetch [--accept-license] [--mirror ID] [--mirror-url URL] [--keep-downloads]` | 查看或补全 Google 组件（adb 和系统镜像），做的是「补全组件.bat」做的事（见「补全 Google 组件」） |
| `window <i> show\|hide`、`install <i> <apk>`、`shell <i> <命令>`、`adb <i> -- <参数>` | 窗口、装 APK、客体命令、adb 透传 |
| `update [status\|check\|prepare\|install\|recover] [--automas-version V]` | 更新根目录里的整个 ModAVD 包，按「设置」里的更新源（见「更新 ModAVD」） |
| `servers`、`version`、`mcp`、`help [命令]` | |

- `<i>` 写成 `3` 或 `mas_3`（0–9，5 除外）；内存写成 `6144`、`6144M` 或 `6G`，只有 3 / 4 / 5 / 6 GB 四档，核数 2 / 4 / 6，帧率 30 / 60。
- 根目录：`--root`，没给就用环境变量 `MODAVD_ROOT`，再没有就用 `modavd.exe` 所在目录或它的上一级。
- 0.3.0 起没有 `restart`：先 `stop` 再 `start`。旧的调用报用法错误（退出码 2），提示改用这两步。

`trim` 的 GUI 任务完成、MCP 成功返回和开机后自动回收日志都只表示触发请求已返回。
自动回收日志里的秒数是 adb/su 命令往返耗时。客体是否完成或失败，需要另看客体信号，
不能用暂时稳定的 discard 计数判定。

**arm64 执行桥**：

- 只对内置名单里的游戏生效，其余应用照旧走原来的转译层。桥装在实例自己的 system / vendor 覆盖层里，要 0.36 起的模拟器。
- modavd（命令行、MCP）和管理器每次开机都会看一眼：实例还没有桥，就在开机后的设置做完后装上，中间关机再冷启动一两次；已经有桥、但桥的文件和这个包里的不一样（更新过 ModAVD 包），就同样在设置做完后换上新的，再冷启动一次。只在这次是它拉起的模拟器时装或更新；接管已经在运行的实例时不动。更新要冷启动，所以开机后的设置做完时已经有应用在前台、或者正在装 APK，这次就先不更新。装不上、更新不上都只是警告，实例照常运行，下次开机再试。AUTO-MAS 自己开机的实例不经过这一步：要换新文件，先关掉实例，再用 ModAVD.exe 或 `modavd start` 开一次机，或者跑 `bridge <i> install`。
- 搬根目录前，先在原位置用新版管理器或 `modavd start` 给每台已装桥的实例冷启动一次，再关机。把整个根目录一起搬走，包括实例目录里的 `modavd-bridge-backings.json`。下次开机发现旧路径失效时，管理器会用记录的大小和 SHA-256 核对新 SDK 里的 system / vendor 镜像。老实例已经搬走且没有记录时，请保留覆盖层，将原镜像和根目录恢复到原位置，再按上述步骤搬迁。
- 不要桥的实例：新建时带 `--no-bridge`，或者之后跑 `bridge <i> uninstall`。两种都会在 `mas-avd.json` 里记下 `instances.<i>.bridge` 为 `false`，以后开机不再自动装；再 `bridge <i> install` 就恢复。`start` 的 `--no-bridge` 只管这一次。
- `bridge` 调用 ModAVD 包里的 `tools\modavd-bridge.ps1`。`install`：需要时关机再开机，装上；已经装了的只在载荷有变时更新。`uninstall`：卸掉桥、开机标记和装桥时改成的 GL 传输方式（asg）；退回更早的版本之前要先卸。实例原来关着的，做完再关掉；开着的，中间冷启动后照原来的方式开回来，带窗口的还带窗口，无头的还无头。`verify`：只读核对，实例要开着。
- `info` / `list` / `status` 的 `bridge`：`off`（记了不要桥）、`pending`（下次由 modavd 拉起的开机会装）、`installed`（开着的客体里桥在）、`unknown`。只问已经开完机的客体；关着或还在开机的实例，开机标记在桥装好后一直留着，装失败时也在，所以首次开机做过、标记也在时是 `unknown`，要知道确切结果就开机后再查。
- 装桥期间（含其中的关机、冷启动）`busy` 为 `bridging`；这时 `delete`、再次 `bridge` 返回实例忙（退出码 5）。调用方不认识的 `busy` 取值一律当忙处理。
- 不经 modavd 或管理器开机的实例（别的程序直接拉起模拟器）不会自动装，要装就跑一次 `bridge <i> install`。

**按应用走代理**（`proxy`）：

- 名单里的应用（按包名）的 TCP、UDP 和 DNS 都走设好的代理，别的应用照旧直连。不用 VPN：客体里一个 root 守护进程（`modavd-proxyd`，随 `modavd.exe` 带进去）按应用的 uid 用策略路由和透明代理接走它们的流量，应用和系统都看不到 VPN。
- `modavd proxy <i>` 查看：开关、地址、名单；实例开着时附上客体里的实际状态（规则在不在、代理连没连上、名单里哪些还没装）。
- `on` 打开、`off` 关掉（地址和名单留着）；`--server` 改地址，`--add` / `--remove` 增删，`--apps` 整个换掉名单（可写多次或用逗号隔开，`--apps ""` 清空）；可以和 `on` 一起给。名单最多 64 个。
- 地址写 `socks5://IP:端口` 或 `http://IP:端口`；`127.0.0.1`、`localhost` 记成实例里看到的 `10.0.2.2`；不写协议就在这台电脑上探测。`--server auto`（第一次 `on` 没给地址时也是）取这台电脑的代理设置：先看 `ALL_PROXY`、`HTTPS_PROXY`、`HTTP_PROXY`，再看 Windows「Internet 选项」里手动设的代理（有 `socks=` 时优先），再探测是 SOCKS5 还是 HTTP。只认 IPv4 地址和 localhost。
- 代理软件的混合端口（Clash 系的 7890、7897 之类）会被认成 SOCKS5，TCP、UDP、DNS 都能走。只给 HTTP 代理时只转 TCP 和 DNS，名单里的应用的 UDP 不通。
- **代理断了，名单里的应用就断网**，不会悄悄改走直连。DNS 经代理用 TCP 问 `1.1.1.1`，名单里的应用的 IPv6 一律不通（让它们用 IPv4）。
- 实例开着时改了马上生效（`applied` 是 `now`），关着就下次开机生效（`next_boot`）。由 modavd、MCP 或管理器开机时，开完机第一件事就是挂上规则；客体自己重启后由开机守护补上。AUTO-MAS 自己开机、不经过 modavd 的实例没有。
- `test`：在这台电脑上探测设好的地址，是什么协议、经它的 UDP 能不能用。探测不到是退出码 9（`proxy_unavailable`）。
- 海外游戏用 Google 登录的，把 Google Play 服务（`com.google.android.gms`）也加进名单。
- 设置存在 `mas-avd.json` 的 `instances.<i>.appProxy`（`enabled`、`server`、`apps`），删除实例时一起删掉。`info` / `list` 的 `app_proxy` 是它的概要（只读配置，不走 adb；没设过是 `null`）。`collect-info` 对开着、又设过的实例另收客体守护进程的状态和日志最后 200 行。

**机读输出**（`--json`）：

- stdout 只有一行：`{"ok":true,"data":…}` 或 `{"ok":false,"error":{"code","message","advice","details"}}`。`data` 的字段见 `internal/emu/types.go`，snake_case，所有字段一直出现。
- 进度日志写 stderr，一行一个 JSON：`{"time","level","msg"}`。调用方不要把 stderr 并进 stdout。
- `version --json` 的 `protocol` 是契约版本，命令或字段删了、改了含义才加一。0.3.0 起是 2：去掉了 `restart`，`start` 加了 `--headless`，运行中的实例和要的开机方式不一样时报 `busy`、不再接管，加了 `window`、`creation_progress` 字段；0.2.0 及以前是 1。
- **端口**（0.3.1 起）：`list`、`check` 的 `port_base` 是这个根目录实际用的端口段（见下面「约定」里的 `portBase`），`status` 也带上了 `serial`、`adb_address`、`ports`（`list`、`info` 本来就有，关机的实例也有）。脚本要连实例时用这几个字段，不要自己按 20000 + 10 × 编号算。没有 `port_base` 的旧版 modavd 只会用 20000 段。
- **错误的 `details`**：可选对象，值全部是字符串。增加这些键不改变现有 `code`、`message` 或 protocol；调用方要容忍缺少字段、不认识的键和值。管理器 HTTP 响应和失败任务保留同一份字段；MCP 的 `isError` 文本在原有错误后追加一行 `error.details: {…}`，其中 `{…}` 是 JSON。
  - `reason` 细分实例操作的 `busy`：`running`（实例未关闭）、`locked`（另一操作持锁）、`starting`（关机时开机还没拉起 qemu）、`stopping`（开机等不到关机退出）、`window_mismatch`（窗口方式不同）、`port_in_use`（端口被占）、`port_other_root`（另一个根目录的同编号实例占端口）。其他 `busy` 可能没有此字段。
  - `reason=locked` 时另有 `lock`：`start`、`stop`、`boot`、`bridge`、`create`、`guest`、`disk`、`metadata` 或 `config`。`disk` 是整理磁盘或共享磁盘探测用的锁；`metadata` 是根目录 JSON 锁，`config` 是实例配置与元数据写锁。
  - `start` 的开机等待或开机后设置失败时带 `instance_status`、`qemu_running`。前者使用已知状态（如 `offline`、`starting`、`create_failed`），也可为 `not_found` 或 `unknown`；后者为 `"true"`、`"false"`、`"unknown"`。开机等待没完成时知道是 `starting`；后续设置可能重启安卓，无法确定就报 `unknown`。进程观察与是否收集客体现场共用一次快照，发生在最多 30 秒的现场收集之前；不保证调用方读到时状态未变。
  - `create` 失败时另带 `instance_exists`（`"true"` / `"false"` / `"unknown"`），并在清理结束后填 `instance_status`、`qemu_running`，覆盖内部 `start` 过时的状态。`instance_exists` 只表示该编号当前存在实例文件或元数据，包括不完整残留和原本已存在的实例，**不表示属于这次创建**；已有实例、并发创建、参数错误时也按此定义，不能只凭它自动删除。未选定编号或读不到事实时用 `unknown`。初始化失败的状态是 `create_failed`；另一个创建者还持锁时不打扰它，状态可能是 `unknown`。
  - `boot_diag_dir` 是开机超时、开机后设置失败等情况下客体现场存放的目录，错误包装和创建清理不会丢掉此字段。
  - 开机失败、模拟器还在时，`start` 和 `create`（包括 MCP 和管理器发起的）在返回前把客体的 `logcat -d -b all`、`dmesg`、`getprop`、`ps -A`、`/proc/meminfo`、`dumpsys activity processes` 前 300 行存进 `logs\mas_<i>-<时间>-bootfail\`，各项是否收到写在同目录的 `summary.txt` 里。
  - 每项单独限时，一共最多 30 秒；超时或出错时，已经读到的部分也留着。
  - 取消（Ctrl+C、MCP 取消）、开机中途被关机、模拟器已经不在时不收。
  - 每台实例留最新 3 个这样的目录，大小算进 `logs\` 的 1 GiB 总量。`collect-info` 把最新的 3 个收进问题包，合计到了 100 MB 就不再往下收。
  - 这些文件可能带账号信息，只存在本机。文本模式出错时，最后一行写这个目录。
- 运行中的实例增加 `started_at_ms`（Unix 毫秒），原来的 `started_at` 保留。`window`（0.3.0 起）：带窗口运行时为 `true`，无头运行或没开机时为 `false`。`stop --all` 即使部分失败也返回 `ok:true` 和结果列表，同时以第一个失败的退出码结束。
- **新建（0.2.0 起）**：`create` 建好文件后在后台无头开机一次，做完开机设置、宿主输入法、首次初始化和执行桥（中间冷启动一两次），再先 sync 后正常关机、确认 qemu 退出，才算新建完成，一般几分钟（`--timeout` 管整个过程，默认 1200 秒，到点先正常关机再记为失败）。之后第一次 `start` 和平常一样快。0.2.0 以前建的实例不受影响，照旧在第一次 `start` 时补做。
  - 这期间 `status` / `list` / `info` 的 `status` 是 `creating`、`busy` 是 `creating`（这次开机的 `running` / `booted` 可能是 `true`，以 `status` 为准），`creation_progress`（0.3.0 起）是做完的百分比（0–100，按做完的步骤算，不是按时间）；`start`、`set`、`bridge`、`delete`、同编号的 `create` 返回实例忙（退出码 5）。单实例 `stop` 也返回 busy，`--force` 不绕过保护。确实要取消，用 `stop <i> --cancel-create`，或 MCP `stop_instance` 的 `cancel_create: true`；照常先 sync、正常关机，创建流程收到取消后记为 `create_failed`，结果带 `interrupted_create: true`。`stop --all` 自动允许取消，结果逐台标记；不在新建中的实例为 `false`。
  - 失败（含新建的进程被结束）是 `create_failed`，`creation_error` 写原因、`creation_log` 是那次开机的日志；这时 `start`、`set`、`bridge` 返回 `not_ready`（退出码 4）。用 `create --resume N` 重做初始化（上次留下的模拟器还开着就先正常关掉），或者 `delete N --yes` 删掉。
  - 新字段 `creation`（`""` / `creating` / `failed`）、`creation_error`、`creation_log` 一直出现；状态记在 `mas-avd.json` 的 `instances.<i>.creationState` 等键里，做完就删掉。调用方（AUTO-MAS）认不出的 `status` 取值一律按忙处理。
  - 进度：命令行逐步打印到 stderr，每行带「新建中 NN%」；MCP 的 `create_instance` 发进度通知（带 progressToken 时）；管理器在卡片上显示「新建中 NN%」和进度条（别处发起的新建也显示），「更多」里可以取消新建，关掉窗口会在后台做完。

**退出码**：0 成功，1 内部错误，2 用法或参数不对 / 没同意许可协议，3 实例不存在，4 根目录没就绪 / 检查没通过 / 不支持，5 实例忙 / 已存在 / 端口被占，6 超时，7 启动失败，8 已取消，9 更新或补全组件时查不了、下不成 / 下载没通过核对，或者按应用走代理的代理探测不到。`shell` 不带 `--json` 时是客体命令的退出码，`adb` 是 adb 的退出码；`bridge` 不带 `--json` 时，`install` 后没装上、`uninstall` 后还在都是 1，`verify` 是脚本的退出码。

**约定**（和 AUTO-MAS 一致）：

- adb 只走根目录的私有 server（`mas-avd.json` 的 `adbServerPort`，默认 20050），不碰 5037。
- 端口：实例 i 的控制台端口是 20000 + 10 × i，adb 加 1、gRPC 加 2，编号 0–9、不含 5。
  - 开发、测试用的根目录可以在 `mas-avd.json` 里写 `"portBase": 21000`，换一段端口（这是 modavd 自己的键，AUTO-MAS 不认）。这样它的实例不会和同一台电脑上日常用的根目录（比如 AUTO-MAS 管的那个）里同编号的实例抢端口，也不会被认成那一台。
  - `portBase` 只能是 20000，或者 1100–65400 之间 100 的整数倍。设了别的端口段，`adbServerPort`、`scriptAdbServerPort` 都要配上，而且不能是 20049、20050，也不能落在 20000 段的实例端口上。
  - 不合规时，`status`、`list`、`info` 照常按 20000 段看，开机和新建都拒绝（`invalid_argument`），`check` 多一行「端口段」说明原因。
  - 改之前先关掉这个根目录的所有实例。设了 `portBase` 的根目录不要加进 AUTO-MAS。
- 模拟器、adb server、logcat 都经一个立即退出的中间进程拉起，不在 modavd 的进程树里：调用方结束 modavd（Ctrl+C、MCP 客户端断开）不会带走模拟器。
- 宿主 logcat 由 `tools\modavd.exe` 的独立采集进程写入 `logs\logcat-mas_<i>-<时间>.txt`，启动命令或管理器退出后仍继续；这次开机的 qemu 退出后，采集进程主动结束自己的 adb 并退出。每满 256 MiB 在下一处换行切分，同一条 adb 输出流接着写新文件；关闭的分段每台留最新 10 份，并计入 `logs\` 的 1 GiB 清理上限，正在写的文件不计入。
- 开机成功后，每台实例另有一个同样不在进程树里的开机守护，随 qemu 退出而结束。客体自己重启（qemu 没退）后，它补做开机调优和 Vulkan 补丁，绝不拉起新的模拟器。
- 同一实例的开机、关机按 `<根>\avd\.mas_<i>.{start,boot,stop}.lock` 排队：开机等前一次开机和关机做完；关机可以打断正在开机的实例。

**开机后调优里的宿主调度**：

- qemu 设为「高于正常」优先级。
- 绑核只在 L3 分大小几组的处理器（7950X3D 这类 X3D、Ryzen AI 300 等）上做，别的处理器不绑：
  - 选空余比例（空余核数 ÷ 这组的核数）最大的一组，一样时选 L3 大的；只开一台时就是 L3 大的那组。
  - 各组都放不下本实例的 vCPU 就不绑。
  - 空余是扣掉整台机器上所有 qemu 的 vCPU 以后算的，包括别的根目录和 AUTO-MAS 起的。那些进程只读、不改。
- 下载这类小块密集写盘时，客体里每个 vCPU 起一个忙等循环（`mas-idlepoll.sh`），不让 vCPU 停机，下载更快。
  - 循环数不超过宿主的余量：逻辑核数减去所有 qemu 的 vCPU 数（含本实例；绑了核就按那一组算）。
  - 所以逻辑核数不到本实例 vCPU 两倍的电脑上，只开一台也会少开：默认 6 核的实例在 8 线程的电脑上只开 2 个，6 线程及以下不开，下载会比开满时慢（在 7950X3D 上实测，1–2 个循环和不开差不多）。
  - 这是有意的取舍：给 qemu 自己的线程、Windows 和别的程序留出核。
分辨率只有两档：1080p 为 1920×1080 / DPI 280，720p 为 1280×720 / DPI 240。新建默认 1080p，720p 更省资源。管理器的新建和实例设置、CLI `--resolution 1080|720`、MCP `resolution` 使用同一套档位。已有实例按宽高识别：横屏或竖屏的 1920×1080 保持 1080p，其余配置及缺字段的旧配置保持 720p。换档联动宽、高、DPI，保留方向，下次冷启动生效；DPI 不单独设置，不使用运行时 `wm size` 或 `wm density`。自动化工具里的分辨率也要跟着改。

AUTO-MAS 当前优先调用包内 CLI，这条启动路径保持所选档位。没有可用 CLI 的旧根目录会回退到固定 720p 的 Python 启动路径，使用 1080p 前须更新包内 CLI。AUTO-MAS 的旧新建和显示配置表单仍需配套支持两档；可先在 ModAVD 管理器里设置。

## MCP

```
modavd mcp --root D:\ModAVD
```

同时支持 MCP 2026-07-28（无状态，`server/discover`）和 2025-11-25 / 2025-06-18（`initialize` 握手）。根目录在启动时定，不是工具参数。工具：`list_instances`、`get_instance`、`check_host`、`create_instance`、`update_instance`、`delete_instance`、`start_instance`（带进度）、`stop_instance`、`set_window_visible`、`shell`、`install_apk`、`disk_info`（数据盘大小）、`trim_disk`（开机态回收空间，模拟器要支持 discard）、`compact_disk`（关机态整理数据盘，实例须已关机）、`get_app_proxy`、`set_app_proxy`（按应用走代理）。

在 Claude Code 里注册（PowerShell 里 `--` 要加引号，否则会被吞掉）：

```powershell
claude mcp add --transport stdio --scope user modavd '--' D:\ModAVD\tools\modavd.exe mcp --root D:\ModAVD
```

或者项目的 `.mcp.json`：

```json
{"mcpServers": {"modavd": {"command": "D:\\ModAVD\\tools\\modavd.exe", "args": ["mcp", "--root", "D:\\ModAVD"]}}}
```

### 在 Claude Code 里实测过的

2026-11-03，Claude Code 2.1.317（命令行版）连 modavd 0.3.0，用 `claude -p --mcp-config <文件> --strict-mcp-config`，只在这一次调用里加载 modavd，共跑了 5 次：

| 做了什么 | 结果 |
| --- | --- |
| 一次按顺序调 7 个工具：`list_instances`、`get_instance`、`create_instance`（3 GB、4 核）、`update_instance`、`start_instance`、`stop_instance`、`delete_instance` | 前 6 个都成功；`delete_instance` 在 `-p` 下被拦（见下） |
| 只调 `start_instance`，开完机 claude 正常退出（跑了两次） | 模拟器还在，`list` 是 online，它的父进程链里没有 claude 和 modavd；之后 `modavd stop` 正常关机 |
| `MCP_TOOL_TIMEOUT` 设得比开机短，`start_instance` 开到一半超时，claude 退出 | 模拟器照常开完机，但开机后的设置没做完；再运行一次 `modavd start <i>` 就补完了 |
| 模拟器起来 8 秒后直接结束 claude 进程 | `modavd mcp` 5 秒内自己退出，模拟器照常开完机 |

- 协商到的协议有 2025-11-25 和 2026-07-28 两种，都能用。
- `delete_instance` 带 `anthropic/requiresUserInteraction` 标记，在 `-p` 下一定被拦，实例不删，这是这个标记要的效果。在无人值守的脚本里删实例，用命令行 `delete <i> --yes`。
- 经 MCP 新建时中途断开，新建就做不完，记成失败；用 `create --resume N` 重做。
- 还没在 Claude Code 里核对的：
  - `check_host`、`shell`、`install_apk`、`set_window_visible` 这几个工具没调过；
  - 没有专门用 `MCP_PROTOCOL_NEGOTIATION=legacy` 跑一轮；
  - `start_instance` 的 `notifications/progress` 是否逐条递增没核对，`-p` 的输出里只看得到 Claude Code 自己每 30 秒一条的心跳。

## 管理器 ModAVD.exe

双击包根的 `ModAVD.exe`：起一个只听 127.0.0.1 的本地服务，再用 Edge 的应用模式开一个窗口（`msedge --app`，Win10 / 11 自带）；找不到 Edge 时用默认浏览器。页面是内嵌在 exe 里的 HTML / CSS / JS，不需要联网；只有在「设置」里点「检查更新」时才联网。

- 功能：实例卡片（在线时有画面缩略图）、开关机（带进度；开机一律带窗口，无头开机只走命令行；不提供重启，也不提供显示 / 隐藏窗口）、新建、设置、删除、电脑检查、活动面板、画面查看（点击即点按，返回 / 主页 / 最近任务）、日志、安装 APK、打开目录；浅色 / 深色 / 跟随系统。
- 安全：每次启动随机令牌，换成 HttpOnly cookie（名字带端口，两个根目录的管理器同时开不会互相顶掉）；校验 Host（防 DNS 重绑定）和 Origin；严格的 CSP。
- 一个根目录只开一个服务，再双击只是把窗口重新打开；状态文件在 `%LOCALAPPDATA%\ModAVD\`，日志在 `<根>\logs\modavd-ui.log`。
- 关掉窗口后，等正在跑的任务做完再退出。
- 选项：`--root`、`--port`、`--no-window`（只起服务）、`--dev`（放出 `/dev/` 下的假后端，给页面调样式用）。
- 前后端接口：`internal/ui/API.md`。页面在 `internal/ui/web/`，图标是同人图（见 `internal/ui/web/assets/README.md` 和 [NOTICE](NOTICE)），不随代码许可授权。

## 更新 ModAVD

`modavd update` 更新根目录里的整个 ModAVD 包（模拟器、组件、脚本，也包括 `ModAVD.exe` 和 `tools\modavd.exe` 自己），和 AUTO-MAS 的「魔改 AVD 一键更新」做的是同一件事，用的也是同一个 `<根>\.update\` 目录和状态文件：哪边更新到一半被打断，另一边都能接着补完或退回。按当前对接约定，AUTO-MAS 目前保留自己的更新器；是否以及何时改成调用这个命令，由 AUTO-MAS 项目自己的维护决定，这不是本仓库已经交付的功能或承诺。

- 只做明确要求的那一步，不自动联网、不在后台下载：
  - `modavd update`（`status`）只看本地；`check` 查最新版本；
  - `prepare` 下载、解包、按包里的 `modavd-manifest.json` 逐文件核对、暂存好，实例开着也行；
  - `install` 需要时先 `prepare`，再换上 `sdk\emulator` 和包里的其他文件。要先关掉所有实例、管理器窗口和 MCP 客户端，有程序在用就什么都不动（退出码 5），暂存的新版留着，下次直接换；
  - `recover` 补完或退回上次没做完的换上（每一步开始前也会先做）。
- 更新源在管理器的「设置」里选：GitHub（`ModAVD/ModAVD` 的最新 Release，整包）或 Mirror 酱（填了 CDK 才能下载，能拿到增量包；出错时改从 GitHub 下，并在结果的 `notices` 里写明）。CDK 用 Windows DPAPI 加密存在 `%LOCALAPPDATA%\ModAVD\update-settings.json`，只有这个 Windows 用户能解开，不写日志、不出现在命令行和接口返回里。管理器里暂时只能改设置和检查更新，「更新」按钮不可用。
- 只换包里的文件：`avd\`、`mas-avd.json`（只改 `components.emulator` 并记一条 `updateHistory`）、`sdk\platform-tools`、`sdk\system-images`、`logs\` 等一律不碰；`sdk\emulator` 整个换掉。根目录在 `mas-avd.json` 里设了 `sdkRoot` 的不更新。
- `--automas-version` 是调用方 AUTO-MAS 的版本：新版的公告第一行要求更高的 AUTO-MAS 时不装。
- 退出码：查不了（断网、限流、GitHub 出错）是 9（`update_unavailable`），下载的包没通过核对也是 9（`update_rejected`），有程序在用、正在更新是 5。
- 下载和 `components fetch` 共用续传、响应校验和超时处理，见下一节；默认连接等待 20 秒、读数据停顿 30 秒，`update` 暂不提供超时参数。GitHub 保留 `.part` 供下次续传；Mirror 酱地址按一次性地址处理，每次尝试从头下载。代理规则也相同。

## 补全 Google 组件

ModAVD 的发布包里没有 adb（平台工具 37.0.1）和 Android 14 Google APIs x86_64 系统镜像（r14，约 1.5 GB，解压后约 4.2 GB）：它们属于 Google，受《Android 软件开发套件许可协议》约束，不能再分发。`modavd components fetch` 从官方渠道下载、装进 `sdk\`。随包的「补全组件.bat」从 ModAVD 0.38.1 起就是调它（第一次运行时先问下载源，再带 `--mirror` 调；0.38.0 及更早调的是 `fetch-components.ps1`，规则和这里一条一条对上）：

- 先从镜像取 `repository2-3.xml` 里的许可协议全文并显示（注明取自哪个镜像、要下载哪些组件），输入 `y` 才继续，否则什么也不下，`downloads\` 里也不多出文件。`--accept-license` 不询问，但协议全文照样取回、照样显示，取不到照样退出；`--json` 时必须给，否则一律报 `usage`（退出码 2）：在看根目录之前就报，组件齐不齐都一样（0.3.0 及以前组件齐的时候会成功）。协议和提问不受 `--quiet` 影响。
- 镜像依次试 googledownloads.cn → 腾讯云镜像 → dl.google.com，要下载时先打一行「下载源：…」说明这次用哪些、为什么（默认 / `--mirror` / `--mirror-url`）；`--mirror` 只用其中一个（不分大小写），`--mirror-url` 用自己的目录地址代替这三个（可写多次或用逗号隔开，按顺序试）。取不到许可协议清单的镜像，下载时排到最后。
- 组件和更新共用下载实现：先写 `.part`，中断后用 `Range` 续传。200 要大小相符，续传收到 200 时清空旧数据、从 0 计数；206 要核对起点、终点和总大小；416 保留已有部分。请求带 `Accept-Encoding: identity`，拒绝压缩响应；重定向最多 10 次，User-Agent 为 `ModAVDManager/<版本>`。组件写入 `downloads\`，完整的本地 `.part` 先校验，坏的从第一个镜像重下；大小和 SHA-1 都通过才改名。HTTP 4xx、大小不符、压缩过的内容、超时马上换下一个镜像（`.part` 留着让下一个接着下）；其余错误同一个镜像最多试 3 次，中间等 3 秒。
- 解压到 `sdk\.staging\`，整包解完、核对完（关键文件、adb 三个文件钉住的 SHA-256、版本）再挪到位；缺 `source.properties` 就补写。原来不完整的目录改名为 `<目录>.incomplete-<时间>` 留着，重名时加 `-2`、`-3` 等后缀；后续组件失败时，JSON 错误的 `advice` 也会列出已迁移的位置。确认没用后自己删掉。Ctrl+C 随时能停，不留下装到一半的组件。
- 已经齐全的组件跳过；默认装好就删 zip，`--keep-downloads` 保留。
- 路径里有非 ASCII 字符、不是 ModAVD 根目录、`mas-avd.json` 设了 `sdkRoot`、空间不够（下载加解压再留 500 MB），先拒绝。`--test-free-space-mb` 只给测试用，假装只剩这么多 MB。
- 超时与 `update` 共用，`components fetch` 可用 `--connect-timeout`（连接、TLS 握手、收到响应头，1–600 秒，默认 20）、`--stall-timeout`（一个字节都没收到，1–600 秒，默认 30）、`--manifest-timeout`（取清单整体，1–1800 秒，默认 40）。代理：设了 `HTTPS_PROXY` / `HTTP_PROXY` 就照环境变量，否则用 Windows「Internet 选项」里手动设的代理；`NO_PROXY` 对两者都生效，只设它不会禁用系统代理；自动配置脚本（PAC）不支持，这时请设 `HTTPS_PROXY`。
- 只写 `downloads\`、`logs\fetch-components.log`（和同目录的 `.lock`，一个根目录同时只跑一个）、`sdk\` 下的两个组件目录和 `sdk\.staging\`。不碰 adb server，不写注册表，不要管理员。日志里每次下载只记开始和结束，进度只显示不记。
- 只有 Windows 有组件清单；macOS、Linux 返回 `unsupported`（退出码 4）。
- 退出码：没同意许可协议是 2（`license_declined`）；路径有中文、空间不够是 4（`precheck_failed`），不是 ModAVD 根目录或设了 `sdkRoot` 也是 4（`not_ready`）；正在补全是 5（`busy`）；许可协议或组件从哪个镜像都没下成是 9（`components_unavailable`），下载的 zip 在每个镜像上 SHA-1 都不对、或者解压后没通过核对也是 9（`components_rejected`）；读不出剩余空间是 1。
- `components status`（默认）和 `check` 的组件一项是同一份结果，判断规则和 AUTO-MAS 一致（关键文件在就算装了）；`fetch` 判断「齐全」更严（照原来的脚本：关键文件不为空，adb 还要核对钉住的哈希和版本号），所以 `status` 显示已装、`fetch` 仍会重装的情况是有的，旧目录会被改名留着。

## 构建

要 Go 1.26.5（`go.mod` 的 `toolchain` 行）和 PowerShell 7（`pwsh`）。在仓库根目录：

```powershell
pwsh -NoProfile -File build.ps1          # -> out\ModAVD.exe、out\tools\modavd.exe
pwsh -NoProfile -File build.ps1 -Test    # 先跑 go vet 和 go test
pwsh -NoProfile -File build.ps1 -Out <目录> -Go <go1.26.5 的 go.exe>
```

- 用哪个 go：`-Go`，没给就用环境变量 `MODAVD_GO`，再没有就用 PATH 上的 go。版本和 `go.mod` 的 `toolchain` 不一致就报错，不会悄悄换别的版本编。
- `-trimpath -buildvcs=false`，不用 `-X` 写版本号：同一份源码在哪台机器、哪个目录编，结果都逐字节相同。
- 只用标准库，没有第三方依赖，离线（`GOPROXY=off`）也能编。
- `.gitattributes` 让仓库里的文本文件一律 LF（图标等二进制文件除外）：`go:embed` 嵌的是工作区里的字节，换成 CRLF 编出来的 exe 就不一样了。

## 测试

```powershell
go vet ./...
go test ./...
```

测试只在 Windows 上跑。可选的环境变量：

| 变量 | 作用 |
| --- | --- |
| `MODAVD_TEST_TMP` | 测试的临时文件放在这个目录下，每个包一个子目录（`adb`、`win`、`emu-ops`、`report`）；不设就用 Go 默认的临时目录（`%TEMP%` 下）。想让测试数据不落 C 盘时设它。 |
| `MODAVD_TEST_PYTHON` | 对照用的 Python 3；不设就用 PATH 上的 `python`，找不到就跳过那几个对照测试。 |
| `MODAVD_TEST_IME_APK_DIR` | ModAVD 输入法 APK（`modavd-ime.apk`、`modavd-ime-testapp.apk`）所在的目录；不设就跳过读这两个 APK 的测试。 |
| `MODAVD_TEST_LIVE` | 设成 `1` 才跑碰真实环境的测试：看本机正在运行的模拟器、起真的 adb server。会碰到本机正在用的实例，平时不要设。 |

- `internal/avd`、`internal/root`、`internal/jsonord` 有一部分测试用 Python 3 运行 AUTO-MAS 的原始逻辑（如 `internal/avd/testdata/instances_ref.py`），和 Go 的输出逐字节对照，所以要装 Python 3。
- 有几项测试只读本机 `D:\ModAVD` 下的现成文件（`mas-avd.json`、实例配置、组件 APK），没有就跳过；`MODAVD_TEST_METADATA` 可以另指一份 `mas-avd.json`，`MODAVD_TEST_SDK` 另指一个带 `platform-tools\adb.exe` 的 SDK，`MODAVD_TEST_VKLIBS` 指向放着原版和打过补丁的 `libvulkan_enc.so` 的目录（Vulkan 修复的逐字节对照）。
- CI（`.github/workflows/ci.yml`）在 `windows-2025` 上跑格式检查、`go vet`、`go test`、`build.ps1`、隐私扫描和作者检查；只在 PR 或手动触发时运行。

## 单独下载与哈希

- 每个版本的 GitHub Release 附 `ModAVDManager-<版本>-win-x64.zip` 和 `SHA256SUMS.txt`，后者和仓库里的 `release/<版本>.SHA256SUMS.txt` 是同一份。v0.1.0 是第一个独立版本，两个 exe 和 ModAVD 0.36.0 包里的逐字节相同。
- zip 没有顶层目录，里面是 `ModAVD.exe`、`tools\modavd.exe`、`LICENSE`、`NOTICE`、`LICENSES\Go-LICENSE.txt`，以及单独下载包的标记 `modavd-manager.json`（版本号和其余各文件的 SHA-256）。
- 核对：`Get-FileHash .\ModAVDManager-0.1.0-win-x64.zip -Algorithm SHA256`，和 `SHA256SUMS.txt` 里那一行比；解压出的两个 exe 也一样比。
- 自己编出同样的字节：Go 1.26.5、`GOTOOLCHAIN=local`，`pwsh -NoProfile -File build.ps1 -Out out`，再 `go run ./internal/tools/mkzip -dir out -version <版本>` 打 zip。
- 单独使用时给 `--root` 或设 `MODAVD_ROOT`，否则会提示找不到 ModAVD 根目录。不要把 zip 解压到 ModAVD 根目录里覆盖包里的那份。
- 单独下载的管理器不会自己更新，新版本要手动下载替换；ModAVD 包里的那份随 ModAVD 更新（见「更新 ModAVD」）。

## 参与

- 报问题、提建议都到 ModAVD 仓库的 [Issues](https://github.com/ModAVD/ModAVD/issues)，选上「管理器和命令行」；本仓库只收 PR。
- 只有一个维护者，回复可能要几天。改代码前先在 ModAVD 仓库开 issue，写清要改什么、为什么；没商量过的大 PR 可能不合。
- 提交署你自己的名字或化名，邮箱用你自己的 GitHub noreply 地址（`<编号>+<登录名>@users.noreply.github.com`，在 GitHub 的邮箱设置里能看到）。
- 机读输出（`--json`、退出码、MCP 工具）是给 AUTO-MAS 等调用方的契约：可以加字段；改了含义、删了命令或字段，要同时把 `internal/buildinfo` 里的 `Protocol` 加一。
- README 有中文（`README.md`）和英文（`README.en.md`）两份，逐节对应：改了一份，就在同一个 PR 里把另一份也改了；只会写一种语言的，在 PR 里说一声，由维护者补上。
- 其余约定（workflow 怎么写、署名与许可、AI 工具、图片类改动）同 ModAVD 的 [CONTRIBUTING.md](https://github.com/ModAVD/ModAVD/blob/main/CONTRIBUTING.md)。
- 环境、硬件、怎么验证和用 AI 工具时的模型建议，见文档站的[开发：管理器和命令行](https://modavd.github.io/develop/manager)。

## 许可证

- 代码按 GNU Affero General Public License 第 3 版或更新版本发布（AGPL-3.0-or-later），见 [LICENSE](LICENSE)。Copyright 2026 qiyinxi。
- 分发本程序（改没改都算），或者把改过的版本放到网上给别人用，都要按同样的许可证提供对应源码。别的程序只是调用 `modavd.exe`、MCP 服务或管理器的本地接口，不受影响。
- 例外见 [NOTICE](NOTICE)：
  - 程序图标（`cmd/modavd-gui/modavd.ico`、`cmd/modavd-gui/rsrc_windows_amd64.syso`、`internal/ui/web/assets/` 下的图片）是《明日方舟》艾雅法拉的同人图，角色版权归上海鹰角网络所有，不在本仓库的许可范围内；
  - `internal/avd/testdata/instances_ref.py` 复制自 AUTO-MAS（AGPL-3.0-or-later），只供测试、在单独的进程里运行，不编进程序。
- 编出的 exe 含 Go 标准库和运行时（BSD-3-Clause）。
