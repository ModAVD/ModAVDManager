# ModAVDManager 维护规矩

通用参与、分支、提交、隐私和发版规矩见 ModAVD 主仓库的 [AGENTS.md](https://github.com/ModAVD/ModAVD/blob/main/AGENTS.md)。本文件只补充管理器仓库自己的约定。

## 目录

- `cmd/modavd-gui/` 是 GUI 管理器入口，`internal/` 是管理器、命令行和 MCP 的实现。
- `build.ps1` 负责构建，`release/` 保存管理器发布说明，`changelog.d/` 保存发版条目。

## 测试

改 Go 代码至少运行 `go vet ./...` 和 `go test ./...`。Windows GUI、更新、签名和报告路径的改动要在 PR 里写清实际运行过的检查。

## 做事

脚本和测试使用临时根目录，不读取生产根目录或真实用户数据。管理器的用户可见改动在同一个 PR 中补 `changelog.d/<名字>.<feat|fix|change|remove>.md`。
