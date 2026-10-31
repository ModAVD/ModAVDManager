# ModAVDManager

[ModAVD（魔改 AVD）](https://github.com/ModAVD/ModAVD) 的管理程序：图形管理器 `ModAVD.exe`，以及命令行和 MCP 服务 `modavd.exe`。Go 写，只用标准库，只支持 Windows x64。

> 这是非官方项目，与 Google 没有关联。

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

- ModAVD 是给 Windows 上的挂机脚本用的安卓模拟器（AOSP Android Emulator 加一批补丁）。本仓库是管理它的程序：新建、修改、删除实例，开机、关机，开机前的电脑检查，装 APK 和 arm64 执行桥，看画面和日志。
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
| `create [--index N] [--title T] [--memory 6G] [--cpu N] [--data-gb N] [--fps 30\|60] [--no-bridge]` | 新建。默认带 arm64 执行桥，第一次开机时装上；`--no-bridge` 记下这台不要桥，以后开机也不装 |
| `set <i> [--title T] [--memory M] [--cpu N] [--fps F] [--data-gb N]` | 修改，下次开机生效。`--data-gb` 只能改大，游戏数据不动：模拟器（0.36 起）开机时扩磁盘镜像，开机后在线扩文件系统。0.36 之前按 16 GB 及以下建的盘只能扩到下一个备份组之前（16 GB 约到 30 GB） |
| `delete <i> --yes` | 删除已关机的实例，连同游戏数据 |
| `start <i> [--window] [--timeout 300] [--no-bridge]` | 开机，等开完并做完开机后的设置；还会等待 `/sdcard` 挂好（最多 60 秒，超时只警告），实例还没有执行桥时接着装上，`--no-bridge` 这次不装 |
| `stop <i> [--force]`、`stop --all` | 关机：先让安卓正常关机，不成再 sync + kill；`--force` 是 sync + 强杀 |
| `restart <i> [--window] [--timeout 300] [--force] [--no-bridge]` | 关机再开机，选项同 `stop`、`start` |
| `bridge <i> install\|uninstall\|verify` | 装、卸、检查 arm64 执行桥；开机时会自动装，一般不用手动跑（见下） |
| `check [--memory M]` | 开机前的电脑检查 |
| `window <i> show\|hide`、`install <i> <apk>`、`shell <i> <命令>`、`adb <i> -- <参数>` | 窗口、装 APK、客体命令、adb 透传 |
| `servers`、`version`、`mcp`、`help [命令]` | |

- `<i>` 写成 `3` 或 `mas_3`（0–9，5 除外）；内存写成 `6144`、`6144M` 或 `6G`，只有 3 / 4 / 5 / 6 GB 四档，核数 2 / 4 / 6，帧率 30 / 60。
- 根目录：`--root`，没给就用环境变量 `MODAVD_ROOT`，再没有就用 `modavd.exe` 所在目录或它的上一级。

**arm64 执行桥**：

- 只对内置名单里的游戏生效，其余应用照旧走原来的转译层。桥装在实例自己的 system / vendor 覆盖层里，要 0.36 起的模拟器。
- modavd（命令行、MCP）和管理器每次开机都会看一眼：实例还没有桥，就在开机后的设置做完后装上，中间关机再冷启动一两次。只在这次是它拉起的模拟器时装；接管已经在运行的实例时不装。装不上只是警告，实例照常运行，下次开机再试。
- 不要桥的实例：新建时带 `--no-bridge`，或者之后跑 `bridge <i> uninstall`。两种都会在 `mas-avd.json` 里记下 `instances.<i>.bridge` 为 `false`，以后开机不再自动装；再 `bridge <i> install` 就恢复。`start` / `restart` 的 `--no-bridge` 只管这一次。
- `bridge` 调用 ModAVD 包里的 `tools\modavd-bridge.ps1`。`install`：需要时关机再开机，装上；已经装了的只在载荷有变时更新。`uninstall`：卸掉桥、开机标记和装桥时改成的 GL 传输方式（asg）；退回更早的版本之前要先卸。实例原来关着的，做完再关掉。`verify`：只读核对，实例要开着。
- `info` / `list` 的 `bridge` 为 `off`、`pending`、`installed` 或 `unknown`；关机时不能查询客体属性，因此没有“不装”记录且没有待装标记时显示 `unknown`。装桥期间 `busy` 为 `bridging`，删除也会持有同一把桥锁。
- 不经 modavd 或管理器开机的实例（别的程序直接拉起模拟器）不会自动装，要装就跑一次 `bridge <i> install`。

**机读输出**（`--json`）：

- stdout 只有一行：`{"ok":true,"data":…}` 或 `{"ok":false,"error":{"code","message","advice"}}`。`data` 的字段见 `internal/emu/types.go`，snake_case，所有字段一直出现。
- 进度日志写 stderr，一行一个 JSON：`{"time","level","msg"}`。调用方不要把 stderr 并进 stdout。
- `version --json` 的 `protocol` 是契约版本，字段改了含义或删了才加一。
- 运行中的实例增加 `started_at_ms`（Unix 毫秒），原来的 `started_at` 保留。`stop --all` 即使部分失败也返回 `ok:true` 和结果列表，同时以第一个失败的退出码结束。
- `create` 会无头开机完成执行桥、宿主输入法和首次初始化，再 sync 后正常关机；这段时间状态为 `creating`，失败为 `create_failed`，可用 `create --resume <i>` 重试或删除。创建命令支持 `--timeout`，CLI、MCP 和管理器活动面板都会显示进度。

**退出码**：0 成功，1 内部错误，2 用法或参数不对，3 实例不存在，4 根目录没就绪 / 检查没通过 / 不支持，5 实例忙 / 已存在 / 端口被占，6 超时，7 启动失败，8 已取消。`shell` 不带 `--json` 时是客体命令的退出码，`adb` 是 adb 的退出码；`bridge` 不带 `--json` 时，`install` 后没装上、`uninstall` 后还在都是 1，`verify` 是脚本的退出码。

**约定**（和 AUTO-MAS 一致）：

- adb 只走根目录的私有 server（`mas-avd.json` 的 `adbServerPort`，默认 20050），不碰 5037。
- 模拟器、adb server、logcat 都经一个立即退出的中间进程拉起，不在 modavd 的进程树里：调用方结束 modavd（Ctrl+C、MCP 客户端断开）不会带走模拟器。
- 开机成功后，每台实例另有一个同样不在进程树里的开机守护，随 qemu 退出而结束。客体自己重启（qemu 没退）后，它补做开机调优和 Vulkan 补丁，绝不拉起新的模拟器。
- 同一实例的开机、关机按 `<根>\avd\.mas_<i>.{start,boot,stop}.lock` 排队：开机等前一次开机和关机做完；关机可以打断正在开机的实例。

## MCP

```
modavd mcp --root D:\ModAVD
```

同时支持 MCP 2026-07-28（无状态，`server/discover`）和 2025-11-25 / 2025-06-18（`initialize` 握手）。根目录在启动时定，不是工具参数。工具：`list_instances`、`get_instance`、`check_host`、`create_instance`、`update_instance`、`delete_instance`、`start_instance`（带进度）、`stop_instance`、`set_window_visible`、`shell`、`install_apk`。

在 Claude Code 里注册（PowerShell 里 `--` 要加引号，否则会被吞掉）：

```powershell
claude mcp add --transport stdio --scope user modavd '--' D:\ModAVD\tools\modavd.exe mcp --root D:\ModAVD
```

或者项目的 `.mcp.json`：

```json
{"mcpServers": {"modavd": {"command": "D:\\ModAVD\\tools\\modavd.exe", "args": ["mcp", "--root", "D:\\ModAVD"]}}}
```

## 管理器 ModAVD.exe

双击包根的 `ModAVD.exe`：起一个只听 127.0.0.1 的本地服务，再用 Edge 的应用模式开一个窗口（`msedge --app`，Win10 / 11 自带）；找不到 Edge 时用默认浏览器。页面是内嵌在 exe 里的 HTML / CSS / JS，不联网、不用构建工具。

- 功能：实例卡片（在线时有画面缩略图）、开关机（带进度）、新建、设置、删除、电脑检查、活动面板、画面查看（点击即点按，返回 / 主页 / 最近任务）、日志、安装 APK、打开目录；浅色 / 深色 / 跟随系统。
- 安全：每次启动随机令牌，换成 HttpOnly cookie（名字带端口，两个根目录的管理器同时开不会互相顶掉）；校验 Host（防 DNS 重绑定）和 Origin；严格的 CSP。
- 一个根目录只开一个服务，再双击只是把窗口重新打开；状态文件在 `%LOCALAPPDATA%\ModAVD\`，日志在 `<根>\logs\modavd-ui.log`。
- 关掉窗口后，等正在跑的任务做完再退出。
- 选项：`--root`、`--port`、`--no-window`（只起服务）、`--dev`（放出 `/dev/` 下的假后端，给页面调样式用）。
- 前后端接口：`internal/ui/API.md`。页面在 `internal/ui/web/`，图标是同人图（见 `internal/ui/web/assets/README.md` 和 [NOTICE](NOTICE)），不随代码许可授权。

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
| `MODAVD_TEST_TMP` | 测试的临时文件放在这个目录下，每个包一个子目录（`adb`、`win`、`emu-ops`）；不设就用 Go 默认的临时目录（`%TEMP%` 下）。想让测试数据不落 C 盘时设它。 |
| `MODAVD_TEST_PYTHON` | 对照用的 Python 3；不设就用 PATH 上的 `python`，找不到就跳过那几个对照测试。 |
| `MODAVD_TEST_IME_APK_DIR` | ModAVD 输入法 APK（`modavd-ime.apk`、`modavd-ime-testapp.apk`）所在的目录；不设就跳过读这两个 APK 的测试。 |
| `MODAVD_TEST_LIVE` | 设成 `1` 才跑碰真实环境的测试：看本机正在运行的模拟器、起真的 adb server。会碰到本机正在用的实例，平时不要设。 |

- `internal/avd`、`internal/root`、`internal/jsonord` 有一部分测试用 Python 3 运行 AUTO-MAS 的原始逻辑（如 `internal/avd/testdata/instances_ref.py`），和 Go 的输出逐字节对照，所以要装 Python 3。
- 有几项测试只读本机 `D:\ModAVD` 下的现成文件（`mas-avd.json`、实例配置、组件 APK），没有就跳过；`MODAVD_TEST_METADATA` 可以另指一份 `mas-avd.json`。
- CI（`.github/workflows/ci.yml`）在 `windows-2025` 上跑格式检查、`go vet`、`go test`、`build.ps1`、隐私扫描和作者检查；只在 PR 或手动触发时运行。

## 独立下载与哈希

v0.1.0 是第一个独立版本。压缩包名为 `ModAVDManager-0.1.0-win-x64.zip`，内容只有两个程序、许可证和清单；压缩包不带顶层目录。下载后核对 `release/0.1.0.SHA256SUMS.txt` 中的两个 exe 与 zip 的 SHA-256。v0.1.0 没有自更新，单独下载的文件需要手动替换；从 v0.2.0 起才提供自更新。

自己编译需要 Go 1.26.5、`GOTOOLCHAIN=local`，运行 `build.ps1`。单独使用时必须指定 `--root` 或 `MODAVD_ROOT`，否则程序会提示找不到 ModAVD 根目录。

## 参与

- 只有一个维护者，回复可能要几天。改代码前先开 issue，写清要改什么、为什么；没商量过的大 PR 可能不合。
- 提交署你自己的名字或化名，邮箱用你自己的 GitHub noreply 地址（`<编号>+<登录名>@users.noreply.github.com`，在 GitHub 的邮箱设置里能看到）。
- 机读输出（`--json`、退出码、MCP 工具）是给 AUTO-MAS 等调用方的契约：可以加字段；改了含义或删了字段，要同时把 `internal/buildinfo` 里的 `Protocol` 加一。
- 其余约定（workflow 怎么写、署名与许可、AI 工具、图片类改动）同 ModAVD 的 [CONTRIBUTING.md](https://github.com/ModAVD/ModAVD/blob/main/CONTRIBUTING.md)。

## 许可证

- 代码按 GNU Affero General Public License 第 3 版或更新版本发布（AGPL-3.0-or-later），见 [LICENSE](LICENSE)，和 AUTO-MAS 相同。Copyright 2026 qiyinxi。
- 分发本程序（改没改都算），或者把改过的版本放到网上给别人用，都要按同样的许可证提供对应源码。别的程序只是调用 `modavd.exe`、MCP 服务或管理器的本地接口，不受影响。
- 例外见 [NOTICE](NOTICE)：
  - 程序图标（`cmd/modavd-gui/modavd.ico`、`cmd/modavd-gui/rsrc_windows_amd64.syso`、`internal/ui/web/assets/` 下的图片）是《明日方舟》艾雅法拉的同人图，角色版权归上海鹰角网络所有，不在本仓库的许可范围内；
  - `internal/avd/testdata/instances_ref.py` 复制自 AUTO-MAS（AGPL-3.0-or-later），只供测试、在单独的进程里运行，不编进程序。
- 编出的 exe 含 Go 标准库和运行时（BSD-3-Clause）。



