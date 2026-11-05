# trim 时机测量

本次只处理 ModAVD/ModAVD#95 的“trim 真正跑完的时机”。不改变 trim 的异步契约，
不涉及 compact、其它磁盘子项或 AOSP 构建。

## 返回和日志分别表示什么

| 入口或信号 | 含义 |
| --- | --- |
| `modavd disk <i> trim` 成功及 `DiskTrimResult` | 管理器确认实例已开机、`vdc` 宣告支持 discard，随后 adb/su 中的 `sm idle-maint run` 成功返回。未等待或确认客体 FITRIM 成功。CLI 保留“已触发磁盘回收” |
| `diskTrimTimeout` 的 2 分钟、`autoTrimTimeout` 的 30 秒 | 请求往返的超时，不限制后台维护的执行时间 |
| 开机后自动 trim 日志里的秒数 | `runIdleMaint` 调用前后的宿主时间差，即 adb/su 请求往返，不是客体回收耗时。`diskTrimAt` 仍是上次自动尝试的时间 |
| GUI `disk-trim` 的 `done`、100%、`finished_at` | 宿主请求任务返回。活动中的时间差还包含管理器检查与任务开销。界面显示“已触发”和“触发请求用时” |
| MCP `trim_disk` 成功 | 同一个 `DiskTrim` 返回，没有额外客体完成检查。`discard_max_bytes` 是能力声明，不是本轮回收字节数 |
| vold `Starting trim of /data` | vold 开始这一轮 `/data` trim 尝试，并不证明已经成功提交 discard |
| vold `Trimmed … bytes on /data in …ms` | 对应 `/data` FITRIM 的明确成功返回信号。脚本要求窗口内只有一次开始，且开始、返回的进程和线程相同；成功分支当前只有合成日志单测，未在本次正式包上观测到 |
| vold `Trim failed on /data: …` | 该次 `/data` trim 失败。不能因为 `sm` 退出 0 就忽略 |
| vold `idle maintenance completed` | 维护线程结束，可能包含其它挂载点及失败的 `/data` trim。本次实机正是这种情况 |
| `/proc/diskstats` discard 计数 | 内核设备累计的 discard 完成请求数、合并数、512 字节扇区数及毫秒数。计数变化是旁证，暂时稳定不能证明本轮结束 |

调用链是 `internal/cli/commands.go`、`internal/ui/disk.go` 或 `internal/mcp/tools.go`
→ `internal/emu/disk.go:DiskTrim` → `runIdleMaint` → `Manager.su` → 私有 adb server。
自动触发来自 `internal/emu/autotrim.go`。管理器的这些路径都没有读取客体完成信号。
本次对 Android 内部行为的判断来自正式包客体日志，不依赖拉取 AOSP 源码。

FITRIM 返回也不等于宿主 qcow2 文件长度已缩小，日志中的字节数不能当作宿主释放字节数。
没有匹配的明确返回、日志缺失或出现重叠轮次时，脚本输出 `unknown`，不根据计数或等待时长补判成功。

## 复跑

需要 Windows、Python 3.10+、已补全组件的现有正式包，以及从待测源码构建的管理器。
不需要下载或同步 AOSP。先把正式包及 SDK **物理复制**到一个新的专用根，
不要复制已有 `avd`、实例配置或使用共享数据盘。为新根写入独立配置，例如：

```json
{"portBase":27400,"adbServerPort":27498,"scriptAdbServerPort":27499,"instances":{}}
```

先确认整段端口空闲，而且没有其它验收正在使用该环境。只有一套可用环境时排队。
以下命令中的端口只是本次选择，复跑时需重新确认。

```powershell
$root = 'C:\Temp\modavd-trim-test'
$manager = (Resolve-Path '.\out\tools\modavd.exe').Path
& $manager --root $root create --index 0 --memory 4096 --cpu 4 --data-gb 16 --no-bridge
```

新建完成会正常关机。为排除开机后的自动请求，在**该专用根**的 `mas-avd.json` 中
将 `instances.0.diskTrimAt` 设为当前 Unix 秒，再开机，不改其它实例：

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

每次输出使用新文件名。`--write-mib` 默认为 0，最多 256 MiB；非零时，脚本只在
`/data/local/tmp/modavd-trim-measure-<随机值>` 新建目录，以 `/dev/urandom` 实际写入文件，
fsync、sync、记录 `du` 后删掉文件并再次 sync。最后只清理这个文件和空目录。
脚本不装应用、不整理磁盘、不换包、不关机。

`--exclusive-test-instance` 明确声明本实例由本次测试独占。根内的排他文件只防止两份测量脚本重叠，
不能阻止其它管理器或 adb 使用者。采样期间不要让其它会话对本实例 trim、compact、安装、换包或关机。
每轮完成后核对结果；若缺少明确结束信号，先查明是否仍有维护在执行，再触发下一轮。

脚本从 mountinfo 的 `/data` 根挂载读取 major:minor，递归读取 sysfs `slaves`，
保存每层设备名称、discard 上限并按设备号筛选 diskstats，不假定 `/data` 一定直接挂在 `vdc`。
每轮保存 boot ID、内核和二进制 SHA-256、vold 单调时间日志、dmesg 前后快照，
以及默认约 10 秒的 diskstats 采样。采样循环的 sleep 为 0.1 秒，实际间隔还包含读数开销，
以记录的客体 uptime 为准。采样有固定上限，主机断连也不会留下无限循环。

`command.sent_at` / `returned_at` 使用宿主 UTC，`round_trip_seconds` 使用宿主单调时钟。
CLI 模式测量整个 CLI 的检查和请求，不假装知道其内部 `sm` 精确发出时刻。
`sm` 模式的 stdout 另存客体 `SM_SENT`、`SM_RETURNED` 与 uptime，包住实际 `sm` 调用。
vold 与 diskstats 使用同一客体 uptime 时基，不将宿主墙钟直接减去客体时间。

原始 JSON 含完整 dmesg，公开前检查内容。测试结束、确认采样退出后，再正常关闭专用实例及私有 adb server。

## 2026-11-05 实测

环境为 Windows 11 x64、WHPX、Android 14、4 GiB 内存、4 核、16 GiB 数据盘、720p 无头、无执行桥。
使用现有正式包 v0.37.1，ZIP SHA-256 为
`d0938755e9fcbe85abc4739d3449796b2c715dbad63eedcce3d652bb1b40c6fd`，
复制后的 308 个发版清单文件全部匹配。SDK 和 guest-patches 从本机已有测试根复制，
没有共享 SDK 或复制原实例。测量使用的 CLI 以 Go 1.26.5 基于 `upstream/main dc6501a`
加本 PR 构建，具体二进制 SHA-256 记录在 JSON 中，与最终 CLI 构建一致。

使用新根 `C:\Temp\modavd-95-trim-00da\root`、新实例 0 和 27400–27499 端口段。
准备时没有运行中的 QEMU。采样只操作本任务的实例和数据盘，未运行 compact、安装应用、换包或关机。
正式包旧 CLI 不含 `disk` 命令，所以测量 CLI 使用源码构建产物。

`/data` 是 ext4，挂在 `254:40 /dev/block/dm-40`，sysfs 的唯一 slave 是
`253:32 vdc`。两层 `discard_max_bytes` 都是 0。内核为
`6.1.23-android14-4-00257-g7e35917775b8-ab9964412`。

结果及对应日志、采样边界读数见 [trim-20261105.json](trim-20261105.json)。
有写入场景均确认 `dd` 写入 134,217,728 字节，`du` 为 131,076 KiB，随后删除并 sync。

| 场景 | 宿主命令往返 | 命令退出码 | 客体证据 |
| --- | ---: | ---: | --- |
| `sm`，写入后删除 128 MiB | 0.414635 秒 | 0 | `/data` 开始尝试后明确失败，维护线程随后结束 |
| `sm`，空闲重复第 1 次 | 0.154564 秒 | 0 | 同上 |
| `sm`，空闲重复第 2 次 | 0.158680 秒 | 0 | 同上 |
| CLI，写入后删除 128 MiB | 0.616822 秒 | 4 | 能力检查拒绝，窗口内没有本轮 vold trim 日志 |
| CLI，空闲重复 | 0.225909 秒 | 4 | 同上 |

本包不能提供成功回收验证：新版 CLI 在能力检查时返回 `unsupported`，退出码 4，未触发 vold；
直接 `sm` 在写入后删除和空闲重复场景均返回 0，vold 却明确记录 `/data` trim 不支持。
两层 discard 计数全程为 0，dmesg 没有提供本轮成功完成信号。
实机覆盖的是工作量准备后的拒绝/失败和空闲重复触发，**没有成功执行 discard 或测得成功完成耗时**。
本次按现有包的范围报告结果，未另取模拟器或 AOSP。

因此不能用这些数据证明“命令总在实际 discard 完成前返回”，也不能证明成功 FITRIM 的日志格式、
vold 毫秒数覆盖的内部工作范围、宿主空间回收或性能。成功日志判定仅有针对性单测。
支持 discard 的正式环境仍需用同一脚本补跑工作量和空闲重复两类场景，缺完成信号时继续报告未知。

## 自动验证

```powershell
python -B -I -X utf8 scripts/test_measure_trim.py
go test -count=1 -run 'TestDiskJobs|TestDiskTrim|TestToolDefinitions|TestToolCalls' ./internal/ui ./internal/cli ./internal/mcp
```

解析测试覆盖明确返回、零字节返回、真实失败后维护仍结束、缺日志、旧日志、其它挂载点、
设备号、缺少 discard 字段、进程或线程不匹配、重叠轮次、窗口外日志及稳定计数。
GUI HTTP 测试检查完成消息只说“已触发”，MCP 测试检查工具公开说明，CLI 现有测试保留准确原文。
