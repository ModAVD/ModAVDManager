# Build tools\modavd.exe reproducibly: the same source and Go version give the same bytes on any machine and path.
#
#   pwsh -NoProfile -File build.ps1                         # -> out\tools\modavd.exe, out\ModAVD.exe
#   pwsh -NoProfile -File build.ps1 -Out <dir> -Go <go1.26.5>\bin\go.exe
#   pwsh -NoProfile -File build.ps1 -Test                    # go vet + go test first
#
# modavd.exe is the console program (CLI, MCP); ModAVD.exe is the manager, a GUI-subsystem exe (-H=windowsgui, no
# console window when double-clicked) built from cmd/modavd-gui. In the package: <root>\tools\modavd.exe and
# <root>\ModAVD.exe.
#
# -trimpath keeps local paths (GOROOT under C:\Users, the source dir) out of the binary, which the release privacy scan
# would flag; -buildvcs=false keeps repository metadata and dirty flags out. GOTOOLCHAIN=local plus the check
# against go.mod's toolchain line makes sure no other Go version builds it silently. No -X version stamp (see
# internal/buildinfo).

param(
    # output directory
    [string]$Out,
    # go.exe to use; default $env:MODAVD_GO, then go on PATH
    [string]$Go,
    [switch]$Test
)

$ErrorActionPreference = 'Stop'
$src = $PSScriptRoot
if (-not $Out) { $Out = Join-Path $src 'out' }
if (-not $Go) { $Go = if ($env:MODAVD_GO) { $env:MODAVD_GO } else { (Get-Command go -ErrorAction Stop).Source } }

$want = (Select-String -LiteralPath (Join-Path $src 'go.mod') -Pattern '^toolchain\s+(go\S+)' | Select-Object -First 1).Matches.Groups[1].Value
if (-not $want) { throw 'go.mod has no toolchain line' }

$saved = @{}
$envs = [ordered]@{
    GOTOOLCHAIN = 'local'; GOENV = 'off'; GOFLAGS = ''; GOWORK = 'off'; GOPROXY = 'off'; CGO_ENABLED = '0'
    GOOS = 'windows'; GOARCH = 'amd64'; GOAMD64 = 'v1'
}
foreach ($k in $envs.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $envs[$k]) }
try {
    $have = (& $Go env GOVERSION).Trim()
    if ($have -ne $want) { throw "Go version $have does not match go.mod toolchain $want ($Go)" }
    Push-Location $src
    try {
        if ($Test) {
            & $Go vet ./...
            if ($LASTEXITCODE) { throw "go vet failed ($LASTEXITCODE)" }
            & $Go test ./...
            if ($LASTEXITCODE) { throw "go test failed ($LASTEXITCODE)" }
        }
        # The package layout: <root>\tools\modavd.exe and <root>\ModAVD.exe. Never side by side: Windows names are
        # case-insensitive, so modavd.exe and ModAVD.exe in one directory are the same file.
        New-Item -ItemType Directory -Force (Join-Path $Out 'tools') | Out-Null
        & $Go build -trimpath -buildvcs=false '-ldflags=-s -w' -o (Join-Path $Out 'tools\modavd.exe') .
        if ($LASTEXITCODE) { throw "go build modavd.exe failed ($LASTEXITCODE)" }
        & $Go build -trimpath -buildvcs=false '-ldflags=-s -w -H=windowsgui' -o (Join-Path $Out 'ModAVD.exe') ./cmd/modavd-gui
        if ($LASTEXITCODE) { throw "go build ModAVD.exe failed ($LASTEXITCODE)" }
    } finally { Pop-Location }
} finally {
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
}
foreach ($name in 'tools\modavd.exe', 'ModAVD.exe') {
    $f = Join-Path $Out $name
    $hash = (Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Host ("{0}  {1} bytes  sha256 {2}  ({3})" -f $f, (Get-Item -LiteralPath $f).Length, $hash, $want)
}
