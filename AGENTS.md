# ModAVDManager 维护规矩

**简体中文** | [English](AGENTS.en.md)

<!-- AGENTS.md 和 AGENTS.en.md 逐节对应：改了其中一份，请在同一个 PR 里把另一份也改了。AI 工具自己只会读 AGENTS.md。 -->

通用参与、分支、提交、隐私和发版规矩见 ModAVD 主仓库的 [AGENTS.md](https://github.com/ModAVD/ModAVD/blob/main/AGENTS.md)。本文件只补充管理器仓库自己的约定。

## 目录

- 根目录的 `main.go` 是命令行和 MCP 服务 `modavd.exe` 的入口，`cmd/modavd-gui/` 是图形管理器 `ModAVD.exe` 的入口，`cmd/modavd-proxyd/` 是装进实例里的按应用代理（客体一侧，Linux）。`internal/` 是它们的实现。
- `build.ps1` 负责构建（输出 `out\tools\modavd.exe`、`out\ModAVD.exe`），`release/` 保存管理器各版的发布说明和校验和，`changelog.d/` 保存发版条目（见 `changelog.d/README.md`），`scripts/` 放页面脚本的 Node 测试、仓库检查和测量脚本。

## 测试

- 改 Go 代码至少运行 `go vet ./...` 和 `go test ./...`，`gofmt -l .` 不能有输出。再分别设 `GOOS=darwin GOARCH=arm64`、`GOOS=linux GOARCH=amd64` 跑 `go vet ./...`；设了 `GOOS` 只跑 `go vet`，不跑 `go test`。
- CI（`.github/workflows/ci.yml`）在 Windows 上还会跑 `scripts/` 里的一部分 Node 测试、`build.ps1`、隐私扫描、提交作者检查和 Python 脚本测试，在 macOS、Ubuntu 上跑 `go vet`、`go test`。改了 `internal/ui/web/` 的页面脚本，就在本地跑对应的 `node --test scripts/test_*.mjs`。
- Windows GUI、更新、签名和问题包路径的改动要在 PR 里写清实际运行过的检查。

## 做事

- 脚本和测试使用临时根目录，不读取生产根目录或真实用户数据。
- 管理器的用户可见改动在同一个 PR 中补 `changelog.d/<名字>.<feat|fix|change|remove>.md`。
- 界面和命令行显示的中文就是翻译的键：新加或改了中文字符串，按 `internal/i18n/README.md` 补英文译文（`TestCatalog` 会查）。
- 中英成对的文档（`X.md`、`X.en.md`）两份一起改。
- `scripts/privacy.py`、`scripts/repo_checks.py`、`scripts/test_repo_checks.py` 和 ModAVD 主仓库的逐字节相同（CI 会核对），要改先在 ModAVD 改；只有 `scripts/privacy-allowlist.json` 是本仓库自己的。
