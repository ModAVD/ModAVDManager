# Writes testdata/golden/: what the ModAVD package scripts report-config.ps1 and collect-info.ps1 print for the fake
# computers and roots of scenarios.json. The report tests compare modavd's output with it line by line.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File gen_golden.ps1 -ScriptDir <ModAVD>\package -Work <empty dir>
#
# Run it with Windows PowerShell 5.1 in a time zone without daylight saving time in January (the fake files are dated
# January 2026; the tests read the UTC offset back from summary.txt), with -Work on a volume that makes 8.3 short
# names (the shortname scenario's %TEMP% is one). Each script runs unchanged in its own
# powershell.exe except for what is injected into a copy of it:
#   - after `$ErrorActionPreference = 'Stop'`: functions of the same name in front of the cmdlets it asks Windows
#     with (Get-CimInstance, Get-ItemProperty, Get-NetTCPConnection, netstat, Get-Partition / Get-Disk /
#     Get-PhysicalDisk, Get-WinEvent, Start-Process, Set-Clipboard, Get-Date), answering from the scenario;
#   - before the first section: AdbDevices / AdbShell (report-config) and Invoke-Native (collect-info), answering
#     from the scenario's adb data, so no adb server, adb.exe or emulator is ever touched;
#   - `New-Object IO.DriveInfo` becomes MockDriveInfo, and the size limits take the scenario's smaller values.
# Paths, the drive letter, the adb port and the PowerShell version become placeholders ({{BASE}}, {{DRIVE}},
# {{ADBPORT}}, {{PSVERSION}}); section timings become 0.0 秒.
param(
    [Parameter(Mandatory = $true)][string]$ScriptDir,
    [Parameter(Mandatory = $true)][string]$Work,
    [string]$Out,
    [switch]$OnlyCollect
)
$ErrorActionPreference = 'Stop'
if (-not $Out) { $Out = Join-Path $PSScriptRoot 'golden' }
$Work = [IO.Path]::GetFullPath($Work)
if ((Test-Path -LiteralPath $Work) -and @(Get-ChildItem -LiteralPath $Work -Force).Count -gt 0) { throw "$Work is not empty" }
[void](New-Item -ItemType Directory -Path $Work -Force)
$AdbPort = 47913
$Utf8 = New-Object Text.UTF8Encoding $false
$Utf8Bom = New-Object Text.UTF8Encoding $true

# ---------------------------------------------------------------- injected into the scripts

$Prelude = @'
# ---- injected by gen_golden.ps1: the scenario answers instead of Windows
[Console]::OutputEncoding = New-Object Text.UTF8Encoding $false
$MockScenario = ([IO.File]::ReadAllText($env:MODAVD_MOCK_FILE, [Text.Encoding]::UTF8) | ConvertFrom-Json).($env:MODAVD_MOCK_NAME)
$MockAdbPort = [int]$env:MODAVD_MOCK_ADBPORT
function MockFail([string]$Name) { $e = $MockScenario.($Name + 'Err'); if ($e) { throw [string]$e } }
function Get-Date { [datetime]::ParseExact($MockScenario.now, 'yyyy-MM-dd HH:mm:ss', [Globalization.CultureInfo]::InvariantCulture) }
function Get-CimInstance {
    [CmdletBinding()] param([Parameter(Position = 0)][string]$ClassName, [string]$Filter, [string]$Namespace)
    switch ($ClassName) {
        'Win32_OperatingSystem' { MockFail 'os'; return $MockScenario.os }
        'Win32_Processor' { MockFail 'cpus'; return @($MockScenario.cpus) }
        'Win32_ComputerSystem' { MockFail 'cs'; return $MockScenario.cs }
        'Win32_VideoController' { MockFail 'gpus'; return @($MockScenario.gpus) }
        'Win32_OptionalFeature' { MockFail 'features'; return @($MockScenario.features) }
        'AntiVirusProduct' { MockFail 'av'; return @($MockScenario.av) }
        'Win32_Process' {
            MockFail 'processes'
            $all = @($MockScenario.processes)
            if ($Filter -match "^Name='(.+)'$") { $n = $Matches[1]; return @($all | Where-Object { $_.Name -eq $n }) }
            if ($Filter -match "^Name like '(.+)%'$") { $n = $Matches[1]; return @($all | Where-Object { $_.Name -like ($n + '*') }) }
        }
    }
    throw "mock: unexpected Get-CimInstance $ClassName $Filter"
}
function Get-ItemProperty {
    [CmdletBinding()] param([Parameter(Position = 0)][string]$Path, [string]$Name)
    if ($Path -eq 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion') {
        MockFail 'currentVersion'
        if ($Name -and ($MockScenario.currentVersion.PSObject.Properties.Name -notcontains $Name)) { throw "Property $Name does not exist" }
        return $MockScenario.currentVersion
    }
    $keys = @{
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' = 'HKLM'
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*' = 'WOW'
        'HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' = 'HKCU'
    }
    if ($keys.ContainsKey($Path)) { return @($MockScenario.uninstall.($keys[$Path])) }
    throw "mock: unexpected Get-ItemProperty $Path"
}
function MockListeners {
    foreach ($l in @($MockScenario.listeners)) {
        New-Object PSObject -Property @{ LocalAddress = [string]$l.LocalAddress; LocalPort = [int]$l.LocalPort; OwningProcess = [int]$l.OwningProcess }
    }
}
function Get-NetTCPConnection {
    [CmdletBinding()] param([string]$State, [int]$LocalPort)
    MockFail 'listeners'
    $all = @(MockListeners)
    if ($PSBoundParameters.ContainsKey('LocalPort')) {
        $hit = @($all | Where-Object { $_.LocalPort -eq $LocalPort })
        if ($hit.Count -eq 0) { throw 'No matching MSFT_NetTCPConnection objects found' }
        return $hit
    }
    return $all
}
function netstat { MockFail 'listeners'; foreach ($l in @(MockListeners)) { '  TCP    {0}:{1}    0.0.0.0:0    LISTENING    {2}' -f $l.LocalAddress, $l.LocalPort, $l.OwningProcess } }
function Get-Partition { [CmdletBinding()] param([string]$DriveLetter) MockFail 'disk'; New-Object PSObject -Property @{ DiskNumber = 0 } }
function Get-Disk { [CmdletBinding()] param([Parameter(ValueFromPipeline = $true)]$InputObject) process { New-Object PSObject -Property @{ Number = 0; BusType = $MockScenario.disk.BusType } } }
function Get-PhysicalDisk { [CmdletBinding()] param() New-Object PSObject -Property @{ DeviceId = '0'; MediaType = $MockScenario.disk.MediaType } }
function MockDriveInfo($Arg) {
    MockFail 'drive'
    $n = [string]$Arg
    if (-not $n.EndsWith('\')) { $n += '\' }
    New-Object PSObject -Property @{ Name = $n; AvailableFreeSpace = [long]$MockScenario.drive.AvailableFreeSpace
        TotalSize = [long]$MockScenario.drive.TotalSize; DriveFormat = [string]$MockScenario.drive.DriveFormat }
}
function Get-WinEvent {
    [CmdletBinding()] param([hashtable]$FilterHashtable, [int]$MaxEvents)
    MockFail 'events'
    foreach ($e in @($MockScenario.events)) {
        New-Object PSObject -Property @{ TimeCreated = [datetime]$e.TimeCreated; Id = [int]$e.Id; ProviderName = [string]$e.ProviderName; Message = [string]$e.Message }
    }
}
function Start-Process {
    [CmdletBinding()] param([string]$FilePath, [object[]]$ArgumentList, [switch]$NoNewWindow, [switch]$PassThru, [string]$RedirectStandardOutput, [string]$RedirectStandardError)
    if ($FilePath -eq 'explorer.exe') { return }
    $r = $MockScenario.run.(Split-Path -Leaf $FilePath)
    if (-not $r) { throw "mock: unexpected Start-Process $FilePath" }
    if ($r.startErr) { throw [string]$r.startErr }
    [IO.File]::WriteAllText($RedirectStandardOutput, [string]$r.stdout)
    [IO.File]::WriteAllText($RedirectStandardError, [string]$r.stderr)
    $p = New-Object PSObject
    if ($r.timeout) { $p | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value { param($ms) $false } }
    else { $p | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value { param($ms) $true } }
    $p | Add-Member -MemberType ScriptMethod -Name Kill -Value { }
    $p
}
function Set-Clipboard { [CmdletBinding()] param($Value) if ($MockScenario.clipboardErr) { throw [string]$MockScenario.clipboardErr } }
# ---- end of injection
'@

$RcOverrides = @'
# ---- injected by gen_golden.ps1: the adb server of the scenario
function AdbDevices([int]$port) {
    if ($port -ne $MockAdbPort) { throw 'mock: nothing listens there' }
    [string]$MockScenario.adb.devices
}
function AdbShell([int]$port, [string]$serial, [string]$cmd) {
    $s = $MockScenario.adb.serials.$serial
    if (-not $s) { throw ('adb server 返回 FAIL：' + "device '$serial' not found") }
    $r = $s.shell.$cmd
    $t = ''
    if ($r) { $t = [string]$r.out + [string]$r.err }
    $t.Trim()
}
# ---- end of injection
'@

$CiOverrides = @'
# ---- injected by gen_golden.ps1: emulator.exe and adb.exe of the scenario
function MockShell($s, [string]$key) {
    $r = $s.shell.$key
    $out = ''; $err = ''; $code = 0
    if ($r) { $out = [string]$r.out; $err = [string]$r.err; if ($null -ne $r.exit) { $code = [int]$r.exit } }
    if (@(([string]$s.features) -split ',') -contains 'shell_v2') { return @{ TimedOut = $false; ExitCode = $code; Stdout = $out; Stderr = $err } }
    # the old shell protocol: stderr merged into stdout, no exit code
    @{ TimedOut = $false; ExitCode = 0; Stdout = $out + $err; Stderr = '' }
}
# adb.exe writes the guest's output through the C runtime's text mode: every LF becomes CRLF (not for exec-out, which
# goes to -StdoutFile as bytes)
function AdbTextMode($r) {
    $r.Stdout = ([string]$r.Stdout).Replace("`n", "`r`n")
    $r.Stderr = ([string]$r.Stderr).Replace("`n", "`r`n")
    $r
}
function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments, [int]$TimeoutSec = 20, [string]$StdoutFile, [hashtable]$ExtraEnv)
    $leaf = Split-Path -Leaf $Exe
    $res = @{ TimedOut = $false; ExitCode = 0; Stdout = ''; Stderr = '' }
    if ($leaf -eq 'emulator.exe') {
        $r = $MockScenario.run.'emulator.exe'
        if ($r.timeout) { $res.TimedOut = $true; $res.ExitCode = $null }
        else { $res.ExitCode = [int]$r.exit; $res.Stdout = [string]$r.stdout; $res.Stderr = [string]$r.stderr }
    } elseif ($leaf -eq 'adb.exe') {
        $a = @($Arguments)
        if ($a[0] -ne '-P' -or [int]$a[1] -ne $MockAdbPort) { throw ('mock: unexpected adb ' + ($a -join ' ')) }
        if ($a[2] -eq 'devices') {
            $res.Stdout = "List of devices attached`n" + [string]$MockScenario.adb.devices + "`n"
        } elseif ($a[2] -eq '-s') {
            $serial = $a[3]; $cmd = @($a[4..($a.Count - 1)])
            $s = $MockScenario.adb.serials.$serial
            if (-not $s) {
                # adb.exe's own message, through the same text mode; a -StdoutFile is still created (empty): the real
                # Invoke-Native creates it before adb.exe runs
                $res.ExitCode = 1; $res.Stderr = "adb: device '$serial' not found`r`n"
                if ($StdoutFile) { [IO.File]::WriteAllBytes($StdoutFile, (New-Object byte[] 0)) }
            } elseif ($cmd[0] -eq 'exec-out') {
                $e = $s.exec.(($cmd[1..($cmd.Count - 1)]) -join ' ')
                $bytes = New-Object byte[] 0
                if ($e) { $bytes = [Convert]::FromBase64String([string]$e.b64) }
                if ($StdoutFile) { [IO.File]::WriteAllBytes($StdoutFile, $bytes) }
            } elseif ($cmd[0] -eq 'shell') {
                $res = AdbTextMode (MockShell $s (($cmd[1..($cmd.Count - 1)]) -join ' '))
            } elseif ($cmd[0] -eq 'logcat') {
                # adb's logcat(): export ANDROID_LOG_TAGS=''; exec logcat '<arg>'...
                $key = "export ANDROID_LOG_TAGS=''; exec logcat"
                foreach ($x in $cmd[1..($cmd.Count - 1)]) { $key += " '" + ($x -replace "'", "'\''") + "'" }
                $res = AdbTextMode (MockShell $s $key)
            } else { throw ('mock: unexpected adb ' + ($a -join ' ')) }
        } else { throw ('mock: unexpected adb ' + ($a -join ' ')) }
    } else {
        throw "mock: unexpected Invoke-Native $Exe"
    }
    New-Object PSObject -Property $res
}
# ---- end of injection
'@

function Replace-Once([string]$Text, [string]$Find, [string]$With, [int]$Count = 1) {
    $n = ([regex]::Matches($Text, [regex]::Escape($Find))).Count
    if ($n -ne $Count) { throw "expected $Count of '$Find' in the script, found $n" }
    return $Text.Replace($Find, $With)
}

function Patch-Script([string]$Text, [string]$Marker, [string]$Overrides, $Limits) {
    $stop = "`$ErrorActionPreference = 'Stop'"
    $i = $Text.IndexOf($stop)
    if ($i -lt 0) { throw 'no $ErrorActionPreference line' }
    $i += $stop.Length
    $Text = $Text.Substring(0, $i) + "`r`n" + $Prelude + "`r`n" + $Text.Substring($i)
    $Text = Replace-Once $Text $Marker ($Overrides + "`r`n" + $Marker)
    $Text = Replace-Once $Text 'New-Object IO.DriveInfo' 'MockDriveInfo'
    if ($Limits) {
        $Text = Replace-Once $Text '$LogCapBytes = 300MB' ('$LogCapBytes = ' + [long]$Limits.logCap)
        $Text = Replace-Once $Text '$MasLogTailBytes = 30MB' ('$MasLogTailBytes = ' + [long]$Limits.masTail)
        $Text = Replace-Once $Text '$DumpMaxBytes = 100MB' ('$DumpMaxBytes = ' + [long]$Limits.dumpMax)
    }
    return $Text
}

# ---------------------------------------------------------------- scenario setup

function Json-Escape([string]$s) { return $s.Replace('\', '\\') }

function Build-Files($Sc, [string]$Base, [string]$Root) {
    foreach ($f in @($Sc.files)) {
        $rel = [string]$f.path
        if ($rel.StartsWith('root/')) { $p = Join-Path $Root $rel.Substring(5) }
        elseif ($rel -eq 'root') { $p = $Root }
        else { $p = Join-Path $Base $rel }
        $p = $p.Replace('/', '\')
        if ($f.dir) { [void](New-Item -ItemType Directory -Path $p -Force); continue }
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $p) -Force)
        if ($null -ne $f.size) { [IO.File]::WriteAllBytes($p, [Text.Encoding]::ASCII.GetBytes(('x' * [int]$f.size))) }
        elseif ($null -ne $f.b64) { [IO.File]::WriteAllBytes($p, [Convert]::FromBase64String([string]$f.b64)) }
        else { [IO.File]::WriteAllBytes($p, $Utf8.GetBytes([string]$f.text)) }
        $mt = '2026-01-10 08:00:00'
        if ($f.mtime) { $mt = [string]$f.mtime }
        (Get-Item -LiteralPath $p -Force).LastWriteTime = [datetime]::ParseExact($mt, 'yyyy-MM-dd HH:mm:ss', [Globalization.CultureInfo]::InvariantCulture)
        if ($f.hidden) { $it = Get-Item -LiteralPath $p -Force; $it.Attributes = $it.Attributes -bor [IO.FileAttributes]::Hidden }
    }
    if (-not (Test-Path -LiteralPath $Root)) { [void](New-Item -ItemType Directory -Path $Root -Force) }
}

function Run-PS([string]$Script, [string[]]$ArgList, [hashtable]$Vars) {
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $q = @('-NoLogo', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $Script) + $ArgList
    $psi.Arguments = (($q | ForEach-Object { '"' + $_ + '"' }) -join ' ')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = $Utf8
    $psi.StandardErrorEncoding = $Utf8
    foreach ($k in $Vars.Keys) { $psi.EnvironmentVariables[$k] = [string]$Vars[$k] }
    $p = [Diagnostics.Process]::Start($psi)
    $errTask = $p.StandardError.ReadToEndAsync()
    $stdout = $p.StandardOutput.ReadToEnd()
    $p.WaitForExit()
    $stderr = $errTask.Result
    if ($p.ExitCode -ne 0 -or $stderr.Trim()) { throw "$Script failed ($($p.ExitCode)): $stderr`n$stdout" }
    return $stdout
}

function Normalize([string]$Text, [string]$Base) {
    $Text = $Text.Replace($Base, '{{BASE}}')
    $Text = [regex]::Replace($Text, '所在盘：[A-Za-z]:\\', '所在盘：{{DRIVE}}:\')
    $Text = [regex]::Replace($Text, '放在 [A-Za-z]: 盘', '放在 {{DRIVE}}: 盘')
    $Text = $Text.Replace([string]$AdbPort, '{{ADBPORT}}')
    $Text = [regex]::Replace($Text, '(?m)^PowerShell：.*?(\r?)$', 'PowerShell：{{PSVERSION}}$1')
    $Text = [regex]::Replace($Text, '(?m)^(  [^\r\n]+：)[0-9.,]+ 秒(\r?)$', '${1}0.0 秒$2')
    $Text = [regex]::Replace($Text, '(?m)^收集耗时：[0-9.,]+ 秒(\r?)$', '收集耗时：0.0 秒$1')
    $Text = [regex]::Replace($Text, '(?m)^大小：.*?(\r?)$', '大小：{{SIZE}}$1')
    return $Text
}

function Write-Golden([string]$Path, [string]$Text) {
    # A derived scenario reuses byte-identical auxiliary text from its base; the test still compares each entry.
    if ($sc -and $sc.extends -and $Path.StartsWith($collectGold + '\') -and (Split-Path -Leaf $Path) -ne 'summary.txt') {
        $relative = $Path.Substring($collectGold.Length + 1)
        $parent = Join-Path (Join-Path (Join-Path $Out ([string]$sc.extends)) 'collect') $relative
        if ((Test-Path -LiteralPath $parent) -and [IO.File]::ReadAllText($parent, $Utf8) -ceq $Text) { return }
    }
    [void](New-Item -ItemType Directory -Path (Split-Path -Parent $Path) -Force)
    [IO.File]::WriteAllText($Path, $Text, $Utf8)
}

# ---------------------------------------------------------------- run

$rcPath = Join-Path $ScriptDir 'report-config.ps1'
$ciPath = Join-Path $ScriptDir 'collect-info.ps1'
$rcText = [IO.File]::ReadAllText($rcPath, [Text.Encoding]::UTF8)
$ciText = [IO.File]::ReadAllText($ciPath, [Text.Encoding]::UTF8)
$rawAll = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'scenarios.json'), [Text.Encoding]::UTF8)
# Apply the same shallow scenario overrides as scenarioData in the Go tests.
$all = $rawAll | ConvertFrom-Json
function Resolve-Scenario([string]$Name) {
    $sc = $all.$Name
    $merged = [ordered]@{}
    if ($sc.extends) {
        $base = Resolve-Scenario ([string]$sc.extends)
        foreach ($p in $base.PSObject.Properties) { $merged[$p.Name] = $p.Value }
    }
    foreach ($p in $sc.PSObject.Properties) { $merged[$p.Name] = $p.Value }
    [pscustomobject]$merged
}
$resolved = [ordered]@{}
foreach ($p in $all.PSObject.Properties) {
    if ($p.Name -ne 'comment') { $resolved[$p.Name] = Resolve-Scenario $p.Name }
}
$rawAll = $resolved | ConvertTo-Json -Depth 100
$names = @(($rawAll | ConvertFrom-Json).PSObject.Properties | Where-Object { $_.Name -ne 'comment' } | ForEach-Object { $_.Name })
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

# Output is updated per scenario; preserve report-config goldens for -OnlyCollect.
$hashes = @(
    ('{0}  report-config.ps1' -f (Get-FileHash -LiteralPath $rcPath -Algorithm SHA256).Hash.ToLowerInvariant()),
    ('{0}  collect-info.ps1' -f (Get-FileHash -LiteralPath $ciPath -Algorithm SHA256).Hash.ToLowerInvariant())
)
Write-Golden (Join-Path $Out 'scripts.sha256') (($hashes -join "`n") + "`n")

foreach ($name in $names) {
    $base = Join-Path $Work $name
    $first = ($rawAll | ConvertFrom-Json).$name
    $root = Join-Path $base ([string]$first.root)
    $mas = Join-Path $base 'mas'
    $raw = $rawAll
    foreach ($kv in @(@('{{ROOT}}', $root), @('{{MAS}}', $mas), @('{{BASE}}', $base))) {
        $raw = $raw.Replace($kv[0], (Json-Escape $kv[1]))
        $raw = $raw.Replace(($kv[0].TrimEnd('}') + '_J}}'), (Json-Escape (Json-Escape $kv[1])))
    }
    $raw = $raw.Replace('{{ADBPORT}}', [string]$AdbPort)
    $sc = ($raw | ConvertFrom-Json).$name
    [void](New-Item -ItemType Directory -Path $base -Force)
    Build-Files $sc $base $root
    $mock = Join-Path $Work ($name + '-mock.json')
    [IO.File]::WriteAllText($mock, $raw, $Utf8)

    $scripts = Join-Path $Work ($name + '-scripts')
    [void](New-Item -ItemType Directory -Path $scripts -Force)
    $rc = Join-Path $scripts 'report-config.ps1'
    $ci = Join-Path $scripts 'collect-info.ps1'
    [IO.File]::WriteAllText($rc, (Patch-Script $rcText '# ---------------------------------------------------------------- 电脑' $RcOverrides $null), $Utf8Bom)
    [IO.File]::WriteAllText($ci, (Patch-Script $ciText '# ---------------------------------------------------------------- 暂存目录' $CiOverrides $sc.limits), $Utf8Bom)

    $envs = @{ MODAVD_MOCK_FILE = $mock; MODAVD_MOCK_NAME = $name; MODAVD_MOCK_ADBPORT = [string]$AdbPort }
    foreach ($p in $sc.env.PSObject.Properties) {
        $v = [string]$p.Value
        # <dir>\rest: dir by its 8.3 short name
        if ($v -match '^<([^>]+)>(.*)$') {
            $dir = Join-Path $base $Matches[1]
            $rest = $Matches[2]
            [void](New-Item -ItemType Directory -Path $dir -Force)
            $short = (New-Object -ComObject Scripting.FileSystemObject).GetFolder($dir).ShortName
            if (-not $short -or $short -eq (Split-Path -Leaf $dir)) { throw "$dir has no 8.3 short name: -Work must be on a volume that makes them" }
            $v = $short + $rest
        }
        $envs[$p.Name] = Join-Path $base $v
    }
    $envs['TMP'] = $envs['TEMP']
    [void](New-Item -ItemType Directory -Path $envs['TEMP'] -Force)
    $gold = Join-Path $Out $name
    $collectGold = [IO.Path]::GetFullPath((Join-Path $gold 'collect'))
    $outPrefix = [IO.Path]::GetFullPath($Out).TrimEnd('\') + '\'
    if (-not $collectGold.StartsWith($outPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'golden path escaped output directory' }
    if (Test-Path -LiteralPath $collectGold) { Remove-Item -LiteralPath $collectGold -Recurse -Force }

    if (-not $OnlyCollect -and -not $sc.collectOnly) {
        Write-Golden (Join-Path $gold 'report-config.txt') (Normalize (Run-PS $rc @('-Root', $root) $envs) $base)
    }

    $outDir = Join-Path $base 'out'
    $stage = Join-Path $base 'stage'
    $console = Run-PS $ci @('-Root', $root, '-OutDir', $outDir, '-StageDir', $stage) $envs
    Write-Golden (Join-Path $gold 'collect-console.txt') (Normalize $console $base)
    $zipPath = @(Get-ChildItem -LiteralPath $outDir -Filter '*.zip')[0].FullName
    $zip = [IO.Compression.ZipFile]::OpenRead($zipPath)
    try {
        $entries = @($zip.Entries | ForEach-Object { $_.FullName })
        Write-Golden (Join-Path $gold 'collect-entries.txt') (($entries -join "`n") + "`n")
        foreach ($e in $zip.Entries) {
            # what the script writes itself; copied files are the sources' bytes
            if ($e.FullName -ne 'summary.txt' -and $e.Name -ne '_files.txt' -and -not ($e.FullName -like 'live/*.txt')) { continue }
            $s = $e.Open()
            try { $ms = New-Object IO.MemoryStream; $s.CopyTo($ms) } finally { $s.Dispose() }
            $b = $ms.ToArray()
            $bom = ($b.Length -ge 3 -and $b[0] -eq 0xEF -and $b[1] -eq 0xBB -and $b[2] -eq 0xBF)
            $t = $Utf8.GetString($b)
            if ($bom) { $t = $t.Substring(1) }
            $t = Normalize $t $base
            if ($bom) { $t = '{{BOM}}' + $t }
            Write-Golden (Join-Path (Join-Path $gold 'collect') $e.FullName.Replace('/', '\')) $t
        }
    } finally { $zip.Dispose() }
    Write-Host ("{0}: {1} entries" -f $name, $entries.Count)
}
