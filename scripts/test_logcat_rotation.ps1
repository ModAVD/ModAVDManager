# Run only against an already-created, disposable instance with its own root, SDK and port plan.
# Exercises the production 256 MiB threshold twice; writes about 580 MB and stops the instance when finished.
param(
    [Parameter(Mandatory)][string]$Root,
    [Parameter(Mandatory)][string]$Binary,
    [int]$Index = 0,
    [int]$Count = 190000
)
$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path -LiteralPath $Root).Path
$Binary = (Resolve-Path -LiteralPath $Binary).Path
$meta = Get-Content -LiteralPath (Join-Path $Root 'mas-avd.json') -Raw | ConvertFrom-Json
if (-not $meta.portBase -or $meta.portBase -eq 20000 -or $meta.sdkRoot) {
    throw 'Use a disposable root with its own SDK and nondefault port plan.'
}
if ($Index -lt 0 -or $Index -gt 9 -or $Index -eq 5) { throw 'Invalid instance index' }
$adb = Join-Path $Root 'sdk/platform-tools/adb.exe'
$serial = 'emulator-' + ($meta.portBase + 10 * $Index)
$server = [string]$meta.adbServerPort
$logs = Join-Path $Root 'logs'
$token = 'rotation97-' + [guid]::NewGuid().ToString('N')
$evidence = Join-Path $Root $token
New-Item -ItemType Directory -Path $evidence | Out-Null

function Invoke-CLI([string]$Verb) {
    $stdout = Join-Path $evidence "$Verb-$([guid]::NewGuid().ToString('N')).out"
    $stderr = "$stdout.err"
    $argv = @('--root', ('"' + $Root + '"'), '--json', $Verb, [string]$Index)
    if ($Verb -eq 'start') { $argv += @('--headless', '--no-bridge', '--timeout', '180') }
    $p = Start-Process -FilePath $Binary -ArgumentList $argv -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $p.WaitForExit()
    if ($p.ExitCode -ne 0) { throw "$Verb failed: $(Get-Content -LiteralPath $stderr -Raw)" }
    if (Get-Process -Id $p.Id -ErrorAction SilentlyContinue) { throw 'Launching CLI still runs' }
    return $p.Id
}

function Get-Captures {
    @(Get-CimInstance Win32_Process -Filter "Name='adb.exe'" | Where-Object {
        $_.ExecutablePath -eq $adb -and $_.CommandLine -match "-P $server " -and
        $_.CommandLine -match "-s $serial logcat "
    })
}

function Wait-For([scriptblock]$Check) {
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while (-not (& $Check)) {
        if ([DateTime]::UtcNow -gt $deadline) { throw 'Logcat condition timed out' }
        Start-Sleep -Milliseconds 100
    }
}

try {
    $launcher = Invoke-CLI start
    Wait-For { @(Get-Captures).Count -eq 1 }
    $capture = (Get-Captures)[0]
    # The CLI has exited before any numbered records are generated. One toybox log process reads all stdin.
    # Pace the stream so the host can drain logd without guest buffer overruns.
    $guest = @'
awk 'BEGIN { p=""; for(j=0;j<3000;j++)p=p"x"; for(i=0;i<COUNT;i++) { printf "TOKEN sequence=%06d %s\n",i,p; if(i%32==31)system("sleep 0.02") } }' | log -t ROTATION97
'@
    $guest = $guest.Replace('COUNT', [string]$Count).Replace('TOKEN', $token)
    $timer = [Diagnostics.Stopwatch]::StartNew()
    & $adb -P $server -s $serial shell $guest
    if ($LASTEXITCODE) { throw 'Guest log generation failed' }
    & $adb -P $server -s $serial shell "log -t ROTATION97 $token-after-rotation"
    if ($LASTEXITCODE) { throw 'Post-rotation marker failed' }
    Start-Sleep -Milliseconds 500
    $paths = @(Get-ChildItem -LiteralPath $logs -Filter "logcat-mas_$Index-*.txt" | Sort-Object Name)
    $next = 0
    $tail = $false
    $segments = @()
    foreach ($path in $paths) {
        # Query the open handle, since directory enumeration can cache the size of an actively written file.
        $stream = [IO.File]::Open($path.FullName, 'Open', 'Read', 'ReadWrite')
        $size = $stream.Length
        $reader = [IO.StreamReader]::new($stream)
        try {
            while ($null -ne ($line = $reader.ReadLine())) {
                if ($line -match "$token sequence=(\d+) ") {
                    if ([int]$Matches[1] -ne $next) { throw "Lost/duplicate record: expected $next, got $($Matches[1])" }
                    $next++
                }
                if ($line.Contains("$token-after-rotation")) { $tail = $true }
            }
        } finally { $reader.Dispose() }
        $segments += [ordered]@{ name = $path.Name; bytes = $size }
    }
    if ($next -ne $Count -or -not $tail) { throw "Incomplete stream: $next/$Count, tail=$tail" }
    if (@($segments | Where-Object { $_.bytes -ge 256MB }).Count -lt 2) { throw 'Did not cross 256 MiB twice' }
    $live = @(Get-Captures)
    if ($live.Count -ne 1 -or $live[0].ProcessId -ne $capture.ProcessId) { throw 'Capture restarted during rotation' }
    $null = Invoke-CLI stop
    Wait-For { @(Get-Captures).Count -eq 0 }
    $null = Invoke-CLI start
    Wait-For { @(Get-Captures).Count -eq 1 }
    $restarted = (Get-Captures)[0]
    if ($restarted.ProcessId -eq $capture.ProcessId) { throw 'Old capture survived stop' }
    & $adb -P $server -s $serial shell "log -t ROTATION97 $token-after-restart"
    Start-Sleep -Milliseconds 500
    $latest = Get-ChildItem -LiteralPath $logs -Filter "logcat-mas_$Index-*.txt" | Sort-Object Name | Select-Object -Last 1
    if (-not (Select-String -LiteralPath $latest.FullName -SimpleMatch "$token-after-restart" -Quiet)) {
        throw 'Restarted capture is not writing'
    }
    [ordered]@{
        token = $token; launcherExited = $launcher; numberedRecords = $next; seconds = $timer.Elapsed.TotalSeconds
        capturePid = $capture.ProcessId; restartedCapturePid = $restarted.ProcessId
        segments = $segments; thresholdBytes = 256MB; rotationsAtThreshold = 2; postRotationMarker = $tail
    } | ConvertTo-Json -Depth 5 | Tee-Object -FilePath (Join-Path $evidence 'result.json')
} finally {
    $null = Invoke-CLI stop
}
