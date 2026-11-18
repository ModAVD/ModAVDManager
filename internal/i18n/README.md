# Translations / 界面翻译

The code is written in Chinese, and the Chinese text is the key: `locales/<lang>/<package>.json` maps each Chinese
string the code shows to its translation (`web.json` holds the page's own strings). See the package comment in
`i18n.go` for where translation happens and what stays Chinese.

代码里照旧写中文，中文原文就是键。新加或改了中文字符串以后：

```
go test ./internal/i18n -run TestCatalog -update          # 把缺的键以 "" 加进表
go test ./internal/i18n -run TestCatalog -update -prune   # 同时删掉源码里已经没有的键
```

再把 `""` 填成英文。`null` 表示故意不翻（只进日志文件、问题包，或者是拿来匹配中文输出的文字、发进客体的命令）。
`TestCatalog` 查四件事：表里有没有漏的、有没有过期的、译文的参数（`%d`、`%[2]s`、页面的 `{name}`）和原文对不对得上、
页面里有没有写在 `t()` 外面的中文。

`extra.json` 放代码拼出来、但拼的那段本身没有中文词的句式（比如 `fmt.Sprintf("%s%s（%s）", 标题, "通过", 原因)`，按拼好的样子写成 `%s通过（%s）`），不查是否过期。

## Writing the English

- Keep every verb of the key (`%d`, `%s`, `%v`, `%q`, `%.1f` …). To reorder, index them: `%[2]s … %[1]d`. A key
  built from a concatenation has `%v` for each non-literal part.
- Plain, short English; sentence case; no Chinese punctuation. Keep leading / trailing spaces and newlines as in the
  key.
- A message is often built from several keys ("外层：内层"): translate each key on its own so it reads well alone.
- `null` only for text no person sees in the manager or the CLI: problem report contents, patterns matched against
  Chinese output of Windows or games, commands and values sent to the guest, file names.

## Terms

| 中文 | English |
| --- | --- |
| 实例 / 实例 3 | instance / instance 3 |
| 新建 | create |
| 开机 / 启动 | start (the instance); boot (Android booting) |
| 关机 / 强制关机 | shut down / force stop |
| 根目录 | root folder |
| 管理器 | manager |
| 命令行 | command line, CLI |
| 组件 / 补全组件 | components / Fetch components |
| 数据盘 / 自动扩盘 | data disk / automatic size |
| 无头 | headless |
| 老板键 | boss key |
| 托盘 | tray |
| 执行桥 | execution bridge |
| 问题包 | problem report |
| 电脑检查 | System check |
| 更新源 | update source |
| Mirror 酱 | MirrorChyan |
| 客体 / 宿主 | guest / host |
| 首次初始化 | initial setup |
| 更多操作 | More actions |
| 设置 | Settings |
| 检查更新 / 更新 | Check for updates / Update |
| 打开管理器 / 退出 | Open manager / Exit |
| 帧率 | frame rate (FPS) |
| 分辨率 | resolution |
| 加速 | acceleration |
| 诊断 | diagnostics |
| 代理 | proxy |
| 安卓 | Android |
| 星铁 / 终末地 / 1999 / 明日方舟 / 阴阳师 | Honkai: Star Rail / Arknights: Endfield / Reverse: 1999 / Arknights / Onmyoji |
