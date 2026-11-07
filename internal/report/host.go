package report

import (
	"context"
	"time"

	"modavd/internal/root"
)

// Host is the diagnostic source contract shared by the CLI and manager. Windows
// maps the package scripts' sources directly. macOS supplies native hardware,
// process and volume data and rejects Windows-only sources; its report layout
// uses the platform's privacy and ownership rules.
type Host interface {
	WMI
	Registry
	ProcessTable
	Runner
	// Drive is [IO.DriveInfo] of a drive root such as `E:\`.
	Drive(root string) (Drive, error)
}

// nativeReporter supplies a platform's report layout when its diagnostics do
// not map to the Windows package scripts. The public Host contract stays stable.
type nativeReporter interface {
	configuration(context.Context, *root.Root) *ConfigResult
	collect(context.Context, *root.Root, CollectOptions) (*CollectResult, error)
}

// WMI is what the scripts read with Get-CimInstance, Get-Partition / Get-Disk / Get-PhysicalDisk and Get-WinEvent.
// Values are passed on as the cmdlets return them (nil where the property is null); formatting is the caller's.
type WMI interface {
	OS(ctx context.Context) (OSInfo, error)                                     // Win32_OperatingSystem
	Processors(ctx context.Context) ([]Processor, error)                        // Win32_Processor
	ComputerSystem(ctx context.Context) (ComputerSystem, error)                 // Win32_ComputerSystem
	VideoControllers(ctx context.Context) ([]VideoController, error)            // Win32_VideoController, all of them
	OptionalFeatures(ctx context.Context) ([]OptionalFeature, error)            // the three features collect-info asks for
	Antivirus(ctx context.Context) ([]Antivirus, error)                         // root/SecurityCenter2 AntiVirusProduct
	DiskKind(ctx context.Context, letter string) (bus, media string, err error) // the disk under a drive letter
	// AppEvents is the Application log of the last 7 days, IDs 1000 / 1001 / 1002, at most 2000, newest first.
	AppEvents(ctx context.Context) ([]AppEvent, error)
}

// Registry is what the scripts read with Get-ItemProperty.
type Registry interface {
	// CurrentVersion is HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion.
	CurrentVersion() (CurrentVersion, error)
	// UninstallEntries are the subkeys of the three Uninstall keys collect-info searches (HKLM, HKLM WOW6432Node,
	// HKCU), in that order and in registry order within each.
	UninstallEntries() ([]UninstallEntry, error)
}

// ProcessTable is the process list (Win32_Process in the scripts) and the TCP listeners (Get-NetTCPConnection).
type ProcessTable interface {
	// Processes are the processes whose executable name match accepts, with command line and path read (empty
	// when unreadable).
	Processes(match func(name string) bool) ([]Process, error)
	// Listeners are the listening TCP sockets, IPv4 and IPv6, in the order Get-NetTCPConnection lists them.
	Listeners() ([]Listener, error)
	// ProductVersion is the ProductVersion of an executable's version resource ((Get-Item).VersionInfo).
	ProductVersion(path string) (string, error)
}

// Runner starts a program (emulator.exe -accel-check, emulator-check.exe accel).
type Runner interface {
	// Run starts exe with args in exe's directory, without a window, and kills it after timeout. Only a failure to
	// start is an error; a timeout is RunResult.TimedOut.
	Run(ctx context.Context, exe string, args []string, timeout time.Duration) (RunResult, error)
}

type OSInfo struct {
	Caption        string
	Version        string
	BuildNumber    string
	OSArchitecture string
	// TotalVisibleKB and FreePhysicalKB are TotalVisibleMemorySize and FreePhysicalMemory (KB).
	TotalVisibleKB float64
	FreePhysicalKB float64
}

type Processor struct {
	Name    string
	Cores   *int64 // NumberOfCores
	Threads *int64 // NumberOfLogicalProcessors
	// VirtualizationFirmwareEnabled; nil when the property is null.
	VirtFirmware *bool
}

type ComputerSystem struct {
	HypervisorPresent *bool
}

type VideoController struct {
	Name          string
	DriverVersion string
	PNPDeviceID   string
	// DriverDate is ([datetime]$_.DriverDate).ToString('yyyy-MM-dd'), "" when the property is null.
	DriverDate string
}

type OptionalFeature struct {
	Name         string
	InstallState *int64
}

type Antivirus struct {
	DisplayName  string
	ProductState int64
}

// AppEvent is one Get-WinEvent record.
type AppEvent struct {
	Time     string // TimeCreated as yyyy-MM-dd HH:mm:ss
	ID       int
	Provider string
	Message  string
}

type Drive struct {
	Name   string // the root, e.g. `E:\`
	Free   uint64 // AvailableFreeSpace
	Total  uint64 // TotalSize
	Format string // DriveFormat (NTFS ...)
}

// CurrentVersion holds the values as text; "" where a value is missing (UBR, a DWORD, in decimal).
type CurrentVersion struct {
	DisplayVersion string
	ReleaseID      string
	CurrentBuild   string
	UBR            string
}

type UninstallEntry struct {
	DisplayName     string
	InstallLocation string
	DisplayVersion  string
}

type Process struct {
	PID         uint32
	Name        string
	Started     time.Time // zero when unknown
	CommandLine string    // "" when unreadable
	ExePath     string    // "" when unreadable
}

type Listener struct {
	Addr string // LocalAddress: 127.0.0.1, ::1, 0.0.0.0 ...
	Port int
	PID  uint32
}

type RunResult struct {
	TimedOut bool
	ExitCode int // meaningless when TimedOut
	Stdout   []byte
	Stderr   []byte
}
