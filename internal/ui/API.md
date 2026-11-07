# modavd 管理器的 HTTP 接口（`internal/ui` ↔ `internal/ui/web`）

内部契约，前端（`web/`）和服务端（`*.go`）按这份写；改了两边一起改。字段名、结构和 CLI `--json` 的 `data` 相同（`internal/emu/types.go`）。

## 连接与安全

- 服务只监听 `127.0.0.1:<端口>`（默认随机端口）。一个服务只管一个根目录，根目录在启动时定。
- 启动器打开 `/?t=<令牌>`：服务回 `Set-Cookie: modavd_ui_<端口>=<令牌>; HttpOnly; SameSite=Strict; Path=/` 和 302 跳到 `/`。令牌每次启动随机生成（32 字节）。cookie 名带端口：cookie 只按主机分、不按端口分，所有管理器窗口又共用一个 Edge 配置，同名的话后开的管理器会把先开的那个顶掉。
- 所有 `/api/*` 都要带这个 cookie，没有或不对回 401。
- `Host` 不是 `127.0.0.1:<端口>` 或 `localhost:<端口>` 的请求一律 403（防 DNS 重绑定）。
- 非 GET 请求：`Origin` 必须是本服务自己的地址（浏览器会带），否则 403；请求体是 JSON（`Content-Type: application/json`），上传 APK 和导入 Google Play 商店的文件例外。
- 静态文件 `/`、`/app.css`、`/js/*`、`/assets/*` 不要 cookie（页面要先加载才能带 cookie），但同样校验 Host。

## 错误

HTTP 4xx / 5xx，体为 `{"error":{"code":"…","message":"…","advice":"…","details":{…}}}`，`code` 是 `internal/errs` 的错误码；`details` 只在有额外的机读信息时出现（现在只有开机失败时客体现场的目录 `boot_diag_dir`，见 README）。任务（jobs）的 `error` 也一样。状态码：

| code | HTTP |
| --- | --- |
| `usage`、`invalid_argument` | 400 |
| `unauthorized`（没登录：cookie 没有或不对） | 401 |
| `forbidden`（Host 或 Origin 不是本服务） | 403 |
| `not_found` | 404 |
| `busy`、`already_exists`、`port_conflict` | 409 |
| `not_ready`、`precheck_failed`、`unsupported` | 422 |
| `canceled` | 499 |
| `launch_failed`、`update_unavailable`、`update_rejected`、`proxy_unavailable` | 502 |
| `timeout` | 504 |
| 其余 | 500 |

## 接口

| 方法 | 路径 | 请求 | 返回 |
| --- | --- | --- | --- |
| GET | `/api/info` | | `{"version","protocol","root":RootInfo}` |
| GET | `/api/instances` | | `ListResult` |
| GET | `/api/instances/{i}` | | `InstanceInfo` |
| POST | `/api/instances` | `{title?, index?, memory_mb?, cpu?, data_partition_gb?, fps?, resolution?}` | 202 `Job`（`kind` 是 `create`：后台开机做完初始化再关机，要几分钟；没给 `index` 时服务端先选好编号，`Job.index` 就是新实例的；结果是 `InstanceInfo`）。新建的实例不带 Google Play 商店（没有 `play_store` 字段，给了回 400），要的话建好后用下面的 `playstore-install` |
| PATCH | `/api/instances/{i}` | `{title?, memory_mb?, cpu?, fps?, resolution?, data_partition_gb?, diagnostics?: bool}` | `UpdateResult`；诊断立即生效，普通配置下次开机生效 |
| GET | `/api/instances/{i}/diagnostics/preview` | | `{"index","files","bytes"}`：关闭诊断时预计清理的份数、字节，不改配置、不删文件 |
| DELETE | `/api/instances/{i}?confirm=1` | 不带 `confirm=1` 回 400 `usage` | `DeleteResult` |
| POST | `/api/instances/{i}/start` | `{}` | 202 `Job`：一律带窗口开机（管理器不开无头的，无头开机只走命令行）；实例已经以无头方式在运行时，任务失败，code 是 `busy`（运行中不切换） |
| POST | `/api/instances/{i}/stop` | `{force?: bool, cancel_create?: bool}` | 202 `Job`；新建中默认 busy，显式 `cancel_create: true` 才取消；`force` 只管是否跳过 Android 正常关机。被取消的新建在结果里标记 `interrupted_create: true` |
| POST | `/api/instances/{i}/install` | `multipart/form-data`，文件字段 `apk` | 202 `Job`（服务端存到临时文件，任务结束删掉） |
| POST | `/api/instances/{i}/disk-info` | `{}` | 202 `Job`（`kind` 是 `disk-info`）：数据盘的占用，随时可以查（开机时 `allocated_bytes`、`reclaimable_bytes` 不返回，`needs_shutdown` 为 `true`）；结果是 emu 的 `DiskInfoResult`。整理中或锁不可用时返回可读取的文件信息及 `warnings`，不运行 qemu-img；读不到宿主占用时省略 `on_disk_bytes` |
| POST | `/api/instances/{i}/disk-trim` | `{}` | 202 `Job`（`kind` 是 `disk-trim`）：请求开着的安卓回收空间；`done` 和 100% 仅表示触发请求已返回，不等待或确认客体 FITRIM 完成，任务耗时也是请求耗时；实例没开机完成、或模拟器版本不支持时任务失败，code 是 `unsupported`；结果是 `DiskTrimResult` |
| POST | `/api/instances/{i}/disk-compact` | `{}` | 202 `Job`（`kind` 是 `disk-compact`）：整理已关机实例的数据盘文件；预估收益不足时跳过重写；实例没关机时任务失败，code 是 `busy`；结果是 `DiskCompactResult` |
| POST | `/api/instances/{i}/resume` | `{}` | 202 `Job`（`kind` 是 `create`）：重做新建失败的那台的初始化 |
| POST | `/api/instances/{i}/playstore-install` | `{}` | 202 `Job`（`kind` 是 `playstore-install`）：把根目录里的五个文件装进这台实例（同 `modavd playstore <i> install`：放进执行桥的可写覆盖层、藏起占位应用、冷启动一次；关着的做完再关上，开着的照原来的方式开回来，要几分钟）；结果是 `PlayStoreResult`。开始前先检查，不行立即回错误、不开任务：根目录还没有五个文件（或文件不全、损坏）是 422 `not_ready`，`details.reason` 为 `playstore_files_missing`（页面引导去「补全组件」勾选或导入）；实例没有执行桥是 422 `not_ready`，`details.reason` 为 `no_bridge`；arm64 客体是 422 `unsupported`（`unsupported_guest`）；新建中 409 `busy`；不存在 404。任务里客体脚本发现系统不可写也是 `not_ready` / `no_bridge`。任务中途失败时客体脚本撤回这次放进去的文件；撤不干净（或脚本没说完就断了）的，实例的 `play_store_partial` 变成 `true`，页面同时给「补全」和「移除」。实例原来关着的，失败后也关上 |
| POST | `/api/instances/{i}/playstore-uninstall` | `{}` | 202 `Job`（`kind` 是 `playstore-uninstall`）：从实例里移除商店（去掉 Google Play 装的更新、删掉五个文件、冷启动后恢复占位应用；同 `playstore <i> uninstall`）；结果是 `PlayStoreResult`。不要五个文件，也不检查执行桥；只装了一部分的实例也用它移除 |
| GET | `/api/instances/{i}/playstore-gsfid` | | `{"index","decimal","hex"}`：开着的实例的 GSF ID（同 `modavd playstore <i> gsfid --json`；十进制是 GSF 记的，十六进制是同一个数，负数按补码），几秒钟。用来到 https://www.google.com/android/uncertified/ 登记。没开机（按最近的列表）或实例上有任务在跑是 409 `busy`；还没有 ID 是 422 `not_ready`，`details.reason` 为 `no_gsf_id`（开着网等一两分钟再试） |
| POST | `/api/instances/{i}/input` | `{type:"tap", x, y}`（坐标是这台实例显示的像素（1280×720 或 1920×1080，竖屏时宽高对调））或 `{type:"key", key:"back"\|"home"\|"recents"}` | `{}` |
| GET | `/api/instances/{i}/screenshot` | | `image/png`（`adb exec-out screencap -p`），`Cache-Control: no-store`；没开机回 409 |
| GET | `/api/instances/{i}/log?kind=emulator\|logcat&lines=300` | | `text/plain; charset=utf-8`，最新那份日志的末尾；没有回 404 |
| GET | `/api/instances/{i}/proxy` | | `ProxyView`，见下 |
| PUT | `/api/instances/{i}/proxy` | `{enabled?: bool, server?: string, apps?: [string]}` | `ProxyView`：给出的项换成新值，没给的不变；一项都没给回 400 `usage`。`server` 可以不写协议（探测出来）或写 `auto`（取这台电脑的代理设置）；第一次打开没给地址也按 `auto`。地址探测不到回 502 `proxy_unavailable`。实例开着就马上生效，几秒内回 |
| GET | `/api/instances/{i}/packages` | | `{"packages":[{"package","name","uid","system"}]}`：实例里装的第三方应用，加上装了的话「Google Play 服务」（`system: true`）；`name` 是认得的游戏的中文名和「Google Play 服务」，其余是 `""`。认得的游戏在前，然后按包名排。没开机回 409 `busy` |
| GET | `/api/proxy/suggest` | | `{"server","notes"}`：这台电脑的代理设置（同 `modavd proxy --server auto`），换成实例里看到的地址，设置没说是 SOCKS5 时探测协议；`notes` 说取自哪里。没设代理回 502 `proxy_unavailable` |
| POST | `/api/proxy/probe` | `{server}` | `{"server","host_addr","kind","udp","udp_error","notes"}`：在这台电脑上探测这个地址：是 SOCKS5 还是 HTTP 代理，SOCKS5 再经它发一个 DNS 查询试 UDP。`server` 换成实例里看到的地址（`127.0.0.1`、`localhost` 记成 `10.0.2.2`），协议换成探测到的，`notes` 说改了什么。连不上回 502 `proxy_unavailable` |
| POST | `/api/open` | `{what: "root"\|"logs"\|"instance"\|"collect-info", index?, job?}` | `{}`，在资源管理器里打开；`collect-info` 打开那个做完了的收集问题信息任务（`job` 是任务 id）生成的问题包所在的文件夹，不接受页面给的路径 |
| GET | `/api/components` | | `{"root","components":[…],"play_store":PlayStoreStatus}`：组件状态；`components` 最后一项是可选的 Google Play 商店的概要（`id` 为 `play-store`，`optional: true`，五个文件都在就是 `installed`），`play_store` 是它的细节，见下 |
| GET | `/api/components/license` | `?source=auto\|googledownloads_cn\|tencent\|google\|custom&mirror_urls=…`（可重复；不带时用保存的下载源） | `{"license":"全文","license_source":"镜像来源","license_sha256":"全文的 SHA-256"}`：从这个下载源取的许可协议，和补全时取的是同一份；不下载组件 |
| GET | `/api/components/source` | | 当前组件下载源：`source` 为 `auto`、内置镜像 ID 或 `custom`，以及 `mirror_urls`、`configured` |
| PUT | `/api/components/source` | `{"source":"auto"\|"googledownloads_cn"\|"tencent"\|"google"\|"custom", "mirror_urls":[...]}` | 保存当前用户的组件下载源；自定义地址只接受 HTTP(S) 目录 |
| POST | `/api/components/fetch` | `{"accept_license":true, "license_sha256":"…", "source"?:..., "mirror_urls"?:[...]}`（`accept_license` 必须明确为 `true`；`license_sha256` 是看过的那份许可协议的，取自上面的接口，同一个下载源） | 202 `Job`（`kind` 是 `components`，根任务，`index` 为 `-1`）。不带 `source` / `mirror_urls` 时用保存的下载源。后台取到的许可协议和 `license_sha256` 对不上时，任务以 `components_rejected` 失败，什么也不下。`"play_store": true` 再补全可选的 Google Play 商店：根目录还没有五个文件时下 Google Play 版系统镜像（约 1.5 GB），取出五个文件后压缩包和解出的 `system.img` 都删掉，只留这五个；已经有了（下载的或导入的）就跳过 |
| POST | `/api/components/import-playstore` | `multipart/form-data`，文件字段 `file`，每个文件一个（1–8 个）：Google 官方的 Google Play 版镜像压缩包 `x86_64-34_r14.zip`，或者那五个文件本身（文件名都不限） | 202 `Job`（`kind` 是 `playstore-import`，根任务，`index` 为 `-1`）：「使用我自己的文件」。不下载、不问许可协议。服务端把上传存到临时文件夹，任务结束删掉；任务按大小认出每个文件，压缩包核对 Google 仓库清单的大小和 SHA-1，五个文件各自核对写死的大小和 SHA-256；结果是 `PlayStoreImportResult`。上传本身不对时立即回 400：不是 multipart 是 `usage`，没有 `file` 字段是 `usage`，空文件、超过 8 个、单个比压缩包还大是 `invalid_argument`。文件被拒收时任务以 `components_rejected` 失败，什么也不改，`details.reason`：`unknown_file`（大小既不是压缩包也不是五个文件之一）、`mismatch`（大小对上了、哈希对不上）、`incomplete`（只给了一部分，根目录里又没有其余的）；另一个补全或导入在跑是 409 `busy`；空间不够 422 `precheck_failed` |
| POST | `/api/diag/collect` | `{out_dir?, stage_dir?, keep_stage?}` | 202 `Job`（`kind` 是 `collect-info`，根任务，`index` 为 `-1`） |
| GET | `/api/diag/report-config` | | `{"text":"…"}` |
| GET | `/api/check?refresh=1` | | `CheckResult`（要几秒：硬件加速检查和 Vulkan 探测） |
| GET | `/api/jobs` | | `{"jobs":[Job]}`，新的在前，最多 50 个 |
| GET | `/api/jobs/{id}` | | `Job` |
| POST | `/api/jobs/{id}/cancel` | | `Job` |
| GET | `/api/events` | | `text/event-stream`，见下 |
| GET | `/api/update` | | `UpdateView`，见下（不联网） |
| POST | `/api/update/check` | `{}` | `UpdateView`：按设置的更新源查最新版本（不带 CDK）；检查进行中回 409 `busy` |
| POST | `/api/update/settings` | `{source?: "github"\|"mirrorchyan", cdk?: string}` | `UpdateView`；`cdk` 为 `""` 时清除，CDK 加密保存，从不返回 |

`{i}` 是实例编号（0–9，5 除外）。

- JSON 请求体：不认识的字段回 400 `usage`（拼错的字段名页面能直接看到）；空请求体等于全用默认值；PATCH 一个字段都没给回 400 `usage`。
- 截图、输入、列应用、取 GSF ID 遇到实例没开机：409，code 是 `busy`。
- `InstanceInfo` 带 `diagnostics: bool`，开启时带 `diagnostics_since`。PATCH 修改诊断时，`UpdateResult` 带 `diagnostics_cleanup: {files, bytes}`，只报实际删除量。关闭预览包含重新加入普通全根目录 1 GiB 池后需淘汰的其它普通实例旧日志；活跃文件不在预览和删除范围内。预览和提交之间现场可能变化，页面以提交结果为准。关闭部分失败时开关已保存为关，再次提交 `diagnostics: false` 会继续清理和客体恢复。
- 诊断设置成功但宿主 logcat 由外部程序采集时，`UpdateResult.warnings` 说明未接管；不结束外部进程，也不等待它切换后才返回。前端显示该提示。

### Job

```json
{"id":"j12","kind":"start","index":8,"state":"running",
 "created_at":"2026-10-28T15:04:05+01:00","finished_at":"",
 "progress":{"step":"boot","message":"等安卓开机（已 18 秒）","done":2,"total":6},
 "result":null,"error":null,"warnings":[]}
```

- `kind`：`start` / `stop` / `install` / `create` / `components` / `collect-info` / `disk-info` / `disk-trim` / `disk-compact` / `playstore-install` / `playstore-uninstall` / `playstore-import`（0.3.0 起没有重启，也没有显示 / 隐藏窗口）。
- `state`：`running` / `done` / `failed` / `canceled`。
- `progress.step`：开机用 emu 的 `launch` / `boot` / `tune` / `ime` / `first_boot` / `vulkan_fix` / `done`；关机（含等被取消的开机停下）是 `stop`，安装是 `install`，这两种做完是 `done`；磁盘任务进行中是 `disk_info` / `disk_trim` / `disk_compact`、做完是 `done`（进行中没有百分比，`total` 为 0，页面显示不定长进度条；做完 `done` = `total` = 1000）；装执行桥的每一轮是 `bridge`；商店的装、卸进行中是 `playstore-install` / `playstore-uninstall`（没有百分比）、做完是 `done`；导入时核对文件是 `verifying`、解压镜像是 `extracting`（`done` / `total` 是字节）；新建是 `create`、其中几次开机的上述各步（开机做完是 `setup`）、`create_stop`、`done`，几次开机合成一条不回退的进度。`total` 一律 1000。
- 安装任务的 `result.apk` 是用户上传的文件名，不是服务端的临时路径（临时文件任务结束就删了）。
- `progress` 在没有进度时为 `null`；`result` 是 `StartResult` / `StopResult` / `InstallResult` / `DiskInfoResult` / `DiskTrimResult` / `DiskCompactResult` / `InstanceInfo`（新建）/ `PlayStoreResult` / `PlayStoreImportResult`，没完成时为 `null`；`error` 失败时是错误对象，否则 `null`。

`DiskCompactResult` 保留非负的 `freed_bytes`。`before_on_disk_bytes` 和 `after_on_disk_bytes` 是整理前后宿主实际占用，新增的 `on_disk_delta_bytes` 等于后者减前者，正值表示占用增大。`skipped=true` 表示检查通过、预计收益不足，未重写原盘；此时前后占用相等、变化和释放量均为零。结果还包含 `reclaim_threshold_bytes` 和可省略的 `estimated_reclaimable_bytes`，详见 [整理预算与结果契约](../emu/COMPACT.md)。
- 同一实例同时只能有一个任务在跑；第二个回 409 `busy`。**例外**：开机任务进行中可以发关机（和 emu 一致：开机中照样能关）。
- 取消开机：只停止等待，模拟器继续开机，不会被强制结束。
- `components`、`playstore-import` 和 `collect-info` 是根目录任务，`index` 为 `-1`，同时只跑一个；取消会传给诊断后端上下文。`components`、`playstore-import` 在跑时，开机、新建、商店的装卸回 409 `busy`，反过来也一样（它们会改 `sdk\`）。

### 事件流 `/api/events`

```
event: instances
data: <ListResult>          连上时立刻发一次，之后每 3 秒一次，任务每走一步也发一次

event: job
data: <Job>                 任务有任何变化时

event: log
data: {"time":"…","level":"info|warn|error","msg":"…","job_id":"j12"}   不属于任务的消息 job_id 是 ""

event: update
data: <UpdateView>          检查、设置有变化时

event: proxy
data: {"marks":[ProxyMark]} 按应用走代理的卡片标记，全量；连上时发一次（有过的话），之后有变化时

event: ping
data: {}                    每 15 秒
```

### 按应用走代理 `ProxyView`、`ProxyMark`

`ProxyView` 是 `AppProxyResult` 的全部字段（和 `modavd proxy <i> --json` 的 `data` 相同：`index`、`enabled`、`server`、`apps`、`dns`、`applied`、`notes`、`guest`、`guest_error`、`error`；`guest_error` 是实例开着但问不到客体时的原因），加上 `mark`：实例开着、开完了机、没在开机或关机、设置开着且名单不空时是一个 `ProxyMark`，否则是 `null`。

- `applied`：GET 是 `""`；PUT 是 `now`（客体里已生效）、`failed`（客体没接受，`error` 说为什么）、`booting`（正在开机，开完马上生效）、`next_boot`（没开机）、`off`（关了，客体里不留它的程序和规则）。
- `ProxyMark` 是 `{"index","tone","text"}`：
  - `ok`：客体里的程序在跑、规则就位，代理通着。
  - `warn`：规则缺了几条、正在补；或者客体最近一次经代理的连接没成功，但这台电脑上能连到代理（可能是目标本身没回应）；或者这次没问到客体（adb、su 没回应）。
  - `err`：名单里的应用现在断网或直连：客体里没有它在跑、规则挂不上，或者代理连不上（客体最近一次连接失败，这台电脑上也连不上）。
  - `text` 是给人看的一句话。
- 卡片的标记：页面连着时，服务每 30 秒问一次每个开着这个设置、已开机的实例（一条 root 命令），实例的状态或设置一变就马上问；正在开机、关机或做开机后设置的不问。GET、PUT 的结果也会更新标记。
- 没开机的实例没有 `ProxyMark`：页面按 `InstanceInfo.app_proxy`（只读配置的概要）显示「下次开机生效」。

### Google Play 商店 `PlayStoreStatus`、`PlayStoreImportResult`、`PlayStoreResult`

Apple Silicon macOS 的组件下载/导入使用 API35 Google Play ARM64 r9，归档 `arm64-v8a-35_r09.zip`；实例内装卸仍返回 `unsupported`。Windows 保持 API34 x86_64 r14。`PlayStoreStatus` 增加 `archive`（可导入的官方归档名）、`download_bytes`（归档大小）和 `install_supported`（当前宿主是否已支持实例装卸）；界面按这些字段展示大小、导入提示和能力边界。macOS 的 `ready` 同时检查 `source.properties` 的 ABI、API 和镜像类型，仍不替代导入/下载时的摘要校验。

商店是可选的：根目录里留五个文件（`sdk\playstore\`，补全组件时勾选下载，或者导入自己的文件），每台实例要的时候再装进去；新建实例不带。

- `PlayStoreStatus`（`GET /api/components` 的 `play_store`，和 `modavd components status --json` 的 `data.play_store` 相同）：`{"dir","ready","source","version","archive","download_bytes","install_supported","files":[{"name","guest","size","present"}]}`。
  - `ready`：五个文件都在、`source.properties` 的版本对（只看大小，SHA-256 在放进来和装进实例时核对）。实例的「补全 Google Play 商店」按钮按它决定是直接装，还是先引导去补全组件。
  - `source`：`download`（补全组件时下载的）、`import`（用自己的文件导入的），没有或来历不明是 `""`。
  - `files` 固定五项，顺序同文档站：`Phonesky.apk`、`PrebuiltGmsCore.apk`、`GoogleServicesFramework.apk`、`PartnerSetupPrebuilt.apk`、`play_store_fsi_cert.der`；`guest` 是装进实例后的路径，`present` 是这个文件在、大小对。
- `PlayStoreImportResult`（`playstore-import` 任务的结果，同 `modavd components import-playstore --json`）：`{"root","from","used","kept","skipped","set_aside","status":PlayStoreStatus}`。
  - `from`：`zip`（给了压缩包）或 `files`（给了文件本身）；`used`：核对通过、用上了的文件（按上传时的文件名）；`kept`：这次没给、根目录里原有且核对无误留下的。
  - `skipped`：根目录里五个文件本来就齐全，给的文件也核对过了，什么都没改。
  - `set_aside`：原来不完整的 `sdk\playstore` 改名后的位置（`sdk\playstore.incomplete-<时间>`），没有是 `""`。
- `PlayStoreResult`（`playstore-install` / `playstore-uninstall` 任务的结果，同 `modavd playstore <i> … --json`）：`{"index","action","state","installed","files","restarted","stopped","warnings"}`；`state` 是做完后客体里的情况：`installed`、`absent` 或 `partial`（还有没放到位的 `.new` 也算 `partial`）。
- `InstanceInfo.play_store`（bool）：这台实例装着商店（按 `mas-avd.json` 的记录，装成功后为 `true`，移除后 `false`；不问客体）。按钮据此显示「补全 Google Play 商店」或「移除 Google Play 商店」，装着时再加一项「复制 GSF ID」（开机后可用）；`bridge` 为 `off` 的实例装不了（会回 `no_bridge`）。
- `InstanceInfo.play_store_partial`（bool）：上次补全中途失败、没能全部撤回（`mas-avd.json` 记的是 `"partial"`）。这时 `play_store` 为 `false`，页面「补全」和「移除」都给；`bridge <i> uninstall` 照样拒绝。
- 页面选文件和装 APK 一样：`<input type="file">`（可多选），每个文件追加一个 `file` 字段，用 `FormData` 发 `multipart/form-data`。

### 更新 `UpdateView`

`update.Status` 的全部字段（和 `modavd update --json` 的 `data` 相同：`root`、`updatable`、`not_updatable_reason`、`current_version`、`current_build`、`source`、`cdk_set`、`checked`、`check_error`、`latest_version`、`latest_source`、`update_available`、`required_automas`、`release_url`、`phase`、`prepared_version`、`last_result`、`recovered`、`updated`、`message`、`notices`），加上 `checking`、`checked_at`（RFC 3339，本次启动后没查过是 `""`）、`error`（上次检查的错误对象，没有是 `null`）、`install_by_cli`（现在恒为 `true`：管理器里不能安装，由 AUTO-MAS 调 `modavd update install`）。

- 只有点「检查更新」才联网；没有自动检查。
- 更新源和 CDK 是本机这个 Windows 用户的设置（`%LOCALAPPDATA%\ModAVD\update-settings.json`），命令行更新时按它来。

## 生命周期

- `ModAVD.exe`（GUI 子系统）启动服务，再用 Edge 的应用模式打开窗口：`msedge --app=<地址> --user-data-dir=%LOCALAPPDATA%\ModAVD\ui-edge2 --disable-features=msImplicitSignin,msEdgeStartupBoost --disable-sync --disable-extensions`。找不到 Edge 时用默认浏览器打开。
  - 管理器自己的配置不登录、不同步、不加载扩展：不然 Edge 会在新配置上自动用 Windows 账户登录，同步装上用户的扩展，扩展的欢迎页再弹出一个完整的浏览器窗口。
  - 关掉窗口时不起「启动增强」：不然 Edge 会另起一个 `msedge --no-startup-window`，它不带 `--user-data-dir`，用的是用户自己的配置。
  - 页面声明了不用翻译（`translate="no"`），Edge 的界面语言不是中文时也不会弹「翻译此页」。
  - 0.3.0 及以前用的是 `ui-edge`，没带这些开关，可能已经登着用户的账户、同步进了收藏夹、历史、密码，所以换了新目录。
  - 新版打开窗口以后删掉旧的 `ui-edge`；还有 Edge 在用它（开着旧版管理器）时跳过，下次打开再删。删不掉只记日志，不影响打开。
- 同一个根目录只开一个服务：再次双击时，把已有的窗口重新打开。
- 窗口关掉（Edge 进程退出，或者 60 秒内没有任何事件流连着）以后，等正在跑的任务做完，服务再退出。
  - Edge 进程在启动后 10 秒内就退出：当作把窗口交给了同一配置下已在运行的 Edge，不算关窗，之后按事件流规则判断。
  - 真正关窗后，5 秒内没有事件流连着就退出（有任务先等任务做完）。
  - `--dev` 不会因为空闲退出。

`resolution` 只接受 1080 和 720。新建默认 1080，分别联动 1920×1080 / DPI 280 与 1280×720 / DPI 240；修改保留方向，下次开机生效。`InstanceInfo.config.resolution` 是按当前配置宽高识别的档位，运行中修改后表示下次开机的设置。

画面点击按当前截图的尺寸换算并校验。运行中改档时，`config` 表示下次开机设置，当前画面和点击坐标在重启前保持原尺寸。
