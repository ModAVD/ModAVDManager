//go:build windows

package report

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"modavd/internal/errs"
	"modavd/internal/win"
)

// cimScript asks WMI the way the package scripts do (see cim.ps1).
//
//go:embed cim.ps1
var cimScript string

// cimTimeout bounds one run of cim.ps1, i.e. one part (the scripts call Get-CimInstance without a limit; a stuck WMI
// service must not hang the command).
const cimTimeout = 60 * time.Second

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	moduser32   = syscall.NewLazyDLL("user32.dll")
	modshell32  = syscall.NewLazyDLL("shell32.dll")
	modntdll    = syscall.NewLazyDLL("ntdll.dll")
	modiphlpapi = syscall.NewLazyDLL("iphlpapi.dll")
	// version.dll is not on the syscall package's list of system DLLs: loaded by its full path, never from the
	// executable's directory.
	modversion = syscall.NewLazyDLL(windowsDir() + `\System32\version.dll`)

	procGetWindowsDirectoryW      = modkernel32.NewProc("GetWindowsDirectoryW")
	procGetDiskFreeSpaceExW       = modkernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumeInformationW     = modkernel32.NewProc("GetVolumeInformationW")
	procExpandEnvironmentStringsW = modkernel32.NewProc("ExpandEnvironmentStringsW")
	procGlobalAlloc               = modkernel32.NewProc("GlobalAlloc")
	procGlobalFree                = modkernel32.NewProc("GlobalFree")
	procGlobalLock                = modkernel32.NewProc("GlobalLock")
	procGlobalUnlock              = modkernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory             = modkernel32.NewProc("RtlMoveMemory")
	procReadProcessMemory         = modkernel32.NewProc("ReadProcessMemory")
	procOpenClipboard             = moduser32.NewProc("OpenClipboard")
	procCloseClipboard            = moduser32.NewProc("CloseClipboard")
	procEmptyClipboard            = moduser32.NewProc("EmptyClipboard")
	procSetClipboardData          = moduser32.NewProc("SetClipboardData")
	procSHGetFolderPathW          = modshell32.NewProc("SHGetFolderPathW")
	procNtQueryInformationProcess = modntdll.NewProc("NtQueryInformationProcess")
	procGetExtendedTcpTable       = modiphlpapi.NewProc("GetExtendedTcpTable")
	procGetFileVersionInfoSizeW   = modversion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW       = modversion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW            = modversion.NewProc("VerQueryValueW")
)

// windowsDir is %SystemRoot% as the system reports it (never a relative path).
func windowsDir() string {
	buf := make([]uint16, 260)
	if procGetWindowsDirectoryW.Find() == nil {
		if n, _, _ := procGetWindowsDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 && int(n) < len(buf) {
			return syscall.UTF16ToString(buf[:n])
		}
	}
	return `C:\Windows`
}

// winHost is Host on this computer. The WMI parts come from cim.ps1, one process per part (and drive letter), each
// on first use; see part.
type winHost struct {
	mu  sync.Mutex
	cim map[string]*cimRun // by part|letter
}

type cimRun struct {
	once sync.Once
	data map[string]json.RawMessage
	err  error
}

func newHost() (Host, error) {
	return &winHost{cim: map[string]*cimRun{}}, nil
}

// cimRunner is runCIM; tests put a stand-in here.
var cimRunner = runCIM

// part runs the cim.ps1 part name (once per part and letter, on first use) and decodes its answer into v (a slice).
// Every part gets a process and a cimTimeout of its own: a slow class (SecurityCenter2 waiting for an antivirus, a
// stuck WMI provider) loses only its own rows, not every WMI line of the report. A part nobody asks for never runs
// (report-config needs three of collect-info's six), and none outlives the command.
func (h *winHost) part(ctx context.Context, letter, name string, v any) error {
	h.mu.Lock()
	key := name + "|" + letter
	run := h.cim[key]
	if run == nil {
		run = &cimRun{}
		h.cim[key] = run
	}
	h.mu.Unlock()
	run.once.Do(func() { run.data, run.err = cimRunner(ctx, name, letter) })
	if run.err != nil {
		return run.err
	}
	if raw, ok := run.data[name+"Err"]; ok {
		var msg string
		_ = json.Unmarshal(raw, &msg)
		return errors.New(msg)
	}
	raw, ok := run.data[name]
	if !ok {
		return fmt.Errorf("WMI 没有返回 %s", name)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("读不懂 WMI 返回的 %s：%v", name, err)
	}
	return nil
}

// runCIM runs cim.ps1 for one part with Windows PowerShell 5.1 (hidden, no profile, stdin NUL) and parses its JSON
// line.
func runCIM(ctx context.Context, part, letter string) (map[string]json.RawMessage, error) {
	ps := filepath.Join(windowsDir(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	u := utf16.Encode([]rune(cimScript))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	ctx, cancel := context.WithTimeout(ctx, cimTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-InputFormat", "None",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), "MODAVD_CIM_PARTS="+part, "MODAVD_CIM_LETTER="+letter)
	cmd.Dir = filepath.Dir(ps)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 3 * time.Second
	win.HideConsoleCmd(cmd)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("WMI 查询（%s）%.0f 秒没有结果", part, cimTimeout.Seconds())
	}
	out := bytes.TrimSpace(bytes.TrimPrefix(stdout.Bytes(), []byte("\xef\xbb\xbf")))
	var data map[string]json.RawMessage
	if jerr := json.Unmarshal(out, &data); jerr != nil {
		if err != nil {
			return nil, fmt.Errorf("Windows PowerShell 运行失败：%v", err)
		}
		return nil, fmt.Errorf("读不懂 Windows PowerShell 的输出：%v", jerr)
	}
	return data, nil
}

// first is the first item of a part h.part decoded. The part runs before first is called: Go leaves unspecified
// whether the items argument of first(raw, h.part(..., &raw), ...) is read before or after the call fills raw.
func first[T any](items []T, err error, what string) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	if len(items) == 0 {
		return zero, fmt.Errorf("WMI 没有返回 %s", what)
	}
	return items[0], nil
}

func (h *winHost) OS(ctx context.Context) (OSInfo, error) {
	var raw []struct {
		Caption string  `json:"caption"`
		Version string  `json:"version"`
		Build   string  `json:"build"`
		Arch    string  `json:"arch"`
		TotalKB float64 `json:"totalKB"`
		FreeKB  float64 `json:"freeKB"`
	}
	err := h.part(ctx, "", "os", &raw)
	o, err := first(raw, err, "Win32_OperatingSystem")
	return OSInfo{Caption: o.Caption, Version: o.Version, BuildNumber: o.Build, OSArchitecture: o.Arch,
		TotalVisibleKB: o.TotalKB, FreePhysicalKB: o.FreeKB}, err
}

func (h *winHost) Processors(ctx context.Context) ([]Processor, error) {
	var raw []struct {
		Name    string `json:"name"`
		Cores   *int64 `json:"cores"`
		Threads *int64 `json:"threads"`
		VT      *bool  `json:"vt"`
	}
	if err := h.part(ctx, "", "cpu", &raw); err != nil {
		return nil, err
	}
	out := []Processor{}
	for _, p := range raw {
		out = append(out, Processor{Name: p.Name, Cores: p.Cores, Threads: p.Threads, VirtFirmware: p.VT})
	}
	return out, nil
}

func (h *winHost) ComputerSystem(ctx context.Context) (ComputerSystem, error) {
	var raw []struct {
		Hypervisor *bool `json:"hypervisor"`
	}
	err := h.part(ctx, "", "cs", &raw)
	cs, err := first(raw, err, "Win32_ComputerSystem")
	return ComputerSystem{HypervisorPresent: cs.Hypervisor}, err
}

func (h *winHost) VideoControllers(ctx context.Context) ([]VideoController, error) {
	var raw []struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
		PNP    string `json:"pnp"`
		Date   string `json:"date"`
	}
	if err := h.part(ctx, "", "gpu", &raw); err != nil {
		return nil, err
	}
	out := []VideoController{}
	for _, g := range raw {
		out = append(out, VideoController{Name: g.Name, DriverVersion: g.Driver, PNPDeviceID: g.PNP, DriverDate: g.Date})
	}
	return out, nil
}

func (h *winHost) OptionalFeatures(ctx context.Context) ([]OptionalFeature, error) {
	var raw []struct {
		Name  string `json:"name"`
		State *int64 `json:"state"`
	}
	if err := h.part(ctx, "", "features", &raw); err != nil {
		return nil, err
	}
	out := []OptionalFeature{}
	for _, f := range raw {
		out = append(out, OptionalFeature{Name: f.Name, InstallState: f.State})
	}
	return out, nil
}

func (h *winHost) Antivirus(ctx context.Context) ([]Antivirus, error) {
	var raw []struct {
		Name  string `json:"name"`
		State int64  `json:"state"`
	}
	if err := h.part(ctx, "", "av", &raw); err != nil {
		return nil, err
	}
	out := []Antivirus{}
	for _, a := range raw {
		out = append(out, Antivirus{DisplayName: a.Name, ProductState: a.State})
	}
	return out, nil
}

func (h *winHost) DiskKind(ctx context.Context, letter string) (string, string, error) {
	var raw []struct {
		Bus   string `json:"bus"`
		Media string `json:"media"`
	}
	err := h.part(ctx, letter, "disk", &raw)
	d, err := first(raw, err, "磁盘")
	return d.Bus, d.Media, err
}

func (h *winHost) AppEvents(ctx context.Context) ([]AppEvent, error) {
	var raw []struct {
		Time     string `json:"time"`
		ID       int    `json:"id"`
		Provider string `json:"provider"`
		Message  string `json:"message"`
	}
	if err := h.part(ctx, "", "events", &raw); err != nil {
		return nil, err
	}
	out := []AppEvent{}
	for _, e := range raw {
		out = append(out, AppEvent{Time: e.Time, ID: e.ID, Provider: e.Provider, Message: e.Message})
	}
	return out, nil
}

// ---- drive -------------------------------------------------------------------------------------------------------

func (h *winHost) Drive(rootPath string) (Drive, error) {
	if len(rootPath) != 3 || rootPath[1] != ':' || rootPath[2] != '\\' {
		return Drive{}, fmt.Errorf("%s 不是盘符的根目录", rootPath)
	}
	p, err := syscall.UTF16PtrFromString(rootPath)
	if err != nil {
		return Drive{}, err
	}
	var free, total, totalFree uint64
	if r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); r == 0 {
		return Drive{}, e
	}
	fsName := make([]uint16, 64)
	if r, _, e := procGetVolumeInformationW.Call(uintptr(unsafe.Pointer(p)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&fsName[0])), uintptr(len(fsName))); r == 0 {
		return Drive{}, e
	}
	return Drive{Name: rootPath, Free: free, Total: total, Format: syscall.UTF16ToString(fsName)}, nil
}

// ---- registry ----------------------------------------------------------------------------------------------------

const (
	regSZ       = 1
	regExpandSZ = 2
	regDWORD    = 4
	regMultiSZ  = 7
	regQWORD    = 11
)

func openKey(root syscall.Handle, path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var k syscall.Handle
	if err := syscall.RegOpenKeyEx(root, p, 0, syscall.KEY_READ, &k); err != nil {
		return 0, err
	}
	return k, nil
}

// regValue is one value as Get-ItemProperty shows it: strings (REG_EXPAND_SZ expanded, REG_MULTI_SZ joined with
// spaces as [string] does), DWORD / QWORD in decimal; ok=false when the value is missing or of another type.
func regValue(k syscall.Handle, name string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", false
	}
	var typ, n uint32
	if syscall.RegQueryValueEx(k, p, nil, &typ, nil, &n) != nil {
		return "", false
	}
	buf := make([]uint16, n/2+2)
	n = uint32(len(buf) * 2)
	if syscall.RegQueryValueEx(k, p, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return "", false
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), n)
	switch typ {
	case regSZ:
		return syscall.UTF16ToString(buf[:n/2]), true
	case regExpandSZ:
		return expandEnv(syscall.UTF16ToString(buf[:n/2])), true
	case regMultiSZ:
		var parts []string
		for _, s := range strings.Split(string(utf16.Decode(buf[:n/2])), "\x00") {
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " "), true
	case regDWORD:
		if len(raw) >= 4 {
			return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(raw)), 10), true
		}
	case regQWORD:
		if len(raw) >= 8 {
			return strconv.FormatUint(binary.LittleEndian.Uint64(raw), 10), true
		}
	}
	return "", false
}

func expandEnv(s string) string {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil || procExpandEnvironmentStringsW.Find() != nil {
		return s
	}
	buf := make([]uint16, 1024)
	for range 3 {
		n, _, _ := procExpandEnvironmentStringsW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return s
		}
		if int(n) <= len(buf) {
			return syscall.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
	return s
}

func (h *winHost) CurrentVersion() (CurrentVersion, error) {
	k, err := openKey(syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`)
	if err != nil {
		return CurrentVersion{}, err
	}
	defer syscall.RegCloseKey(k)
	var cv CurrentVersion
	cv.DisplayVersion, _ = regValue(k, "DisplayVersion")
	cv.ReleaseID, _ = regValue(k, "ReleaseId")
	cv.CurrentBuild, _ = regValue(k, "CurrentBuild")
	cv.UBR, _ = regValue(k, "UBR")
	return cv, nil
}

var uninstallKeys = []struct {
	root syscall.Handle
	path string
}{
	{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
	{syscall.HKEY_CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
}

func (h *winHost) UninstallEntries() ([]UninstallEntry, error) {
	var out []UninstallEntry
	for _, u := range uninstallKeys {
		k, err := openKey(u.root, u.path)
		if err != nil {
			continue // -ErrorAction SilentlyContinue
		}
		for i := uint32(0); ; i++ {
			name := make([]uint16, 256)
			n := uint32(len(name))
			if syscall.RegEnumKeyEx(k, i, &name[0], &n, nil, nil, nil, nil) != nil {
				break
			}
			sub, err := openKey(k, syscall.UTF16ToString(name[:n]))
			if err != nil {
				continue
			}
			var e UninstallEntry
			e.DisplayName, _ = regValue(sub, "DisplayName")
			e.InstallLocation, _ = regValue(sub, "InstallLocation")
			e.DisplayVersion, _ = regValue(sub, "DisplayVersion")
			syscall.RegCloseKey(sub)
			out = append(out, e)
		}
		syscall.RegCloseKey(k)
	}
	return out, nil
}

// ---- processes and listeners -------------------------------------------------------------------------------------

const (
	processQueryLimitedInformation = 0x1000
	processCommandLineInformation  = 60
)

func (h *winHost) Processes(match func(name string) bool) ([]Process, error) {
	procs, err := win.ListProcesses()
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, p := range procs {
		if !match(p.Name) {
			continue
		}
		q := Process{PID: p.PID, Name: p.Name, Started: p.StartedAt()}
		q.CommandLine, _ = rawCommandLine(p.PID)
		// the path it was started with, as Win32_Process.ExecutablePath shows it (<root>\sdk\platform-tools\adb.exe
		// says which root's server it is); else the image's own path, junctions resolved (an elevated process)
		if path, err := pebImagePath(p.PID); err == nil && path != "" {
			q.ExePath = path
		} else {
			q.ExePath, _ = p.ImagePath()
		}
		out = append(out, q)
	}
	return out, nil
}

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

// rawCommandLine is the command line string of pid (ProcessCommandLineInformation), as Win32_Process.CommandLine
// shows it; an error when it cannot be read (another user's or an elevated process).
func rawCommandLine(pid uint32) (string, error) {
	if err := procNtQueryInformationProcess.Find(); err != nil {
		return "", err
	}
	hp, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(hp)
	size := uint32(4096)
	for range 6 {
		buf := make([]uint64, (size+7)/8)
		size = uint32(len(buf) * 8)
		var ret uint32
		st, _, _ := procNtQueryInformationProcess.Call(uintptr(hp), processCommandLineInformation,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
		switch uint32(st) {
		case 0:
			us := (*unicodeString)(unsafe.Pointer(&buf[0]))
			if us.Length == 0 || us.Buffer == nil {
				return "", nil
			}
			return syscall.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2)), nil
		case 0xC0000004, 0x80000005, 0xC0000023: // info length mismatch, buffer overflow, buffer too small
			if ret <= size {
				ret = size * 2
			}
			size = ret
		default:
			return "", fmt.Errorf("NTSTATUS 0x%08X", uint32(st))
		}
	}
	return "", errors.New("命令行一直在变长")
}

const (
	processQueryInformation = 0x0400
	processVMRead           = 0x0010
	processBasicInformation = 0
)

// pebImagePath is ProcessParameters->ImagePathName in the PEB of pid (64-bit layout): the path the process was
// started with, what WMI reads for Win32_Process.ExecutablePath. It needs PROCESS_VM_READ, so it fails for an
// elevated process when we are not, as WMI does.
func pebImagePath(pid uint32) (string, error) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return "", errors.New("只读 64 位的 PEB")
	}
	if err := procReadProcessMemory.Find(); err != nil {
		return "", err
	}
	hp, err := syscall.OpenProcess(processQueryInformation|processVMRead, false, pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(hp)
	var pbi [6]uintptr // PROCESS_BASIC_INFORMATION: PebBaseAddress is the second field
	var ret uint32
	if st, _, _ := procNtQueryInformationProcess.Call(uintptr(hp), processBasicInformation, uintptr(unsafe.Pointer(&pbi[0])),
		unsafe.Sizeof(pbi), uintptr(unsafe.Pointer(&ret))); st != 0 {
		return "", fmt.Errorf("NTSTATUS 0x%08X", uint32(st))
	}
	read := func(addr uintptr, n int) ([]byte, error) {
		b := make([]byte, n)
		var got uintptr
		if r, _, e := procReadProcessMemory.Call(uintptr(hp), addr, uintptr(unsafe.Pointer(&b[0])), uintptr(n),
			uintptr(unsafe.Pointer(&got))); r == 0 {
			return nil, e
		}
		if got != uintptr(n) {
			return nil, errors.New("进程内存读不全")
		}
		return b, nil
	}
	p, err := read(pbi[1]+0x20, 8) // PEB.ProcessParameters
	if err != nil {
		return "", err
	}
	us, err := read(uintptr(binary.LittleEndian.Uint64(p))+0x60, 16) // RTL_USER_PROCESS_PARAMETERS.ImagePathName
	if err != nil {
		return "", err
	}
	n, buf := binary.LittleEndian.Uint16(us), uintptr(binary.LittleEndian.Uint64(us[8:]))
	if n == 0 || buf == 0 {
		return "", nil
	}
	b, err := read(buf, int(n&^1))
	if err != nil {
		return "", err
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u)), nil
}

const (
	afInet                   = 2
	afInet6                  = 23
	tcpTableOwnerPIDListener = 3
	errorInsufficientBuffer  = 122
)

// Listeners reads GetExtendedTcpTable (IPv6, then IPv4) and lists the rows in the order Get-NetTCPConnection does
// (measured: IPv6 first, each by port then address, descending).
func (h *winHost) Listeners() ([]Listener, error) {
	var out []Listener
	for _, af := range []uintptr{afInet6, afInet} {
		buf, err := tcpTable(af)
		if err != nil {
			if af == afInet6 {
				continue // no IPv6 stack
			}
			return nil, errs.Wrap(errs.Internal, err, "读不到系统的 TCP 监听表："+err.Error())
		}
		rowSize, addrOff, addrLen, portOff, pidOff := 24, 4, 4, 8, 20
		if af == afInet6 {
			rowSize, addrOff, addrLen, portOff, pidOff = 56, 0, 16, 20, 52
		}
		var rows []Listener
		if len(buf) >= 4 {
			n := int(binary.LittleEndian.Uint32(buf))
			for i := 0; i < n; i++ {
				row := 4 + i*rowSize
				if row+rowSize > len(buf) {
					break
				}
				ip := net.IP(append([]byte(nil), buf[row+addrOff:row+addrOff+addrLen]...))
				v := binary.LittleEndian.Uint32(buf[row+portOff:])
				rows = append(rows, Listener{Addr: ip.String(), Port: int(v&0xff)<<8 | int(v>>8&0xff),
					PID: binary.LittleEndian.Uint32(buf[row+pidOff:])})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Port != rows[j].Port {
				return rows[i].Port > rows[j].Port
			}
			return bytes.Compare(net.ParseIP(rows[i].Addr), net.ParseIP(rows[j].Addr)) > 0
		})
		out = append(out, rows...)
	}
	return out, nil
}

func tcpTable(af uintptr) ([]byte, error) {
	if err := procGetExtendedTcpTable.Find(); err != nil {
		return nil, err
	}
	size := uint32(16 << 10)
	for range 8 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af,
			tcpTableOwnerPIDListener, 0)
		switch r {
		case 0:
			return buf, nil
		case errorInsufficientBuffer:
			size += 4 << 10
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, syscall.Errno(errorInsufficientBuffer)
}

// ProductVersion reads the version resource like .NET's FileVersionInfo: the first translation, then the US English
// ones; "" when the file has none.
func (h *winHost) ProductVersion(path string) (string, error) {
	if err := procGetFileVersionInfoSizeW.Find(); err != nil {
		return "", err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var handle uint32
	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&handle)))
	if size == 0 {
		return "", nil
	}
	data := make([]byte, size)
	if r, _, e := procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(p)), 0, size, uintptr(unsafe.Pointer(&data[0]))); r == 0 {
		return "", e
	}
	// VerQueryValueW points into data: the answer is read from data at that offset
	query := func(sub string) []byte {
		q, _ := syscall.UTF16PtrFromString(sub)
		var ptr uintptr
		var n uint32
		if r, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(q)),
			uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&n))); r == 0 || ptr == 0 {
			return nil
		}
		off := ptr - uintptr(unsafe.Pointer(&data[0]))
		if off >= uintptr(len(data)) {
			return nil
		}
		return data[off:]
	}
	var langs []string
	if t := query(`\VarFileInfo\Translation`); len(t) >= 4 {
		langs = append(langs, fmt.Sprintf("%04x%04x", binary.LittleEndian.Uint16(t), binary.LittleEndian.Uint16(t[2:])))
	}
	langs = append(langs, "040904b0", "040904e4", "04090000")
	for _, l := range langs {
		if v := query(`\StringFileInfo\` + l + `\ProductVersion`); v != nil {
			var u []uint16
			for i := 0; i+1 < len(v); i += 2 {
				c := binary.LittleEndian.Uint16(v[i:])
				if c == 0 {
					break
				}
				u = append(u, c)
			}
			return string(utf16.Decode(u)), nil
		}
	}
	return "", nil
}

// ---- programs ----------------------------------------------------------------------------------------------------

func (h *winHost) Run(ctx context.Context, exe string, args []string, timeout time.Duration) (RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = filepath.Dir(exe)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// a killed client may leave a child holding the pipes: stop waiting for them (as Invoke-Native does)
	cmd.WaitDelay = 3 * time.Second
	win.HideConsoleCmd(cmd)
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	err := cmd.Wait()
	res := RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

// ---- files -------------------------------------------------------------------------------------------------------

// isHidden: the entry has FILE_ATTRIBUTE_HIDDEN (what Get-ChildItem leaves out without -Force).
func isHidden(fi fs.FileInfo) bool {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&syscall.FILE_ATTRIBUTE_HIDDEN != 0
	}
	return false
}

// longPath is an existing path with its 8.3 short names expanded, as [IO.Path]::GetFullPath of Windows PowerShell
// 5.1 gives it: GetLongPathNameW, only when the path has a '~' (otherwise GetFullPath keeps it, case included).
// %TEMP% is the 8.3 short form of %LOCALAPPDATA%\Temp (PROFIL~1 style) when the profile folder name is long or not ASCII, while
// %LOCALAPPDATA% keeps the long name. p unchanged when it cannot be expanded.
func longPath(p string) string {
	if !strings.Contains(p, "~") {
		return p
	}
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 260)
	for range 3 {
		n, err := syscall.GetLongPathName(u, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return p
		}
		if int(n) < len(buf) {
			return syscall.UTF16ToString(buf[:n])
		}
		// too small: n is the size it needs, terminating NUL included
		buf = make([]uint16, n)
	}
	return p
}

// ---- desktop, clipboard, Explorer --------------------------------------------------------------------------------

// desktopDir is [Environment]::GetFolderPath('Desktop') (SHGetFolderPath CSIDL_DESKTOP).
func desktopDir() (string, error) {
	if err := procSHGetFolderPathW.Find(); err != nil {
		return "", errs.Wrap(errs.Internal, err, "找不到桌面文件夹")
	}
	buf := make([]uint16, 260)
	if r, _, _ := procSHGetFolderPathW.Call(0, 0x0000, 0, 0, uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return "", errs.Newf(errs.Internal, "找不到桌面文件夹（0x%08X）", uint32(r)).WithAdvice("用 --out-dir 指定问题包放在哪里")
	}
	d := syscall.UTF16ToString(buf)
	if d == "" {
		return "", errs.New(errs.Internal, "找不到桌面文件夹").WithAdvice("用 --out-dir 指定问题包放在哪里")
	}
	return d, nil
}

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// copyClipboard puts text on the clipboard as CF_UNICODETEXT (what Set-Clipboard does), retrying for a second while
// another program has the clipboard open.
func copyClipboard(text string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	u, err := syscall.UTF16FromString(text)
	if err != nil {
		return errs.Wrap(errs.InvalidArgument, err, "文字里有 NUL 字符")
	}
	opened := false
	var lastErr error
	for range 20 {
		if r, _, e := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		} else {
			lastErr = e
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		return errs.Wrap(errs.Internal, lastErr, "剪贴板被别的程序占着："+fmt.Sprint(lastErr))
	}
	defer procCloseClipboard.Call()
	if r, _, e := procEmptyClipboard.Call(); r == 0 {
		return errs.Wrap(errs.Internal, e, "清不了剪贴板："+e.Error())
	}
	hmem, _, e := procGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if hmem == 0 {
		return errs.Wrap(errs.Internal, e, "分配不了剪贴板内存："+e.Error())
	}
	p, _, e := procGlobalLock.Call(hmem)
	if p == 0 {
		procGlobalFree.Call(hmem)
		return errs.Wrap(errs.Internal, e, "分配不了剪贴板内存："+e.Error())
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)*2))
	procGlobalUnlock.Call(hmem)
	if r, _, e := procSetClipboardData.Call(cfUnicodeText, hmem); r == 0 {
		procGlobalFree.Call(hmem)
		return errs.Wrap(errs.Internal, e, "写不进剪贴板："+e.Error())
	}
	return nil
}

// openExplorer is `explorer.exe /select,"<path>"`, not waited for.
func openExplorer(path string) error {
	exe := filepath.Join(windowsDir(), "explorer.exe")
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
