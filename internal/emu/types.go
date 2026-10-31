//go:build windows

package emu

import (
	"time"

	"modavd/internal/win"
)

// The result types below are the machine-readable contract (CLI --json "data", MCP structuredContent). Field names
// are snake_case. Add fields freely; renaming or removing one needs buildinfo.Protocol bumped.

type Status string

const (
	StatusOffline  Status = "offline"  // no qemu process for this instance
	StatusStarting Status = "starting" // qemu runs, Android has not finished booting (sys.boot_completed != 1)
	StatusOnline   Status = "online"   // booted
)

type Ports struct {
	Console int `json:"console"`
	Adb     int `json:"adb"`
	Grpc    int `json:"grpc"`
}

// Screencap is the shared-memory screenshot channel (MaaFramework AVDExtras, the fake-MuMu DLL for MAA).
type Screencap struct {
	Shm      string `json:"shm"`       // "SHM_videmulator<console>"
	ShmReady bool   `json:"shm_ready"` // the mapping exists right now
}

type InstanceConfig struct {
	MemoryMB        int `json:"memory_mb"` // what the next boot passes as -memory
	CPU             int `json:"cpu"`
	FPS             int `json:"fps"`
	DataPartitionGB int `json:"data_partition_gb"`
	Width           int `json:"width"`
	Height          int `json:"height"`
	DPI             int `json:"dpi"`
}

type InstanceInfo struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`  // mas_<i>
	Title   string `json:"title"` // display name (avd.ini.displayname)
	Status  Status `json:"status"`
	Running bool   `json:"running"` // qemu process exists (MuMuManager is_process_started)
	Booted  bool   `json:"booted"`  // Android finished booting (MuMuManager is_android_started)
	// Busy: "starting" / "stopping" / "bridging" while some modavd process holds that lock of the instance (AUTO-MAS's
	// updater must not swap sdk\emulator then); "" otherwise. "bridging" covers the whole bridge install, including the
	// stops and cold boots in it; callers that do not know a value treat it as busy.
	Busy string `json:"busy"`
	// Bridge (the arm64 execution bridge): "off" (instances.<i>.bridge is false), "pending" (the next start modavd
	// launches installs it), "installed" (the booted guest runs it) or "unknown". Only a booted guest is asked; an
	// instance that is off or still booting and has the bridge's boot marker after its first boot reads unknown, since
	// the marker stays while the bridge is installed and also after a failed install.
	Bridge string `json:"bridge"`
	PID    int    `json:"pid"`
	// StartedAt is the qemu process start, local time RFC 3339 ("2026-10-28T15:04:05+01:00").
	StartedAt string `json:"started_at"`
	// StartedAtMS is the same moment in Unix milliseconds; 0 when not running.
	StartedAtMS int64 `json:"started_at_ms"`
	// Serial is the device on the private adb server (adb -P <adb_server_port> -s <serial>). Scripts that use their own
	// adb server connect to AdbAddress instead.
	Serial      string         `json:"serial"`
	AdbAddress  string         `json:"adb_address"` // "127.0.0.1:<adb port>"
	Ports       Ports          `json:"ports"`
	Screencap   Screencap      `json:"screencap"`
	Config      InstanceConfig `json:"config"`
	Initialized bool           `json:"initialized"` // first-boot setup done (mas-avd.json instances.<i>.initialized)
	Launcher    string         `json:"launcher"`
	Renderer    string         `json:"renderer"`
	// SoftwareRenderer: the first boot found SwiftShader (no usable GPU driver).
	SoftwareRenderer bool   `json:"software_renderer"`
	Path             string `json:"path"` // <root>\avd\mas_<i>.avd
}

// RootInfo describes the root a command worked on.
type RootInfo struct {
	Root                string `json:"root"`
	SDK                 string `json:"sdk"`       // the SDK actually used (sdkRoot or <root>\sdk)
	LocalSDK            bool   `json:"local_sdk"` // mas-avd.json sets sdkRoot
	AdbExe              string `json:"adb_exe"`
	AdbServerPort       int    `json:"adb_server_port"`        // the private server modavd and the emulator use
	ScriptAdbServerPort int    `json:"script_adb_server_port"` // the server scripts should use for these instances
	Version             string `json:"version"`                // "0.35.0"; "" when the emulator's build is unknown
	Build               int    `json:"build"`                  // N of mas-N; 0 when unknown
}

type ListResult struct {
	RootInfo
	Instances []InstanceInfo `json:"instances"` // never null
}

type CreateRequest struct {
	Index           *int // nil = the smallest free index whose ports are free
	Title           string
	MemoryMB        *int
	CPU             *int
	DataPartitionGB *int
	FPS             *int
	// NoBridge: no execution bridge (bridge.go). By default a new instance gets the bridge's boot marker and its
	// first start installs the bridge.
	NoBridge bool
}

type UpdateRequest struct {
	Title    *string
	MemoryMB *int
	CPU      *int
	FPS      *int
	// DataPartitionGB raises the data partition (never lowers it); see avd.UpdateOptions.
	DataPartitionGB *int
}

type UpdateResult struct {
	Instance InstanceInfo   `json:"instance"`
	Applied  map[string]any `json:"applied"` // option name -> new value
	// AppliesNextBoot: the instance is running; the changes take effect when it boots next.
	AppliesNextBoot bool `json:"applies_next_boot"`
}

type DeleteResult struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

type StartOptions struct {
	// Window: boot with the emulator window (default headless, like AUTO-MAS silent mode).
	Window bool
	// Timeout for Android to finish booting; 0 = DefaultBootTimeout.
	Timeout time.Duration
	// Progress, when set, is told about each step (CLI shows nothing extra; MCP sends notifications/progress).
	Progress func(Progress)

	// watchQemu is set by the boot watch (bootwatch.go) after a guest-only reboot of that qemu process: the start adopts
	// exactly it (never launches), waits for no other start or stop, and leaves no further watch.
	watchQemu *win.Process
	// NoBridge: do not install the execution bridge on this boot even when its marker is there and the bridge is not
	// (bridge.go), the way AUTO-MAS boots. Bridge starts with it to uninstall.
	NoBridge bool
	// bridgeLocked: the caller (Bridge) holds the instance's bridge lock, so the install step runs under it instead of
	// taking it (the lock is per handle: a second TryLock in the same process fails).
	bridgeLocked bool
}

type Progress struct {
	Step    string // launch / boot / tune / first_boot / ime / vulkan_fix / done
	Message string // Chinese, for people
	Done    int
	Total   int
}

type StartResult struct {
	Instance InstanceInfo `json:"instance"`
	// Launched: this call started qemu. false = it was already running; the per-boot setup ran again where safe.
	Launched     bool    `json:"launched"`
	BootSeconds  float64 `json:"boot_seconds"`  // from launch to sys.boot_completed (0 when already running)
	TotalSeconds float64 `json:"total_seconds"` // including the setup after boot
	FirstBoot    bool    `json:"first_boot"`    // the one-time setup ran in this call
	LogFile      string  `json:"log_file"`
	LogcatFile   string  `json:"logcat_file"`
	// ZygoteRestarted: the guest Vulkan fix was mounted, which restarts zygote (all apps start over).
	ZygoteRestarted bool `json:"zygote_restarted"`
	// SetupPending: this boot's one-time setup (the Vulkan fix mount) is still owed: an app was in the foreground or a
	// guest job ran, and mounting restarts zygote. A later start of the same boot (before the marker expires) finishes
	// it; otherwise the next boot does.
	SetupPending bool     `json:"setup_pending"`
	Warnings     []string `json:"warnings"` // never null; setup steps that failed (none of them stop the boot)
}

type StopOptions struct {
	// Force: skip the Android shutdown; sync, then kill qemu (AUTO-MAS ForceKillOnClose).
	Force bool
}

type StopResult struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	// Method: already_offline / graceful (Android shut down, qemu exited by itself) / sync_kill (graceful failed:
	// sync, console kill) / console_kill (private adb unusable: console kill without sync) / launching_kill (qemu had
	// only just been launched by a concurrent start, Android not booted, nothing to sync) / force (Force) — with
	// "+terminate" appended when qemu had to be terminated in the end.
	Method   string   `json:"method"`
	Synced   bool     `json:"synced"` // a sync ran before qemu was ended (always true for graceful)
	Seconds  float64  `json:"seconds"`
	Warnings []string `json:"warnings"`
	// PID of the qemu this stop ended; 0 when it was already offline.
	PID int `json:"pid"`
	// Process is that qemu (PID + creation time), for callers in this process that must wait until it is really gone
	// (restart): a qemu that was terminated can take seconds to release its memory, and by then the root-identity
	// checks no longer recognise it (its runtime file and run lock are gone first).
	Process win.Process `json:"-"`
}

// CheckItem is one host check (AUTO-MAS precheck.PrecheckItem). ID: path / components / emulator / acceleration /
// vulkan / disk / memory.
type CheckItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	OK       *bool  `json:"ok"` // null = could not be checked now
	Blocking bool   `json:"blocking"`
	Reason   string `json:"reason"`
	Advice   string `json:"advice"`
}

type CheckOptions struct {
	// MemoryMB to judge host memory against (default avd.DefaultMemoryMB).
	MemoryMB int
	// Refresh: run the acceleration check and the Vulkan probe again instead of using their cached results
	// (AUTO-MAS run_prechecks(refresh=True), the "重新检查" button). Only long-running processes (MCP, the manager)
	// have a cache at all.
	Refresh bool
}

type CheckResult struct {
	RootInfo
	// Ready: no blocking item failed (ok == false and blocking).
	Ready      bool              `json:"ready"`
	Items      []CheckItem       `json:"items"`
	Components []ComponentStatus `json:"components"`
}

// ComponentStatus mirrors root.ComponentStatus for the JSON contract.
type ComponentStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Build     string `json:"build"`
	Installed bool   `json:"installed"`
	Optional  bool   `json:"optional"`
	LocalSDK  bool   `json:"local_sdk"`
}

type ShellResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"` // stdout and stderr merged
}

type InstallResult struct {
	Index   int    `json:"index"`
	APK     string `json:"apk"`
	Package string `json:"package"` // from the APK manifest when readable
	Output  string `json:"output"`  // adb install output ("Success")
}

type ServersResult struct {
	AdbServerPort       int  `json:"adb_server_port"`
	AdbServerUp         bool `json:"adb_server_up"`
	ScriptAdbServerPort int  `json:"script_adb_server_port"`
	ScriptAdbServerUp   bool `json:"script_adb_server_up"`
}

type WindowResult struct {
	Index   int  `json:"index"`
	Visible bool `json:"visible"`
	Windows int  `json:"windows"` // windows shown / hidden; 0 for a headless instance
}
