# Run compact policy, controlled-process, result-contract and real-image checks
# using only an existing SDK. No emulator, adb, AOSP or download is involved.
param(
    [Parameter(Mandatory)][string]$SdkRoot,
    [Parameter(Mandatory)][string]$TestRoot,
    [string]$Go = 'go'
)
$ErrorActionPreference = 'Stop'
$src = Split-Path $PSScriptRoot -Parent
$sdk = [IO.Path]::GetFullPath($SdkRoot)
$testDir = [IO.Path]::GetFullPath($TestRoot)
if (-not (Test-Path -LiteralPath (Join-Path $sdk 'emulator\qemu-img.exe'))) { throw 'An existing SDK with qemu-img.exe is required' }
$ownedSdk = Join-Path $testDir 'sdk'
$emulatorDir = Join-Path $ownedSdk 'emulator'
New-Item -ItemType Directory -Path $emulatorDir -Force | Out-Null
if ($sdk -ne $ownedSdk) {
    Copy-Item -LiteralPath (Join-Path $sdk 'emulator\qemu-img.exe') -Destination $emulatorDir
    Get-ChildItem -LiteralPath (Join-Path $sdk 'emulator') -Filter '*.dll' -File | Copy-Item -Destination $emulatorDir
}
$saved = @{}
$envs = @{GOTOOLCHAIN='local'; GOPROXY='off'; MODAVD_TEST_SDK_ROOT=$ownedSdk; MODAVD_TEST_TMP=(Join-Path $testDir 'temp')}
foreach ($k in $envs.Keys) { $saved[$k]=[Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k,$envs[$k]) }
Push-Location $src
try {
    $want = (Select-String -LiteralPath go.mod -Pattern '^toolchain\s+(go\S+)').Matches.Groups[1].Value
    if ((& $Go env GOVERSION).Trim() -ne $want) { throw "Use the existing $want toolchain via -Go; this script does not download it" }
    & (Join-Path $emulatorDir 'qemu-img.exe') --version
    if ($LASTEXITCODE) { throw 'qemu-img could not start; check its local runtime DLLs' }
    & $Go test -count=1 -v '-run=^TestCompact|^TestDiskCompactIntegration$' ./internal/emu ./internal/cli ./internal/mcp ./internal/ui 2>&1 | Tee-Object -FilePath (Join-Path $testDir 'compact-tests.log')
    if ($LASTEXITCODE) { throw 'compact regression tests failed' }
    & node --test scripts/test_compact_format.mjs 2>&1 | Tee-Object -FilePath (Join-Path $testDir 'compact-format.log')
    if ($LASTEXITCODE) { throw 'compact browser formatting tests failed' }
    & python -B -I -X utf8 scripts/verify_compact_process.py --go $Go --out (Join-Path $testDir 'process-checks')
    if ($LASTEXITCODE) { throw 'compact controlled deadline or fault checks failed' }
} finally {
    Pop-Location
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k,$saved[$k]) }
}
