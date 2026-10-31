// Package root is a ModAVD root directory: its layout, mas-avd.json, port plan and component checks.
//
// The on-disk contract is shared with AUTO-MAS (app/utils/common/emulator2/avd/{constants,components}.py), which
// manages the same roots today. Every rule here must stay identical to it, or the two would disagree about the
// same instance: index 0-9 without 5, ports 20000 + 10*i (+1 adb, +2 gRPC), private adb server 20050, mas-avd.json
// read with a BOM allowed and written atomically without one.
//
//	<root>\
//	  sdk\emulator\emulator.exe        ModAVD's own emulator (the package)
//	  sdk\platform-tools\adb.exe       Google's, fetched by 补全组件.bat
//	  sdk\system-images\android-34\google_apis\x86_64\
//	  avd\mas_<i>.ini, avd\mas_<i>.avd\config.ini
//	  components\                      optional APKs installed into the guest (light launcher, host IME)
//	  logs\  shader-cache\  guest-patches\
//	  mas-avd.json                     metadata: components, per-instance options, ports, sdkRoot
//	  tools\modavd.exe                 this program
//
// mas-avd.json is written the way AUTO-MAS's Python writes it on Windows: json.dumps(ensure_ascii=False, indent=2)
// through a text-mode file, i.e. CRLF line ends, UTF-8 without BOM, no final line break.
package root

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

// ---- ports and names (identical to AUTO-MAS constants.py) -------------------------------------------------------

const (
	PortBase = 20000
	PortStep = 10
	// The private adb server every instance registers with (ANDROID_ADB_SERVER_PORT). Never 5037: LDPlayer / MuMu /
	// production MAA use it and adb servers of different versions kill each other there.
	DefaultAdbServerPort = 20050
	// The adb server scripts (MAA, MaaFW ...) use for these instances.
	DefaultScriptAdbServerPort = 20049
	// Used when the configured private server port equals DefaultScriptAdbServerPort.
	FallbackScriptAdbServerPort = 20048
	MaxIndex                    = 9
	AvdNamePrefix               = "mas_"
)

// ReservedIndex: index 5's ports 20050-20052 sit on the private adb server.
const ReservedIndex = 5

// NoLocalScanEnv keeps any adb server we start from scanning 5555-5585 and grabbing other emulators (LDPlayer) on the
// same machine (10-04: 20058 / 20059 both picked up production emulator-5560).
var NoLocalScanEnv = map[string]string{"ADB_LOCAL_TRANSPORT_MAX_PORT": "5554"}

func ValidIndex(i int) bool { return i >= 0 && i <= MaxIndex && i != ReservedIndex }

func ConsolePort(i int) int { return PortBase + PortStep*i }
func AdbPort(i int) int     { return ConsolePort(i) + 1 }
func GrpcPort(i int) int    { return ConsolePort(i) + 2 }
func AvdName(i int) string  { return AvdNamePrefix + strconv.Itoa(i) }
func Serial(i int) string   { return "emulator-" + strconv.Itoa(ConsolePort(i)) }

// IndexOfConsolePort maps 20000 + 10*i back to i; ok=false for any other port.
func IndexOfConsolePort(port int) (int, bool) {
	if port < PortBase || (port-PortBase)%PortStep != 0 {
		return 0, false
	}
	i := (port - PortBase) / PortStep
	return i, i <= MaxIndex
}

// InstancePorts is every console / adb / gRPC port of every valid index; configured server ports must avoid them.
func InstancePorts() map[int]bool {
	ports := map[int]bool{}
	for i := 0; i <= MaxIndex; i++ {
		if ValidIndex(i) {
			c := ConsolePort(i)
			ports[c], ports[c+1], ports[c+2] = true, true, true
		}
	}
	return ports
}

// ---- layout ------------------------------------------------------------------------------------------------------

const (
	SDKDir          = "sdk"
	AvdDir          = "avd"
	ComponentsDir   = "components"
	MetadataFile    = "mas-avd.json"
	LogsDir         = "logs"
	ShaderCacheDir  = "shader-cache"
	GuestPatchDir   = "guest-patches"
	ManifestFile    = "modavd-manifest.json"
	SystemImageDir  = `system-images\android-34\google_apis\x86_64`
	LauncherAPKName = "de.jrpie.android.launcher_57.apk"
	ImeAPKName      = "modavd-ime.apk"
)

// MinModBuild: emulators whose Pkg.BuildId is below mas-<this> refuse to boot (AUTO-MAS MIN_MOD_AVD_BUILD).
const MinModBuild = 25

// SelfBuiltMark is a qemu property only our build has (sdk-mas19+); scanned for in qemu-system-x86_64-headless.exe.
const SelfBuiltMark = "free-page-reporting"

// EnvRoot names the root when --root is not given.
const EnvRoot = "MODAVD_ROOT"

// Root is one ModAVD root. Dir is absolute and cleaned.
type Root struct {
	Dir string
}

// Resolve picks the root: the --root flag when given, else env MODAVD_ROOT, else the directory this executable
// sits in or its parent, whichever looks like a root (has sdk\ or mas-avd.json; the package puts modavd.exe in
// <root>\tools). The directory must exist. Error code: NotFound / Usage.
func Resolve(flag string) (*Root, error) {
	exe, _ := os.Executable()
	return resolve(flag, os.Getenv(EnvRoot), exe)
}

func resolve(flag, env, exe string) (*Root, error) {
	if s := cleanArg(flag); s != "" {
		return openDir(s, "--root 指定的")
	}
	if s := cleanArg(env); s != "" {
		return openDir(s, "环境变量 "+EnvRoot+" 指定的")
	}
	if exe != "" {
		dir := filepath.Dir(exe)
		for _, cand := range []string{dir, filepath.Dir(dir)} {
			if looksLikeRoot(cand) {
				return openDir(cand, "")
			}
		}
	}
	return nil, errs.New(errs.Usage, "找不到魔改 AVD 根目录").WithAdvice(
		"用 --root 指定根目录（里面有 sdk 文件夹和 mas-avd.json），或设置环境变量 " + EnvRoot +
			"；modavd.exe 放在根目录或根目录的 tools 文件夹里时会自动找到")
}

// cleanArg drops surrounding blanks and quotes: `--root "D:\ModAVD\"` reaches us as `D:\ModAVD"` (the backslash
// escapes the quote in CommandLineToArgvW), and a Windows path never contains '"'.
func cleanArg(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

func looksLikeRoot(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, SDKDir)); err == nil && fi.IsDir() {
		return true
	}
	fi, err := os.Stat(filepath.Join(dir, MetadataFile))
	return err == nil && fi.Mode().IsRegular()
}

func openDir(dir, what string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errs.Wrap(errs.Usage, err, fmt.Sprintf("%s魔改 AVD 根目录写法不对: %s", what, dir))
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, errs.Wrap(errs.NotFound, err, fmt.Sprintf("%s魔改 AVD 根目录不存在: %s", what, abs))
	}
	if !fi.IsDir() {
		return nil, errs.Newf(errs.NotFound, "%s魔改 AVD 根目录不是文件夹: %s", what, abs)
	}
	return &Root{Dir: abs}, nil
}

// New wraps an existing directory without auto-detection (tests, MCP).
func New(dir string) (*Root, error) { return openDir(cleanArg(dir), "") }

func (r *Root) SDK() string { return filepath.Join(r.Dir, SDKDir) } // <root>\sdk

// LocalSDKRoot is mas-avd.json's sdkRoot (a dev SDK such as C:\sdk-src\sdk-mas35), "" when not set. A relative
// value is taken relative to the root (AUTO-MAS would resolve it against its own working directory, which is
// never what was meant).
func (r *Root) LocalSDKRoot() string {
	s, ok := r.MetadataLoose().Obj().Get("sdkRoot").Str()
	if !ok {
		return ""
	}
	if s = PyStrip(s); s == "" {
		return ""
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(r.Dir, s)
	}
	return filepath.Clean(s)
}

// RuntimeSDK is the SDK actually used: LocalSDKRoot when set, else SDK().
func (r *Root) RuntimeSDK() string {
	if s := r.LocalSDKRoot(); s != "" {
		return s
	}
	return r.SDK()
}

func (r *Root) EmulatorExe() string { return filepath.Join(r.RuntimeSDK(), "emulator", "emulator.exe") }
func (r *Root) AdbExe() string      { return filepath.Join(r.RuntimeSDK(), "platform-tools", "adb.exe") }
func (r *Root) QemuHeadlessExe() string {
	return filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe")
}
func (r *Root) AvdHome() string        { return filepath.Join(r.Dir, AvdDir) }
func (r *Root) LogsDir() string        { return filepath.Join(r.Dir, LogsDir) }
func (r *Root) ShaderCacheDir() string { return filepath.Join(r.Dir, ShaderCacheDir) }
func (r *Root) GuestPatchDir() string  { return filepath.Join(r.Dir, GuestPatchDir) }
func (r *Root) ComponentsDir() string  { return filepath.Join(r.Dir, ComponentsDir) }
func (r *Root) MetadataPath() string   { return filepath.Join(r.Dir, MetadataFile) }
func (r *Root) LauncherAPK() string    { return filepath.Join(r.ComponentsDir(), LauncherAPKName) }
func (r *Root) ImeAPK() string         { return filepath.Join(r.ComponentsDir(), ImeAPKName) }
func (r *Root) ManifestPath() string   { return filepath.Join(r.Dir, ManifestFile) }

// ---- mas-avd.json ------------------------------------------------------------------------------------------------

// replaceRetry: AUTO-MAS _REPLACE_RETRY_SECONDS.
const replaceRetry = 3 * time.Second

// metadataLockWait bounds the wait for another modavd's read-modify-write (each takes milliseconds, plus at most
// replaceRetry). A variable for tests.
var metadataLockWait = 10 * time.Second

const metadataAdvice = "这份文件没有被改动；请修好它（JSON 格式）后再试"

// ReadMetadata reads mas-avd.json: a missing file is an empty object; a file that exists but cannot be read, is not
// JSON or is not an object is an errs.MetadataUnreadable error (callers that write must stop then, never overwrite).
func (r *Root) ReadMetadata() (*jsonord.Value, error) {
	path := r.MetadataPath()
	data, err := ReadFileShared(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return jsonord.NewObject(), nil
		}
		reason := err.Error()
		if errors.Is(err, fs.ErrPermission) {
			reason = "拒绝访问（可能正被别的程序独占）"
		}
		return nil, errs.Wrap(errs.MetadataUnreadable, err, fmt.Sprintf("%s 读不出：%s", path, reason)).
			WithAdvice(metadataAdvice)
	}
	v, err := jsonord.Parse(data)
	if err != nil {
		return nil, errs.Wrap(errs.MetadataUnreadable, err, fmt.Sprintf("%s 读不出：%s", path, errs.As(err).Message)).
			WithAdvice(metadataAdvice)
	}
	if v.Kind() != jsonord.Object {
		return nil, errs.Newf(errs.MetadataUnreadable, "%s 不是 JSON 对象", path).WithAdvice(metadataAdvice)
	}
	return v, nil
}

// MetadataLoose is ReadMetadata with any error turned into an empty object (for reads only).
func (r *Root) MetadataLoose() *jsonord.Value {
	v, err := r.ReadMetadata()
	if err != nil {
		return jsonord.NewObject()
	}
	return v
}

// UpdateMetadata does read (strict) - fn - atomic write: <file>.tmp then rename over it, retrying the rename for up
// to 3 s while another process holds the file open (AUTO-MAS does the same). Concurrent modavd processes are
// serialized with a lock on <root>\mas-avd.json.lock. fn returning an error aborts without writing.
//
// The lock is LockFileEx on the lock file (released by the system when the process dies, so there are no stale
// locks). AUTO-MAS does not take it: a write by AUTO-MAS at the same moment can still win over ours (lost update,
// as between two AUTO-MAS writers today), but never leaves a half-written file. The temp file is
// mas-avd.json.modavd-<pid>.tmp rather than AUTO-MAS's mas-avd.json.tmp, so the two writers never truncate each
// other's temp file.
func (r *Root) UpdateMetadata(fn func(o *jsonord.Obj) error) error {
	return r.editMetadata(func(o *jsonord.Obj) (bool, error) { return true, fn(o) })
}

// editMetadata is UpdateMetadata where fn also says whether it changed anything (no write when not).
func (r *Root) editMetadata(fn func(o *jsonord.Obj) (bool, error)) error {
	l, err := r.lockMetadata()
	if err != nil {
		return err
	}
	defer l.unlock()
	v, err := r.ReadMetadata()
	if err != nil {
		return err
	}
	changed, err := fn(v.Obj())
	if err != nil || !changed {
		return err
	}
	return r.writeMetadata(v)
}

func (r *Root) lockMetadata() (*fileLock, error) {
	path := r.MetadataPath() + ".lock"
	l, ok, err := lockFile(path, metadataLockWait)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "mas-avd.json 的锁文件打不开: "+path)
	}
	if !ok {
		return nil, errs.Newf(errs.Busy, "mas-avd.json 正被另一个 modavd 改写，等了 %.0f 秒仍没改完",
			metadataLockWait.Seconds())
	}
	return l, nil
}

// metadataBytes is the file content AUTO-MAS would write for v: Python's json.dumps through a Windows text-mode
// file turns every "\n" into "\r\n" (strings never hold a raw newline: they are escaped).
func metadataBytes(v *jsonord.Value) []byte {
	return bytes.ReplaceAll(v.MarshalPython(), []byte("\n"), []byte("\r\n"))
}

// writeMetadata writes the temp file (flushed to disk) and renames it over mas-avd.json. Caller holds the lock.
func (r *Root) writeMetadata(v *jsonord.Value) error {
	path := r.MetadataPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errs.Wrap(errs.Internal, err, "建不了目录: "+filepath.Dir(path))
	}
	tmp := fmt.Sprintf("%s.modavd-%d.tmp", path, os.Getpid())
	if err := writeFileSync(tmp, metadataBytes(v)); err != nil {
		os.Remove(tmp)
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("写 %s 失败：%v", tmp, err))
	}
	if err := ReplaceWithRetry(tmp, path); err != nil {
		os.Remove(tmp)
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("替换 %s 失败：%v", path, err)).
			WithAdvice("可能正被别的程序独占；关掉占用它的程序后再试")
	}
	return nil
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReplaceWithRetry renames source over target, retrying for up to 3 s while target (or source) is open somewhere
// (AUTO-MAS replace_with_retry). Any open handle blocks the rename, delete sharing or not (see OpenShared).
func ReplaceWithRetry(source, target string) error {
	deadline := time.Now().Add(replaceRetry)
	for {
		err := os.Rename(source, target)
		if err == nil || !retryableReplaceError(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// EnsureMetadata writes a fresh mas-avd.json when there is none, like AUTO-MAS ensure_metadata: "components" (id ->
// installed version, only installed ones) and "addedAt" (local time, ISO seconds). Returns whether it wrote one.
// The optional host IME is left out of "components", as AUTO-MAS's install_status does not list it.
func (r *Root) EnsureMetadata() (bool, error) {
	exists := func() (bool, error) {
		_, err := os.Stat(r.MetadataPath())
		if err == nil {
			return true, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, errs.Wrap(errs.MetadataUnreadable, err, fmt.Sprintf("%s 读不出：%v", r.MetadataPath(), err))
	}
	if ok, err := exists(); ok || err != nil {
		return false, err
	}
	l, err := r.lockMetadata()
	if err != nil {
		return false, err
	}
	defer l.unlock()
	if ok, err := exists(); ok || err != nil {
		return false, err
	}
	components := jsonord.NewObject()
	for _, c := range r.Components() {
		if c.Installed && c.ID != "ime" {
			components.Obj().Set(c.ID, jsonord.NewString(c.Version))
		}
	}
	v := jsonord.NewObject()
	v.Obj().Set("components", components)
	v.Obj().Set("addedAt", jsonord.NewString(NowISO()))
	if err := r.writeMetadata(v); err != nil {
		return false, err
	}
	return true, nil
}

// NowISO is datetime.now().isoformat(timespec="seconds"): local time, no offset ("2026-10-28T15:04:05").
func NowISO() string { return time.Now().Format("2006-01-02T15:04:05") }

// InstanceMeta is a copy of instances.<i> (empty object when absent or not an object).
func (r *Root) InstanceMeta(i int) *jsonord.Obj {
	return r.MetadataLoose().Obj().Get("instances").Obj().Get(strconv.Itoa(i)).Obj().Clone()
}

// instanceKeyOrder is the order AUTO-MAS's keyword arguments add these keys in (create: memoryMb, initialized,
// createdAt; first boot: initialized, initializedAt, launcher, renderer, softwareRenderer, initErrors). A map has no
// order, so UpdateInstanceMeta appends new keys in this order, then any others sorted, which gives the same file.
var instanceKeyOrder = []string{"memoryMb", "balloon", "initialized", "createdAt", "initializedAt", "launcher",
	"renderer", "softwareRenderer", "initErrors"}

// UpdateInstanceMeta merges changes into instances.<i> (creating it), keeping its other keys. Existing keys change
// in place; new ones are appended in AUTO-MAS's order (instanceKeyOrder), unknown ones after them sorted. A nil
// value is written as null.
func (r *Root) UpdateInstanceMeta(i int, changes map[string]*jsonord.Value) error {
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		for n, known := range instanceKeyOrder {
			if k == known {
				return n
			}
		}
		return len(instanceKeyOrder)
	}
	sort.Slice(keys, func(a, b int) bool {
		ra, rb := rank(keys[a]), rank(keys[b])
		if ra != rb {
			return ra < rb
		}
		return keys[a] < keys[b]
	})
	return r.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		for _, k := range keys {
			meta.Set(k, changes[k])
		}
		return nil
	})
}

// EditInstanceMeta runs fn on instances.<i> (an empty object when absent or not an object) inside UpdateMetadata
// and stores the result back in place; for changes whose new keys must come in a given order. fn returning an error
// aborts without writing.
func (r *Root) EditInstanceMeta(i int, fn func(meta *jsonord.Obj) error) error {
	key := strconv.Itoa(i)
	return r.UpdateMetadata(func(o *jsonord.Obj) error {
		instances := o.Get("instances")
		if instances.Kind() != jsonord.Object {
			instances = jsonord.NewObject()
		}
		meta := instances.Obj().Get(key)
		if meta.Kind() != jsonord.Object {
			meta = jsonord.NewObject()
		}
		if err := fn(meta.Obj()); err != nil {
			return err
		}
		instances.Obj().Set(key, meta)
		o.Set("instances", instances)
		return nil
	})
}

// RemoveInstanceMeta deletes instances.<i> if present (no write otherwise).
func (r *Root) RemoveInstanceMeta(i int) error {
	key := strconv.Itoa(i)
	return r.editMetadata(func(o *jsonord.Obj) (bool, error) {
		instances := o.Get("instances").Obj()
		if !instances.Has(key) {
			return false, nil
		}
		instances.Delete(key)
		return true, nil
	})
}

// HasInstanceMeta: mas-avd.json holds instances.<i> (of any type). False when the file cannot be read.
func (r *Root) HasInstanceMeta(i int) bool {
	return r.MetadataLoose().Obj().Get("instances").Obj().Has(strconv.Itoa(i))
}

// ---- per-instance config lock ------------------------------------------------------------------------------------

// configLockWait bounds the wait for another modavd's change of the same instance (a read-modify-write takes
// milliseconds plus at most replaceRetry for the rename; a delete as long as removing the data disk). A variable for
// tests.
var configLockWait = 10 * time.Second

// ConfigLockPath is <root>\avd\.mas_<i>.config.lock, next to package emu's start / boot / stop locks (same naming,
// and like them it does not match AUTO-MAS's "mas_*.ini" scan).
func (r *Root) ConfigLockPath(i int) string {
	return filepath.Join(r.AvdHome(), "."+AvdName(i)+".config.lock")
}

// InstanceLock is a held LockInstanceConfig.
type InstanceLock struct {
	l *fileLock
}

// Unlock releases the lock; nil-safe and idempotent. The lock file stays (see LockInstanceConfig).
func (l *InstanceLock) Unlock() {
	if l != nil {
		l.l.unlock()
	}
}

// LockInstanceConfig serializes, between modavd processes and goroutines, every change of instance i's files: the
// read-modify-write of config.ini (with the instances.<i> change that goes with it), creating and deleting it.
// Without it two writers both read the old config.ini and the later write drops the other's change while both report
// success, or an update that read the config just before a delete writes the deleted instance back. It is the same
// LockFileEx lock as mas-avd.json's (released by the system when the process dies). AUTO-MAS does not take it; it
// only gets a temp name of its own (see avd.WriteIni).
//
// errs.NotFound when the avd directory does not exist: there is nothing to guard then, and neither the directory nor
// the lock file is created. errs.Busy when another holder keeps it for configLockWait.
func (r *Root) LockInstanceConfig(i int) (*InstanceLock, error) {
	path := r.ConfigLockPath(i)
	l, ok, err := lockFile(path, configLockWait)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errs.Wrap(errs.NotFound, err, "实例目录不存在: "+r.AvdHome())
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("实例 %s 的锁文件打不开: %s（%v）", AvdName(i), path, err))
	}
	if !ok {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正被另一个 modavd 改动（新建、改设置或删除），等了 %.0f 秒仍没改完",
			i, configLockWait.Seconds())
	}
	return &InstanceLock{l: l}, nil
}

// AdbServerPort is mas-avd.json adbServerPort when valid (an integer 1024-65535, not 5037, not an instance port),
// else DefaultAdbServerPort. Same rule as AUTO-MAS adb_server_port.
func (r *Root) AdbServerPort() int {
	return adbServerPort(r.MetadataLoose().Obj())
}

// ScriptAdbServerPort is mas-avd.json scriptAdbServerPort when valid and different from AdbServerPort, else
// DefaultScriptAdbServerPort, or FallbackScriptAdbServerPort when that equals the private port.
func (r *Root) ScriptAdbServerPort() int {
	o := r.MetadataLoose().Obj()
	private := adbServerPort(o)
	if port, ok := metadataPort(o, "scriptAdbServerPort"); ok && port != private {
		return port
	}
	if DefaultScriptAdbServerPort != private {
		return DefaultScriptAdbServerPort
	}
	return FallbackScriptAdbServerPort
}

func adbServerPort(o *jsonord.Obj) int {
	if port, ok := metadataPort(o, "adbServerPort"); ok {
		return port
	}
	return DefaultAdbServerPort
}

// metadataPort is AUTO-MAS _metadata_port: int(raw) (so "20089" and 20089.0 count too), then 1024-65535, not 5037,
// not an instance port.
func metadataPort(o *jsonord.Obj, key string) (int, bool) {
	port, ok := pyIntOfValue(o.Get(key))
	if !ok || port < 1024 || port > 65535 || port == 5037 || InstancePorts()[int(port)] {
		return 0, false
	}
	return int(port), true
}

// pyIntOfValue is Python's int() of a JSON value: integers as they are, floats truncated, strings parsed, booleans
// 0 / 1; null, arrays, objects, NaN and infinities fail.
func pyIntOfValue(v *jsonord.Value) (int64, bool) {
	switch v.Kind() {
	case jsonord.Bool:
		if b, _ := v.BoolVal(); b {
			return 1, true
		}
		return 0, true
	case jsonord.Number:
		if i, ok := v.Int(); ok {
			return i, true
		}
		f, ok := v.Float()
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= 1<<63 {
			return 0, false
		}
		return int64(f), true // int() truncates toward zero, like Go's conversion
	case jsonord.String:
		s, _ := v.Str()
		return PyInt(s)
	}
	return 0, false
}

// ---- components ----------------------------------------------------------------------------------------------------

// ComponentStatus is one row of the component list (same ids and meaning as AUTO-MAS install_status).
type ComponentStatus struct {
	ID        string `json:"id"` // platform-tools / emulator / system-image / launcher / ime
	Name      string `json:"name"`
	Version   string `json:"version"`         // source.properties Pkg.Revision, "" when not installed
	Build     string `json:"build,omitempty"` // emulator only: "0.35.0"
	Installed bool   `json:"installed"`       // emulator: present AND self-built AND build >= MinModBuild
	Optional  bool   `json:"optional"`
	LocalSDK  bool   `json:"local_sdk"`
}

// Component names and fixed data (AUTO-MAS constants.py).
const (
	PlatformToolsName = "平台工具（adb）"
	EmulatorName      = "Android 模拟器（emulator）"
	SystemImageName   = "系统镜像 Android 14 Google APIs x86_64"
	LauncherName      = "轻量桌面 µLauncher"
	LauncherVersion   = "0.2.12"
	LauncherAPKSize   = 4099556
	ImeName           = "宿主输入法 org.modavd.ime"
)

// Advice for missing components (AUTO-MAS FETCH_COMPONENTS_ADVICE / PACKAGE_ADVICE; "回来点「检查」" is AUTO-MAS's
// button, here it is the check command).
const (
	FetchComponentsAdvice = "双击魔改 AVD 根目录里的「补全组件.bat」：它会先显示《Android 软件开发套件许可协议》，输入 y 同意后" +
		"从官方渠道下载 adb 和系统镜像（约 1.5 GB，中断了再运行会接着下），看到「完成」后再运行一次 modavd check"
	PackageAdvice = "模拟器要用魔改 AVD 的发布包：从 https://github.com/ModAVD/ModAVD/releases 下载 " +
		"ModAVD-<版本>-win-x64.zip，整个解压到这个目录"
)

// keyFiles: a half-extracted directory exists too, so each component needs these files (AUTO-MAS _KEY_FILES).
var keyFiles = map[string][]string{
	"platform-tools": {"adb.exe"},
	"emulator":       {"emulator.exe", `qemu\windows-x86_64\qemu-system-x86_64.exe`},
	"system-image":   {"system.img", "vendor.img", "kernel-ranchu", "ramdisk.img"},
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func allFiles(dir string, names []string) bool {
	for _, n := range names {
		if !isFile(filepath.Join(dir, n)) {
			return false
		}
	}
	return true
}

// readProperties is AUTO-MAS _read_properties: unreadable = empty. Read with delete sharing and closed at once
// (ReadFileShared), so a read never makes AUTO-MAS's updater fail to delete an old component directory.
func readProperties(path string) map[string]string {
	data, err := ReadFileShared(path)
	if err != nil {
		return map[string]string{}
	}
	_, values := ReadKeyValues(data, false)
	return values
}

// Components lists platform-tools, emulator, system-image (required) and launcher, ime (optional), in that order.
// Key files: platform-tools adb.exe; emulator emulator.exe + qemu\windows-x86_64\qemu-system-x86_64.exe;
// system-image system.img, vendor.img, kernel-ranchu, ramdisk.img. Launcher counts when the APK has its exact size
// 4099556; ime when components\modavd-ime.apk exists.
func (r *Root) Components() []ComponentStatus {
	sdk := r.RuntimeSDK()
	local := r.LocalSDKRoot() != ""
	sdkItem := func(id, name, dir string) ComponentStatus {
		installed := allFiles(dir, keyFiles[id])
		version := ""
		if installed {
			version = readProperties(filepath.Join(dir, "source.properties"))["Pkg.Revision"]
		}
		return ComponentStatus{ID: id, Name: name, Version: version, Installed: installed, LocalSDK: local}
	}
	emulator := ComponentStatus{ID: "emulator", Name: EmulatorName, LocalSDK: local}
	if r.EmulatorPresent() {
		emulator.Version = r.EmulatorVersion()
	}
	if r.EmulatorSelfBuilt() {
		emulator.Build = r.VersionText()
		b, ok := r.ModBuild()
		emulator.Installed = ok && b >= MinModBuild
	}
	launcher := ComponentStatus{ID: "launcher", Name: LauncherName, Version: LauncherVersion, Optional: true}
	if fi, err := os.Stat(r.LauncherAPK()); err == nil && fi.Size() == LauncherAPKSize {
		launcher.Installed = true
	}
	return []ComponentStatus{
		sdkItem("platform-tools", PlatformToolsName, filepath.Join(sdk, "platform-tools")),
		emulator,
		sdkItem("system-image", SystemImageName, filepath.Join(sdk, SystemImageDir)),
		launcher,
		{ID: "ime", Name: ImeName, Installed: isFile(r.ImeAPK()), Optional: true},
	}
}

// Missing names the required components that are not installed (Chinese names), empty when ready.
func (r *Root) Missing() []string {
	var missing []string
	for _, c := range r.Components() {
		if !c.Optional && !c.Installed {
			missing = append(missing, c.Name)
		}
	}
	return missing
}

// MissingError is the NotReady error for Missing() with the advice AUTO-MAS gives (package / 补全组件.bat):
// "这个目录里缺少：A、B。<advice>" (AUTO-MAS missing_components_message). An empty list is taken from Missing().
func (r *Root) MissingError(missing []string) *errs.Error {
	if len(missing) == 0 {
		missing = r.Missing()
	}
	if len(missing) == 0 {
		return errs.New(errs.NotReady, "这个目录里的组件不全").WithAdvice(FetchComponentsAdvice)
	}
	var advice []string
	if slices.Contains(missing, EmulatorName) {
		advice = append(advice, PackageAdvice)
	}
	if slices.ContainsFunc(missing, func(name string) bool { return name != EmulatorName }) {
		advice = append(advice, FetchComponentsAdvice)
	}
	return errs.New(errs.NotReady, "这个目录里缺少："+strings.Join(missing, "、")).
		WithAdvice(strings.Join(advice, "；"))
}

func (r *Root) EmulatorPresent() bool {
	return allFiles(filepath.Join(r.RuntimeSDK(), "emulator"), keyFiles["emulator"])
}

// EmulatorSelfBuilt: present and qemu-system-x86_64-headless.exe contains SelfBuiltMark.
func (r *Root) EmulatorSelfBuilt() bool {
	return r.EmulatorPresent() && HasSelfBuiltMark(r.QemuHeadlessExe())
}

// selfBuiltChunk is the read size of the mark scan (a variable so tests can exercise chunk borders).
var selfBuiltChunk = 4 << 20

// selfBuiltCache: (path, mtime, size) -> found. A replaced emulator has another mtime, so it is scanned again.
var selfBuiltCache sync.Map

// HasSelfBuiltMark scans exe for SelfBuiltMark in chunks, keeping len(mark)-1 bytes across chunk borders. A missing
// or unreadable file counts as not ours (and is not cached). The scan reads tens of MB (once per file version: the
// result is cached), so it opens the file with delete sharing (OpenShared): meanwhile the updater may delete the old
// emulator directory.
func HasSelfBuiltMark(exe string) bool {
	fi, err := os.Stat(exe)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	key := fmt.Sprintf("%s|%d|%d", strings.ToLower(filepath.Clean(exe)), fi.ModTime().UnixNano(), fi.Size())
	if v, ok := selfBuiltCache.Load(key); ok {
		return v.(bool)
	}
	f, err := OpenShared(exe)
	if err != nil {
		return false
	}
	defer f.Close()
	mark := []byte(SelfBuiltMark)
	keep := len(mark) - 1
	buf := make([]byte, keep+selfBuiltChunk)
	carry := 0
	found := false
	for {
		n, err := io.ReadFull(f, buf[carry:])
		data := buf[:carry+n]
		if bytes.Contains(data, mark) {
			found = true
			break
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return false
		}
		carry = min(keep, len(data))
		copy(buf, data[len(data)-carry:])
	}
	selfBuiltCache.Store(key, found)
	return found
}

var (
	modBuildRe  = regexp.MustCompile(`^mas-([0-9]+)$`)
	newTagRe    = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	legacyTagRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+-mas([0-9]+)$`)
)

func (r *Root) emulatorProps() map[string]string {
	return readProperties(filepath.Join(r.RuntimeSDK(), "emulator", "source.properties"))
}

// ModBuild is N from sdk\emulator\source.properties Pkg.BuildId=mas-N; ok=false when absent or another format.
func (r *Root) ModBuild() (int, bool) {
	m := modBuildRe.FindStringSubmatch(PyStrip(r.emulatorProps()["Pkg.BuildId"]))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// EmulatorVersion is sdk\emulator\source.properties Pkg.Revision ("37.2.10"), "" when unreadable.
func (r *Root) EmulatorVersion() string { return r.emulatorProps()["Pkg.Revision"] }

// ParseVersion reads a ModAVD version: "v0.32.1" / "0.33.0" -> (0, 32, 1); the legacy tag "v37.2.10-mas32" ->
// (0, 32, 0). ok=false for anything else (AUTO-MAS update_source.parse_version).
func ParseVersion(text string) (major, build, patch int, ok bool) {
	text = PyStrip(text)
	if m := newTagRe.FindStringSubmatch(text); m != nil {
		var err1, err2, err3 error
		major, err1 = strconv.Atoi(m[1])
		build, err2 = strconv.Atoi(m[2])
		patch, err3 = strconv.Atoi(m[3])
		return major, build, patch, err1 == nil && err2 == nil && err3 == nil
	}
	if m := legacyTagRe.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		return 0, n, 0, err == nil
	}
	return 0, 0, 0, false
}

// manifestVersion is modavd-manifest.json's "version", "" when the file is missing / unreadable / not an object or
// the value is not a string. Read like readProperties (the updater replaces it).
func (r *Root) manifestVersion() string {
	data, err := ReadFileShared(r.ManifestPath())
	if err != nil {
		return ""
	}
	v, err := jsonord.Parse(data)
	if err != nil {
		return ""
	}
	s, _ := v.Obj().Get("version").Str()
	return s
}

// VersionText is the ModAVD version for people, "0.35.0": the manifest's version when its build matches ModBuild,
// else 0.<ModBuild>.0; "" when ModBuild is unknown. Same rule as AUTO-MAS local_version_text.
func (r *Root) VersionText() string {
	build, ok := r.ModBuild()
	if !ok {
		return ""
	}
	if major, b, patch, ok := ParseVersion(r.manifestVersion()); ok && b == build {
		return fmt.Sprintf("%d.%d.%d", major, b, patch)
	}
	return fmt.Sprintf("0.%d.0", build)
}
