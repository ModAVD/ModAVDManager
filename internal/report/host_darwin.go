//go:build darwin && arm64 && cgo

package report

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/macos"
)

// macHost adapts native host data to the existing public diagnostics contract.
// Windows-only sources explicitly report unsupported and are never fabricated.
type macHost struct{}

const reportsAvailable = true

func newHost() (Host, error) { return &macHost{}, nil }

func macUnsupported(what string) error { return errs.New(errs.Unsupported, what+"不适用于 macOS") }

func (h *macHost) OS(ctx context.Context) (OSInfo, error) {
	version, err := h.Run(ctx, "/usr/bin/sw_vers", []string{"-productVersion"}, 5*time.Second)
	if err != nil || version.TimedOut || version.ExitCode != 0 {
		return OSInfo{}, fmt.Errorf("读不到 macOS 版本")
	}
	build, err := macos.SystemString("kern.osversion")
	if err != nil {
		return OSInfo{}, err
	}
	total, err := macos.SystemNumber("hw.memsize")
	if err != nil {
		return OSInfo{}, err
	}
	free, err := macos.AvailableMemoryMB()
	if err != nil {
		return OSInfo{}, err
	}
	return OSInfo{Caption: "macOS", Version: strings.TrimSpace(string(version.Stdout)), BuildNumber: build,
		OSArchitecture: runtime.GOARCH, TotalVisibleKB: float64(total) / 1024, FreePhysicalKB: float64(free) * 1024}, nil
}

func (h *macHost) Processors(context.Context) ([]Processor, error) {
	name, err := macos.SystemString("machdep.cpu.brand_string")
	if err != nil {
		return nil, err
	}
	cores, err := macos.SystemNumber("hw.physicalcpu")
	if err != nil {
		return nil, err
	}
	threads, err := macos.SystemNumber("hw.logicalcpu")
	if err != nil {
		return nil, err
	}
	c, t := int64(cores), int64(threads)
	return []Processor{{Name: name, Cores: &c, Threads: &t}}, nil
}

func (h *macHost) HVFSupport(context.Context) (bool, error) {
	value, err := macos.SystemNumber("kern.hv_support")
	return value == 1, err
}

func (*macHost) ComputerSystem(context.Context) (ComputerSystem, error) {
	return ComputerSystem{}, macUnsupported("Windows HypervisorPresent 查询")
}

func (h *macHost) VideoControllers(ctx context.Context) ([]VideoController, error) {
	devices, err := macos.MetalDevices(ctx)
	var result []VideoController
	for _, name := range devices {
		result = append(result, VideoController{Name: name, DriverVersion: "macOS / Metal"})
	}
	return result, err
}

func (*macHost) OptionalFeatures(context.Context) ([]OptionalFeature, error) {
	return nil, macUnsupported("Windows 可选功能")
}
func (*macHost) Antivirus(context.Context) ([]Antivirus, error) {
	return nil, macUnsupported("Windows 安全中心")
}
func (*macHost) DiskKind(context.Context, string) (string, string, error) {
	return "", "", macUnsupported("Windows 磁盘总线查询")
}
func (*macHost) AppEvents(context.Context) ([]AppEvent, error) {
	return nil, macUnsupported("Windows 事件日志")
}
func (*macHost) CurrentVersion() (CurrentVersion, error) {
	return CurrentVersion{}, macUnsupported("Windows 注册表")
}
func (*macHost) UninstallEntries() ([]UninstallEntry, error) {
	return nil, macUnsupported("Windows 卸载注册表")
}
func (*macHost) ProductVersion(string) (string, error) {
	return "", macUnsupported("Windows 版本资源")
}

func (*macHost) Processes(match func(string) bool) ([]Process, error) {
	processes, err := host.ListProcesses()
	var out []Process
	for _, p := range processes {
		if !match(p.Name) {
			continue
		}
		// Do not expose other processes' command lines or environments to reports.
		out = append(out, Process{PID: p.PID, Name: p.Name, Started: p.StartedAt()})
	}
	return out, err
}

func (*macHost) Listeners() ([]Listener, error) {
	listeners, err := host.Listeners()
	var out []Listener
	for _, l := range listeners {
		out = append(out, Listener{Port: l.Port, PID: l.PID})
	}
	return out, err
}

func (*macHost) Run(ctx context.Context, exe string, args []string, timeout time.Duration) (RunResult, error) {
	r, err := macos.RunDiagnostic(ctx, exe, args, timeout)
	return RunResult{TimedOut: r.TimedOut, ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}, err
}

func (*macHost) Drive(path string) (Drive, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Drive{}, err
	}
	var format []byte
	for _, b := range stat.Fstypename {
		if b == 0 {
			break
		}
		format = append(format, byte(b))
	}
	return Drive{Name: "实例所在卷", Free: stat.Bavail * uint64(stat.Bsize), Total: stat.Blocks * uint64(stat.Bsize), Format: string(format)}, nil
}

func desktopDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Desktop"), nil
}
func copyClipboard(text string) error { return macos.CopyClipboard(text) }
func openExplorer(path string) error  { return macos.RevealInFinder(path) }
func isHidden(info fs.FileInfo) bool  { return strings.HasPrefix(info.Name(), ".") }
func longPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return path
}
