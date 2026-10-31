# modavd 管理器的 HTTP 接口（`internal/ui` ↔ `internal/ui/web`）

内部契约，前端（`web/`）和服务端（`*.go`）按这份写；改了两边一起改。字段名、结构和 CLI `--json` 的 `data` 相同（`internal/emu/types.go`）。

## 连接与安全

- 服务只监听 `127.0.0.1:<端口>`（默认随机端口）。一个服务只管一个根目录，根目录在启动时定。
- 启动器打开 `/?t=<令牌>`：服务回 `Set-Cookie: modavd_ui_<端口>=<令牌>; HttpOnly; SameSite=Strict; Path=/` 和 302 跳到 `/`。令牌每次启动随机生成（32 字节）。cookie 名带端口：cookie 只按主机分、不按端口分，所有管理器窗口又共用一个 Edge 配置，同名的话后开的管理器会把先开的那个顶掉。
- 所有 `/api/*` 都要带这个 cookie，没有或不对回 401。
- `Host` 不是 `127.0.0.1:<端口>` 或 `localhost:<端口>` 的请求一律 403（防 DNS 重绑定）。
- 非 GET 请求：`Origin` 必须是本服务自己的地址（浏览器会带），否则 403；请求体是 JSON（`Content-Type: application/json`），上传 APK 例外。
- 静态文件 `/`、`/app.css`、`/js/*`、`/assets/*` 不要 cookie（页面要先加载才能带 cookie），但同样校验 Host。

## 错误

HTTP 4xx / 5xx，体为 `{"error":{"code":"…","message":"…","advice":"…"}}`，`code` 是 `internal/errs` 的错误码。状态码：

| code | HTTP |
| --- | --- |
| `usage`、`invalid_argument` | 400 |
| `unauthorized`（没登录：cookie 没有或不对） | 401 |
| `forbidden`（Host 或 Origin 不是本服务） | 403 |
| `not_found` | 404 |
| `busy`、`already_exists`、`port_conflict` | 409 |
| `not_ready`、`precheck_failed`、`unsupported` | 422 |
| `canceled` | 499 |
| `launch_failed` | 502 |
| `timeout` | 504 |
| 其余 | 500 |

## 接口

| 方法 | 路径 | 请求 | 返回 |
| --- | --- | --- | --- |
| GET | `/api/info` | | `{"version","protocol","root":RootInfo}` |
| GET | `/api/instances` | | `ListResult` |
| GET | `/api/instances/{i}` | | `InstanceInfo` |
| POST | `/api/instances` | `{title?, index?, memory_mb?, cpu?, data_partition_gb?, fps?}` | 202 `Job`（`kind` 是 `create`：后台开机做完初始化再关机，要几分钟；没给 `index` 时服务端先选好编号，`Job.index` 就是新实例的；结果是 `InstanceInfo`） |
| PATCH | `/api/instances/{i}` | `{title?, memory_mb?, cpu?, fps?}` | `UpdateResult` |
| DELETE | `/api/instances/{i}?confirm=1` | 不带 `confirm=1` 回 400 `usage` | `DeleteResult` |
| POST | `/api/instances/{i}/start` | `{window?: bool}` | 202 `Job` |
| POST | `/api/instances/{i}/stop` | `{force?: bool}` | 202 `Job` |
| POST | `/api/instances/{i}/restart` | `{window?: bool, force?: bool}` | 202 `Job` |
| POST | `/api/instances/{i}/install` | `multipart/form-data`，文件字段 `apk` | 202 `Job`（服务端存到临时文件，任务结束删掉） |
| POST | `/api/instances/{i}/resume` | `{}` | 202 `Job`（`kind` 是 `create`）：重做新建失败的那台的初始化 |
| POST | `/api/instances/{i}/window` | `{visible: bool}` | `WindowResult` |
| POST | `/api/instances/{i}/input` | `{type:"tap", x, y}`（坐标是 1280×720 显示的像素）或 `{type:"key", key:"back"\|"home"\|"recents"}` | `{}` |
| GET | `/api/instances/{i}/screenshot` | | `image/png`（`adb exec-out screencap -p`），`Cache-Control: no-store`；没开机回 409 |
| GET | `/api/instances/{i}/log?kind=emulator\|logcat&lines=300` | | `text/plain; charset=utf-8`，最新那份日志的末尾；没有回 404 |
| POST | `/api/open` | `{what: "root"\|"logs"\|"instance", index?}` | `{}`，在资源管理器里打开 |
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
- 截图、输入遇到实例没开机：409，code 是 `busy`。

### Job

```json
{"id":"j12","kind":"start","index":8,"state":"running",
 "created_at":"2026-10-28T15:04:05+01:00","finished_at":"",
 "progress":{"step":"boot","message":"等安卓开机（已 18 秒）","done":2,"total":6},
 "result":null,"error":null,"warnings":[]}
```

- `kind`：`start` / `stop` / `restart` / `install` / `create`。
- `state`：`running` / `done` / `failed` / `canceled`。
- `progress.step`：开机用 emu 的 `launch` / `boot` / `tune` / `ime` / `first_boot` / `vulkan_fix` / `done`；关机（含重启的关机阶段、等被取消的开机停下）是 `stop`，安装是 `install`，这两种做完是 `done`；装执行桥的每一轮是 `bridge`；新建是 `create`、其中几次开机的上述各步（开机做完是 `setup`）、`create_stop`、`done`，几次开机合成一条不回退的进度。`total` 一律 1000。
- 安装任务的 `result.apk` 是用户上传的文件名，不是服务端的临时路径（临时文件任务结束就删了）。
- `progress` 在没有进度时为 `null`；`result` 是 `StartResult` / `StopResult` / `InstallResult` / `InstanceInfo`（新建），没完成时为 `null`；`error` 失败时是错误对象，否则 `null`。
- 同一实例同时只能有一个任务在跑；第二个回 409 `busy`。**例外**：开机任务进行中可以发关机（和 emu 一致：开机中照样能关）。
- 取消开机：只停止等待，模拟器继续开机，不会被强制结束。

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

event: ping
data: {}                    每 15 秒
```

### 更新 `UpdateView`

`update.Status` 的全部字段（和 `modavd update --json` 的 `data` 相同：`root`、`updatable`、`not_updatable_reason`、`current_version`、`current_build`、`source`、`cdk_set`、`checked`、`check_error`、`latest_version`、`latest_source`、`update_available`、`required_automas`、`release_url`、`phase`、`prepared_version`、`last_result`、`recovered`、`updated`、`message`、`notices`），加上 `checking`、`checked_at`（RFC 3339，本次启动后没查过是 `""`）、`error`（上次检查的错误对象，没有是 `null`）、`install_by_cli`（现在恒为 `true`：管理器里不能安装，由 AUTO-MAS 调 `modavd update install`）。

- 只有点「检查更新」才联网；没有自动检查。
- 更新源和 CDK 是本机这个 Windows 用户的设置（`%LOCALAPPDATA%\ModAVD\update-settings.json`），命令行更新时按它来。

## 生命周期

- `ModAVD.exe`（GUI 子系统）启动服务，再用 Edge 的应用模式打开窗口：`msedge --app=<地址> --user-data-dir=%LOCALAPPDATA%\ModAVD\ui-edge`。找不到 Edge 时用默认浏览器打开。
- 同一个根目录只开一个服务：再次双击时，把已有的窗口重新打开。
- 窗口关掉（Edge 进程退出，或者 60 秒内没有任何事件流连着）以后，等正在跑的任务做完，服务再退出。
  - Edge 进程在启动后 10 秒内就退出：当作把窗口交给了同一配置下已在运行的 Edge，不算关窗，之后按事件流规则判断。
  - 真正关窗后，5 秒内没有事件流连着就退出（有任务先等任务做完）。
  - `--dev` 不会因为空闲退出。
