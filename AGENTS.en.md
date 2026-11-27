# ModAVDManager maintenance rules

[简体中文](AGENTS.md) | **English**

<!-- AGENTS.md and AGENTS.en.md match section by section: if you change one, change the other in the same PR. AI tools only read AGENTS.md on their own. -->

The general rules for contributing, branches, commits, privacy and releases are in the ModAVD main repository's [AGENTS.en.md](https://github.com/ModAVD/ModAVD/blob/main/AGENTS.en.md). This file only adds what is specific to the manager repository.

## Layout

- `main.go` at the top is the entry point of the CLI and MCP server `modavd.exe`, `cmd/modavd-gui/` that of the GUI manager `ModAVD.exe`, and `cmd/modavd-proxyd/` is the per-app proxy installed into instances (the guest half, Linux). `internal/` holds their implementation.
- `build.ps1` builds (output `out\tools\modavd.exe`, `out\ModAVD.exe`), `release/` keeps the manager's release notes and checksums for each version, `changelog.d/` keeps the release note lines (see `changelog.d/README.md`), and `scripts/` holds the Node tests of the page scripts, the repository checks and the measurement scripts.

## Tests

- For Go changes run at least `go vet ./...` and `go test ./...`, and `gofmt -l .` must print nothing. Then run `go vet ./...` with `GOOS=darwin GOARCH=arm64` and with `GOOS=linux GOARCH=amd64`; with `GOOS` set, run only `go vet`, not `go test`.
- On Windows, CI (`.github/workflows/ci.yml`) also runs some of the Node tests in `scripts/`, `build.ps1`, the privacy scan, the commit author check and the Python script tests; on macOS and Ubuntu it runs `go vet` and `go test`. If you change page scripts in `internal/ui/web/`, run the matching `node --test scripts/test_*.mjs` locally.
- For changes to the Windows GUI, updates, signing and problem reports, say in the PR which checks you actually ran.

## Working

- Scripts and tests use a temporary root folder; they never read a production root folder or real user data.
- A manager change that users can see adds `changelog.d/<name>.<feat|fix|change|remove>.md` in the same PR, with two lines: Chinese, then English.
- The Chinese text the manager and the CLI show is the translation key: when you add or change a Chinese string, add its English translation as described in `internal/i18n/README.md` (`TestCatalog` checks this).
- Change both files of a Chinese / English document pair (`X.md`, `X.en.md`) together.
- `scripts/privacy.py`, `scripts/repo_checks.py` and `scripts/test_repo_checks.py` are byte for byte the same as in the ModAVD main repository (CI checks this): change them in ModAVD first. Only `scripts/privacy-allowlist.json` belongs to this repository.
