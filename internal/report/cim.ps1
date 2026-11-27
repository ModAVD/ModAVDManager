# The WMI part of modavd report-config / collect-info (internal/report/host_windows.go runs it with Windows
# PowerShell 5.1). The expressions are those of the package scripts report-config.ps1 and collect-info.ps1, so the
# values are what the scripts see. Prints one line of JSON: per part in $env:MODAVD_CIM_PARTS its values, or
# <part>Err with the message when the part failed. Read-only.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = New-Object Text.UTF8Encoding $false
$parts = @(([string]$env:MODAVD_CIM_PARTS) -split ',')
$r = @{}
function Part([string]$Name, [scriptblock]$Body) {
    if ($parts -notcontains $Name) { return }
    try { $r[$Name] = @(& $Body) } catch { $r[$Name + 'Err'] = $_.Exception.Message }
}

Part 'os' {
    $o = Get-CimInstance Win32_OperatingSystem
    @{ caption = [string]$o.Caption; version = [string]$o.Version; build = [string]$o.BuildNumber
        arch = [string]$o.OSArchitecture; totalKB = [double]$o.TotalVisibleMemorySize; freeKB = [double]$o.FreePhysicalMemory }
}
Part 'cpu' {
    foreach ($c in @(Get-CimInstance Win32_Processor)) {
        @{ name = [string]$c.Name; cores = $c.NumberOfCores; threads = $c.NumberOfLogicalProcessors; vt = $c.VirtualizationFirmwareEnabled }
    }
}
Part 'cs' {
    @{ hypervisor = (Get-CimInstance Win32_ComputerSystem).HypervisorPresent }
}
Part 'gpu' {
    foreach ($g in @(Get-CimInstance Win32_VideoController)) {
        $dd = ''
        if ($g.DriverDate) { $dd = ([datetime]$g.DriverDate).ToString('yyyy-MM-dd') }
        @{ name = [string]$g.Name; driver = [string]$g.DriverVersion; pnp = [string]$g.PNPDeviceID; date = $dd }
    }
}
Part 'features' {
    foreach ($f in @(Get-CimInstance Win32_OptionalFeature -Filter "Name='HypervisorPlatform' or Name='VirtualMachinePlatform' or Name='Microsoft-Hyper-V-All'")) {
        @{ name = [string]$f.Name; state = $f.InstallState }
    }
}
Part 'av' {
    foreach ($a in @(Get-CimInstance -Namespace 'root/SecurityCenter2' -ClassName AntiVirusProduct)) {
        @{ name = [string]$a.displayName; state = [int]$a.productState }
    }
}
Part 'disk' {
    $disk = Get-Partition -DriveLetter ([string]$env:MODAVD_CIM_LETTER) -ErrorAction Stop | Get-Disk -ErrorAction Stop
    $pd = Get-PhysicalDisk -ErrorAction Stop | Where-Object { [string]$_.DeviceId -eq [string]$disk.Number } | Select-Object -First 1
    $mt = ''
    if ($pd) { $mt = [string]$pd.MediaType }
    @{ bus = [string]$disk.BusType; media = $mt }
}
Part 'events' {
    $ev = @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; Id = 1000, 1001, 1002; StartTime = (Get-Date).AddDays(-7) } -MaxEvents 2000 -ErrorAction SilentlyContinue)
    # the filter of collect-info.ps1 (applied again by the caller), so that only the hits are serialized
    foreach ($e in @($ev | Where-Object { $_.Message -match 'qemu|emulator|\badb\b|AUTO-MAS' } | Select-Object -First 30)) {
        @{ time = $e.TimeCreated.ToString('yyyy-MM-dd HH:mm:ss'); id = $e.Id; provider = [string]$e.ProviderName; message = [string]$e.Message }
    }
}

ConvertTo-Json -InputObject $r -Compress -Depth 5
