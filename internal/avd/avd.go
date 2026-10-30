// Package avd is the instance files: avd\mas_<i>.ini and avd\mas_<i>.avd\config.ini, plus the per-instance part of
// mas-avd.json. File operations only; nothing here looks at processes or ports (callers pass a busy-port check).
//
// Port of AUTO-MAS app/utils/common/emulator2/avd/instances.py (+ the choices in constants.py). Files must come out
// byte-identical to what AUTO-MAS writes, so the two can manage the same root during the move to modavd.
package avd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// Instance options (user decisions: memory and cores only in these steps, no silent rounding).
var (
	MemoryChoicesMB = []int{3072, 4096, 5120, 6144}
	CPUChoices      = []int{2, 4, 6}
	FPSChoices      = []int{30, 60}
)

const (
	DefaultMemoryMB        = 6144
	DefaultCPU             = 6
	DefaultFPS             = 60
	DefaultDataPartitionGB = 64
	MinDataPartitionGB     = 16
	MaxDataPartitionGB     = 512
	// Display is fixed at 720p (user 10-04), written into config.ini before every boot, never `wm size`.
	ScreenWidth, ScreenHeight, ScreenDensity = 1280, 720, 240
	TargetAPI                                = "android-34"
	// image.sysdir.1, relative to the SDK root, with the trailing backslash AUTO-MAS writes.
	SystemImageSysdir = `system-images\android-34\google_apis\x86_64\`
)

// ConfigTemplate is config.ini of a new instance before the per-instance keys (AUTO-MAS _CONFIG_TEMPLATE, same
// order). Taken from the tuned mas_p0: landscape, host GPU, no device frame, no sound card keys (the real switch is
// -feature -VirtioSndCard), hw.mainKeys=yes (no on-screen navigation bar; ModAVD #1).
var ConfigTemplate = [][2]string{
	{"PlayStore.enabled", "no"},
	{"abi.type", "x86_64"},
	{"avd.ini.encoding", "UTF-8"},
	{"disk.cachePartition", "yes"},
	{"disk.cachePartition.size", "66MB"},
	{"fastboot.forceColdBoot", "yes"},
	{"fastboot.forceFastBoot", "no"},
	{"hw.accelerometer", "yes"},
	{"hw.arc", "false"},
	{"hw.audioInput", "no"},
	{"hw.audioOutput", "no"},
	{"hw.battery", "yes"},
	{"hw.camera.back", "none"},
	{"hw.camera.front", "none"},
	{"hw.cpu.arch", "x86_64"},
	{"hw.dPad", "no"},
	{"hw.gps", "no"},
	{"hw.gpu.enabled", "yes"},
	{"hw.gpu.mode", "host"},
	{"hw.gsmModem", "yes"},
	{"hw.gyroscope", "yes"},
	{"hw.initialOrientation", "landscape"},
	{"hw.keyboard", "yes"},
	{"hw.keyboard.lid", "yes"},
	{"hw.lcd.depth", "32"},
	{"hw.lcd.vsync", "60"},
	{"hw.mainKeys", "yes"},
	{"hw.sdCard", "no"},
	{"hw.sensors.orientation", "yes"},
	{"hw.sensors.proximity", "no"},
	{"hw.trackBall", "no"},
	{"hw.useext4", "yes"},
	{"showDeviceFrame", "no"},
	{"tag.display", "Google APIs"},
	{"tag.id", "google_apis"},
	{"target", "android-34"},
	{"vm.heapSize", "192M"},
}

// nowISO stamps createdAt; a variable so tests can compare with Python byte for byte.
var nowISO = root.NowISO

// ---- ini files -------------------------------------------------------------------------------------------------

// Ini is a flat key=value file in file order. Lines without '=' or with an empty key are dropped on read (AUTO-MAS
// read_ini); keys and values are trimmed.
type Ini struct {
	keys []string
	m    map[string]string
}

func NewIni() *Ini { return &Ini{m: map[string]string{}} }

func (c *Ini) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.m[key]
	return v, ok
}

func (c *Ini) Value(key string) string { v, _ := c.Get(key); return v } // "" when absent

// Set changes the value in place, or appends the key (a Python dict assignment).
func (c *Ini) Set(key, value string) {
	if c.m == nil {
		c.m = map[string]string{}
	}
	if _, ok := c.m[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.m[key] = value
}

// Keys is a copy of the keys in order.
func (c *Ini) Keys() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.keys...)
}

// Len is the number of keys (0 for nil): AUTO-MAS treats an empty config as unreadable.
func (c *Ini) Len() int {
	if c == nil {
		return 0
	}
	return len(c.keys)
}

// ReadIni reads a file as UTF-8 (invalid bytes replaced). A missing / unreadable file is an error (errs.NotFound /
// errs.Internal).
//
// List reads every config.ini on each status poll, so the read must not get in AUTO-MAS's way: the file is opened
// with delete sharing (root.ReadFileShared), else AUTO-MAS deleting the instance at that moment would stop half-way
// (sharing violation), and it is read in one go and closed, since nothing makes a replace over an open file work
// (write_ini, which does not retry; see root.OpenShared).
func ReadIni(path string) (*Ini, error) {
	data, err := root.ReadFileShared(path)
	if err != nil {
		code, reason := errs.Internal, err.Error()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			code, reason = errs.NotFound, "文件不存在"
		case errors.Is(err, fs.ErrPermission):
			reason = "拒绝访问（可能正被别的程序独占）"
		}
		return nil, errs.Wrap(code, err, fmt.Sprintf("%s 读不出：%s", path, reason))
	}
	return parseIni(data), nil
}

// parseIni is AUTO-MAS read_ini on the bytes: read_text(encoding="utf-8", errors="replace"), splitlines, partition
// at the first '=', strip, skip empty keys. A BOM is not skipped (Python's "utf-8" keeps it in the first key).
func parseIni(data []byte) *Ini {
	keys, values := root.ReadKeyValues(data, true)
	return &Ini{keys: keys, m: values}
}

// Encode is "key=value\n" for every key, in order.
func (c *Ini) Encode() []byte {
	var b bytes.Buffer
	for _, k := range c.Keys() {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(c.m[k])
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// WriteIni writes <path>.modavd-<pid>.tmp then renames it over path. Callers of an instance's files hold its
// root.LockInstanceConfig (one temp name per process is enough then).
//
// The bytes are what AUTO-MAS's Path.write_text(encoding="utf-8") writes on Windows: text mode turns every "\n" into
// "\r\n" (the files in production are CRLF), no BOM. Unlike AUTO-MAS the temp file is flushed before the rename and
// the rename is retried for up to 3 s while the target is open elsewhere (root.ReplaceWithRetry); both only matter
// where AUTO-MAS would leave a damaged file or fail.
//
// Two more differences, both because AUTO-MAS writes the same files without our lock:
//   - The temp name is not AUTO-MAS's <path>.tmp. Both open it truncating with write sharing, so two writers at once
//     would interleave their bytes in one temp file and rename the mix over config.ini.
//   - The parent directory is never created (errs.NotFound when it is missing). An instance deleted meanwhile must not
//     come back as a directory holding only config.ini, which neither tool lists and Create refuses.
func WriteIni(path string, c *Ini) error {
	data := bytes.ReplaceAll(c.Encode(), []byte("\n"), []byte("\r\n"))
	tmp := fmt.Sprintf("%s.modavd-%d.tmp", path, os.Getpid())
	if err := writeFileSync(tmp, data); err != nil {
		os.Remove(tmp)
		if errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(errs.NotFound, err, fmt.Sprintf("写不了 %s：目录 %s 不存在（实例可能刚被删除）", path,
				filepath.Dir(path)))
		}
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("写 %s 失败：%v", tmp, err))
	}
	if err := root.ReplaceWithRetry(tmp, path); err != nil {
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

// ---- instances ---------------------------------------------------------------------------------------------------

// Instance is one mas_<i> found on disk.
type Instance struct {
	Index  int
	Name   string // mas_<i>
	Title  string // avd.ini.displayname, or Name
	Dir    string // <root>\avd\mas_<i>.avd
	Config *Ini
}

// intOrNone is AUTO-MAS _int_or_none: int(str(raw).strip()); ok=false where Python gets None.
func intOrNone(s string) (int, bool) {
	v, ok := root.PyInt(root.PyStrip(s))
	return int(v), ok
}

// mul1024 is v*1024, ok=false when it does not fit (Python's int would just grow).
func mul1024(v int) (int, bool) {
	if v > (1<<62)/512 || v < -(1<<62)/512 {
		return 0, false
	}
	return v * 1024, true
}

// floorDiv1024 is Python's v // 1024 (rounds toward minus infinity, unlike Go's /).
func floorDiv1024(v int) int {
	q := v / 1024
	if v%1024 != 0 && v < 0 {
		q--
	}
	return q
}

// parseRAMSize is AUTO-MAS parse_memory_mb: "4096" / "4096M" / "4096MB" / "4G" (case-insensitive) -> MB.
func parseRAMSize(raw string) (int, bool) {
	text := strings.ToUpper(root.PyStrip(raw))
	if text == "" {
		return 0, false
	}
	if strings.HasSuffix(text, "G") {
		v, ok := intOrNone(text[:len(text)-1])
		if !ok {
			return 0, false
		}
		return mul1024(v)
	}
	if strings.HasSuffix(text, "M") || strings.HasSuffix(text, "MB") {
		// text.rstrip("B").rstrip("M"): every trailing B, then every trailing M
		return intOrNone(strings.TrimRight(strings.TrimRight(text, "B"), "M"))
	}
	return intOrNone(text)
}

func (in *Instance) MemoryMB() (int, bool) { return parseRAMSize(in.Config.Value("hw.ramSize")) } // 4096 / 4096M / 4096MB / 4G
func (in *Instance) CPU() (int, bool)      { return intOrNone(in.Config.Value("hw.cpu.ncore")) }  // hw.cpu.ncore

// FPS is hw.lcd.vsync, DefaultFPS when absent / bad (and when 0: AUTO-MAS writes `or DEFAULT_FPS`); the emulator
// boots at 60 Hz without the key.
func (in *Instance) FPS() int {
	if v, ok := intOrNone(in.Config.Value("hw.lcd.vsync")); ok && v != 0 {
		return v
	}
	return DefaultFPS
}

// DataPartitionGB is disk.dataPartition.size: 64G / 65536M (M rounded down to whole GB); anything else, "64GB"
// included, is ok=false (AUTO-MAS data_partition_gb).
func (in *Instance) DataPartitionGB() (int, bool) {
	raw := strings.ToUpper(root.PyStrip(in.Config.Value("disk.dataPartition.size")))
	switch {
	case strings.HasSuffix(raw, "G"):
		return intOrNone(raw[:len(raw)-1])
	case strings.HasSuffix(raw, "M"):
		v, ok := intOrNone(raw[:len(raw)-1])
		if !ok {
			return 0, false
		}
		return floorDiv1024(v), true
	}
	return 0, false
}

// Display returns the size and density the instance will boot with (after DisplayChanges).
func (in *Instance) Display() (width, height, dpi int) {
	ch := DisplayChanges(in.Config)
	width, _ = strconv.Atoi(ch[0][1])
	height, _ = strconv.Atoi(ch[1][1])
	dpi, _ = strconv.Atoi(ch[2][1])
	return width, height, dpi
}

func IniPath(r *root.Root, i int) string    { return filepath.Join(r.AvdHome(), root.AvdName(i)+".ini") } // <root>\avd\mas_<i>.ini
func Dir(r *root.Root, i int) string        { return filepath.Join(r.AvdHome(), root.AvdName(i)+".avd") } // <root>\avd\mas_<i>.avd
func ConfigPath(r *root.Root, i int) string { return filepath.Join(Dir(r, i), "config.ini") }             // <root>\avd\mas_<i>.avd\config.ini

// indexOfIniName matches AUTO-MAS's glob "mas_*.ini" (case-insensitive, as pathlib globs on Windows) and its
// stem[len("mas_"):].isdecimal() check: "mas_3.ini" and "MAS_03.INI" are both index 3. Only ASCII digits are taken
// (Python's isdecimal also takes other scripts' digits, which no tool writes).
func indexOfIniName(name string) (int, bool) {
	const prefix, suffix = root.AvdNamePrefix, ".ini"
	if len(name) <= len(prefix)+len(suffix) ||
		!asciiEqualFold(name[:len(prefix)], prefix) || !asciiEqualFold(name[len(name)-len(suffix):], suffix) {
		return 0, false
	}
	digits := name[len(prefix) : len(name)-len(suffix)]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// asciiEqualFold compares ignoring ASCII case only (strings.EqualFold would also match "ſ" with "s", which
// Windows' and Python's case folding do not).
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// List scans avd\mas_<digits>.ini that have a config.ini, sorted by index. No avd dir = empty list.
//
// Like AUTO-MAS list_instances: any number counts (also 5 or 12, which no tool creates), and an unreadable
// config.ini still lists the instance, with an empty config. An avd directory that exists but cannot be listed is
// an error here (AUTO-MAS's glob would silently find nothing).
func List(r *root.Root) ([]*Instance, error) {
	home := r.AvdHome()
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return []*Instance{}, nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不了实例目录 %s：%v", home, err))
	}
	found := map[int]*Instance{}
	for _, e := range entries {
		i, ok := indexOfIniName(e.Name())
		if !ok {
			continue
		}
		cfgPath := ConfigPath(r, i)
		if !isRegularFile(cfgPath) {
			continue
		}
		cfg, err := ReadIni(cfgPath)
		if err != nil {
			cfg = NewIni() // read_ini: unreadable = {}
		}
		name := root.AvdName(i)
		title := cfg.Value("avd.ini.displayname")
		if title == "" {
			title = name
		}
		found[i] = &Instance{Index: i, Name: name, Title: title, Dir: Dir(r, i), Config: cfg}
	}
	out := make([]*Instance, 0, len(found))
	for _, in := range found {
		out = append(out, in)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out, nil
}

// Get is one instance; errs.NotFound when it does not exist. It is List's entry for i, so it finds exactly what
// AUTO-MAS's _instance finds.
func Get(r *root.Root, i int) (*Instance, error) {
	list, err := List(r)
	if err != nil {
		return nil, err
	}
	for _, in := range list {
		if in.Index == i {
			return in, nil
		}
	}
	return nil, notFound(i)
}

func notFound(i int) error { return errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i) }

// lockInstance takes root.LockInstanceConfig of i. noDir: the avd directory does not exist (no lock taken, nothing on
// disk to guard); err is nil then.
func lockInstance(r *root.Root, i int) (l *root.InstanceLock, noDir bool, err error) {
	l, err = r.LockInstanceConfig(i)
	if errs.CodeOf(err) == errs.NotFound {
		return nil, true, nil
	}
	return l, false, err
}

// ---- options -----------------------------------------------------------------------------------------------------

// ParseMemoryMB accepts user input "4096", "4096M", "4096MB", "4G", "4GB" (case-insensitive) and returns MB.
// Validation against MemoryChoicesMB is separate.
func ParseMemoryMB(s string) (int, error) {
	bad := errs.Newf(errs.InvalidArgument, "内存要写成 6144、6144M 或 6G，收到「%s」", s)
	t := strings.ToUpper(strings.TrimSpace(s))
	gb := false
	switch {
	case strings.HasSuffix(t, "GB"):
		t, gb = t[:len(t)-2], true
	case strings.HasSuffix(t, "G"):
		t, gb = t[:len(t)-1], true
	case strings.HasSuffix(t, "MB"):
		t = t[:len(t)-2]
	case strings.HasSuffix(t, "M"):
		t = t[:len(t)-1]
	}
	t = strings.TrimSpace(t)
	// at most 7 digits: no overflow below, and nothing near that size is a memory choice anyway
	if t == "" || len(t) > 7 {
		return 0, bad
	}
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return 0, bad
		}
	}
	n, _ := strconv.Atoi(t)
	if gb {
		n *= 1024
	}
	return n, nil
}

// MemoryFor is the -memory of every boot: instances.<i>.memoryMb when it is an integer in MemoryChoicesMB, else
// DefaultMemoryMB (old "auto by game" instances).
func MemoryFor(r *root.Root, i int) int {
	return memoryForMeta(r.InstanceMeta(i))
}

// memoryForMeta is AUTO-MAS memory_for: isinstance(explicit, int) takes integer literals only (not 4096.0, not
// "4096"; true is an int in Python but no choice).
func memoryForMeta(meta *jsonord.Obj) int {
	if n, ok := meta.Get("memoryMb").Int(); ok && slices.Contains(MemoryChoicesMB, int(n)) {
		return int(n)
	}
	return DefaultMemoryMB
}

func joinInts(values []int, format string, div int) string {
	parts := make([]string, len(values))
	for k, v := range values {
		parts[k] = fmt.Sprintf(format, v/div)
	}
	return strings.Join(parts, " / ")
}

// ValidateOptions applies defaults to nil and checks the steps; errs.InvalidArgument with the AUTO-MAS wording.
func ValidateOptions(memoryMB, cpu, dataGB *int) (int, int, int, error) {
	memory, cores, data := DefaultMemoryMB, DefaultCPU, DefaultDataPartitionGB
	if memoryMB != nil {
		memory = *memoryMB
	}
	if cpu != nil {
		cores = *cpu
	}
	if dataGB != nil {
		data = *dataGB
	}
	if !slices.Contains(MemoryChoicesMB, memory) {
		return 0, 0, 0, errs.New(errs.InvalidArgument, "魔改 AVD 的内存只能选 "+joinInts(MemoryChoicesMB, "%d GB", 1024))
	}
	if !slices.Contains(CPUChoices, cores) {
		return 0, 0, 0, errs.New(errs.InvalidArgument, "魔改 AVD 的 CPU 核数只能选 "+joinInts(CPUChoices, "%d", 1))
	}
	if data < MinDataPartitionGB || data > MaxDataPartitionGB {
		return 0, 0, 0, errs.Newf(errs.InvalidArgument, "数据盘上限只能在 %d–%d GB 之间", MinDataPartitionGB,
			MaxDataPartitionGB)
	}
	return memory, cores, data, nil
}

// ValidateFPS: nil means DefaultFPS; anything not in FPSChoices is errs.InvalidArgument.
func ValidateFPS(fps *int) (int, error) {
	value := DefaultFPS
	if fps != nil {
		value = *fps
	}
	if !slices.Contains(FPSChoices, value) {
		return 0, errs.New(errs.InvalidArgument, "魔改 AVD 的帧率只能选 "+joinInts(FPSChoices, "%d", 1))
	}
	return value, nil
}

// isLineBreak is every character str.splitlines() splits at: a title holding one would break config.ini apart.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// validateTitle refuses line breaks (AUTO-MAS does not check; its UI cannot type them, a command line can).
func validateTitle(title string) error {
	if strings.ContainsFunc(title, isLineBreak) {
		return errs.New(errs.InvalidArgument, "实例标题不能包含换行")
	}
	return nil
}

// ---- create / update / delete ------------------------------------------------------------------------------------

type CreateOptions struct {
	Title           string // "" = the AVD name
	MemoryMB        int
	CPU             int
	DataPartitionGB int
	FPS             int
}

// Create writes mas_<i>.ini and mas_<i>.avd\config.ini exactly like AUTO-MAS create_instance_files (template +
// avd.id, avd.name, avd.ini.displayname, disk.dataPartition.size=<n>G, hw.cpu.ncore, hw.ramSize=<n>M, hw.lcd.width,
// hw.lcd.height, hw.lcd.density, hw.lcd.vsync, image.sysdir.1 appended in that order; the .ini holds
// avd.ini.encoding, path=<absolute dir>, path.rel=avd\mas_<i>.avd, target) and sets instances.<i> = memoryMb,
// initialized=false, createdAt (local ISO seconds). Options must already be validated. errs.InvalidArgument for a
// bad index, errs.AlreadyExists when the .ini or the directory exists.
//
// "Appended" is a dict update: hw.lcd.vsync is already in the template, so it keeps its place there. The options are
// checked again anyway (same rules as ValidateOptions / ValidateFPS; zero values fail). Before writing anything
// mas-avd.json must be readable, and when it cannot be updated at the end the new files are removed again, so a
// failed Create leaves nothing behind (AUTO-MAS would keep the files, or overwrite an unreadable mas-avd.json).
func Create(r *root.Root, i int, opt CreateOptions) (*Instance, error) {
	if !root.ValidIndex(i) {
		return nil, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", i)
	}
	memory, cpu, dataGB, err := ValidateOptions(&opt.MemoryMB, &opt.CPU, &opt.DataPartitionGB)
	if err != nil {
		return nil, err
	}
	fps, err := ValidateFPS(&opt.FPS)
	if err != nil {
		return nil, err
	}
	if err := validateTitle(opt.Title); err != nil {
		return nil, err
	}
	name := root.AvdName(i)
	dir, iniPath := Dir(r, i), IniPath(r, i)
	if pathExists(iniPath) || pathExists(dir) {
		return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在", name)
	}
	if _, err := r.ReadMetadata(); err != nil {
		return nil, err
	}

	title := opt.Title
	if title == "" {
		title = name
	}
	cfg := NewIni()
	for _, kv := range ConfigTemplate {
		cfg.Set(kv[0], kv[1])
	}
	for _, kv := range [][2]string{
		{"avd.id", name},
		{"avd.name", name},
		{"avd.ini.displayname", title},
		{"disk.dataPartition.size", strconv.Itoa(dataGB) + "G"},
		{"hw.cpu.ncore", strconv.Itoa(cpu)},
		{"hw.ramSize", strconv.Itoa(memory) + "M"},
		{"hw.lcd.width", strconv.Itoa(ScreenWidth)},
		{"hw.lcd.height", strconv.Itoa(ScreenHeight)},
		{"hw.lcd.density", strconv.Itoa(ScreenDensity)},
		{"hw.lcd.vsync", strconv.Itoa(fps)},
		{"image.sysdir.1", SystemImageSysdir},
	} {
		cfg.Set(kv[0], kv[1])
	}
	ini := NewIni()
	ini.Set("avd.ini.encoding", "UTF-8")
	ini.Set("path", dir)
	ini.Set("path.rel", root.AvdDir+`\`+name+".avd")
	ini.Set("target", TargetAPI)

	if err := os.MkdirAll(r.AvdHome(), 0o755); err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了目录 %s：%v", r.AvdHome(), err))
	}
	// Held until instances.<i> is written: an update or delete of the same index waits for the whole instance.
	l, err := r.LockInstanceConfig(i)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	if pathExists(iniPath) {
		return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在", name)
	}
	// Mkdir, not MkdirAll: a second creator racing for the same index loses here instead of sharing the directory.
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在", name)
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了目录 %s：%v", dir, err))
	}
	rollback := func() {
		os.RemoveAll(dir)
		os.Remove(iniPath)
	}
	if err := WriteIni(filepath.Join(dir, "config.ini"), cfg); err != nil {
		rollback()
		return nil, err
	}
	if err := WriteIni(iniPath, ini); err != nil {
		rollback()
		return nil, err
	}
	createdAt := nowISO()
	err = r.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set("memoryMb", jsonord.NewInt(int64(memory)))
		meta.Set("initialized", jsonord.NewBool(false))
		meta.Set("createdAt", jsonord.NewString(createdAt))
		return nil
	})
	if err != nil {
		rollback()
		return nil, err
	}
	if in, err := Get(r, i); err == nil {
		return in, nil
	}
	return &Instance{Index: i, Name: name, Title: title, Dir: dir, Config: cfg}, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// FreeIndex is the smallest valid index that has no instance and whose three ports busy() reports free.
// errs.Busy "魔改 AVD 实例已满（端口段 20000–20100 最多 9 台）" when there is none. A nil busy skips the port check.
func FreeIndex(existing map[int]bool, busy func(ports []int) []int) (int, error) {
	for i := 0; i <= root.MaxIndex; i++ {
		if !root.ValidIndex(i) || existing[i] {
			continue
		}
		c := root.ConsolePort(i)
		if busy != nil && len(busy([]int{c, c + 1, c + 2})) > 0 {
			continue
		}
		return i, nil
	}
	return 0, errs.New(errs.Busy, "魔改 AVD 实例已满（端口段 20000–20100 最多 9 台）")
}

type UpdateOptions struct {
	Title    *string
	MemoryMB *int
	CPU      *int
	FPS      *int
	// DataPartitionGB raises disk.dataPartition.size (modavd only; AUTO-MAS cannot change it): within
	// MinDataPartitionGB..MaxDataPartitionGB and never below the current value. The emulator (sdk-mas36 on) grows the
	// disk image at the next boot without touching the data, and the boot's after-boot step grows the file system.
	DataPartitionGB *int
}

// Update writes the changed keys like AUTO-MAS write_instance_settings: hw.ramSize=<n>M and hw.cpu.ncore always
// (current values when not changed), hw.lcd.vsync only when FPS is given, avd.ini.displayname when Title is given,
// disk.dataPartition.size when DataPartitionGB is given, and instances.<i>.memoryMb always. Validates first
// (memory / cpu / fps steps). Returns the applied changes by option name ("title", "memory_mb", "cpu", "fps",
// "data_partition_gb"). Takes effect at the next boot.
//
// As in AUTO-MAS the unchanged values are validated too: memory is MemoryFor (old instances get 6144 written), cores
// hw.cpu.ncore (DefaultCPU when unreadable), and the instance's data partition must still be within range unless a
// new DataPartitionGB is given (an old 8 GB instance can be raised that way). A given empty Title means the AVD name,
// like Create.
//
// The whole update runs under the instance's root.LockInstanceConfig, from reading the current values (they are
// written back as they were read) to instances.<i>.
func Update(r *root.Root, i int, opt UpdateOptions) (map[string]any, error) {
	l, noDir, err := lockInstance(r, i)
	if err != nil {
		return nil, err
	}
	if noDir {
		return nil, notFound(i)
	}
	defer l.Unlock()
	in, err := Get(r, i)
	if err != nil {
		return nil, err
	}
	if opt.Title != nil {
		if err := validateTitle(*opt.Title); err != nil {
			return nil, err
		}
	}
	var fps int
	if opt.FPS != nil {
		if fps, err = ValidateFPS(opt.FPS); err != nil {
			return nil, err
		}
	}
	memoryArg := opt.MemoryMB
	if memoryArg == nil {
		current := MemoryFor(r, i)
		memoryArg = &current
	}
	cpuArg := opt.CPU
	if cpuArg == nil {
		if n, ok := in.CPU(); ok {
			cpuArg = &n
		}
	}
	var dataArg *int
	currentData, currentOK := in.DataPartitionGB()
	if currentOK {
		dataArg = &currentData
	}
	if opt.DataPartitionGB != nil {
		dataArg = opt.DataPartitionGB
	}
	memory, cpu, data, err := ValidateOptions(memoryArg, cpuArg, dataArg)
	if err != nil {
		return nil, err
	}
	// The image can only grow: the emulator never shrinks it, and a file system cannot be shrunk online.
	if opt.DataPartitionGB != nil && currentOK && data < currentData {
		return nil, errs.Newf(errs.InvalidArgument, "数据盘上限只能改大：实例 %d 现在是 %d GB，不能改成 %d GB", i,
			currentData, data)
	}
	if _, err := r.ReadMetadata(); err != nil {
		return nil, err
	}

	changes := [][2]string{{"hw.ramSize", strconv.Itoa(memory) + "M"}, {"hw.cpu.ncore", strconv.Itoa(cpu)}}
	if opt.FPS != nil {
		changes = append(changes, [2]string{"hw.lcd.vsync", strconv.Itoa(fps)})
	}
	if opt.DataPartitionGB != nil {
		changes = append(changes, [2]string{"disk.dataPartition.size", strconv.Itoa(data) + "G"})
	}
	title := ""
	if opt.Title != nil {
		title = *opt.Title
		if title == "" {
			title = in.Name
		}
		changes = append(changes, [2]string{"avd.ini.displayname", title})
	}
	if err := writeInstanceConfig(r, i, changes); err != nil {
		return nil, err
	}
	// memory goes into the metadata every time: boots pass it as -memory, so old "auto" instances get a fixed value
	if err := r.UpdateInstanceMeta(i, map[string]*jsonord.Value{"memoryMb": jsonord.NewInt(int64(memory))}); err != nil {
		return nil, err
	}
	applied := map[string]any{}
	if opt.Title != nil {
		applied["title"] = title
	}
	if opt.MemoryMB != nil {
		applied["memory_mb"] = memory
	}
	if opt.CPU != nil {
		applied["cpu"] = cpu
	}
	if opt.FPS != nil {
		applied["fps"] = fps
	}
	if opt.DataPartitionGB != nil {
		applied["data_partition_gb"] = data
	}
	return applied, nil
}

// configUnreadable is AUTO-MAS's "实例 mas_<i> 的配置读不出" (raised for a missing, unreadable or empty config.ini).
// The code is errs.NotFound when the file (or the instance directory) is not there, else errs.Internal.
func configUnreadable(i int, cause error) error {
	code := errs.Internal
	if errs.CodeOf(cause) == errs.NotFound {
		code = errs.NotFound
	}
	return errs.Wrap(code, cause, fmt.Sprintf("实例 %s 的配置读不出", root.AvdName(i)))
}

// writeInstanceConfig is AUTO-MAS write_instance_config: read config.ini (refusing an empty one), apply changes in
// order, write it back. The caller holds the instance's root.LockInstanceConfig.
func writeInstanceConfig(r *root.Root, i int, changes [][2]string) error {
	path := ConfigPath(r, i)
	cfg, err := ReadIni(path)
	if err != nil || cfg.Len() == 0 {
		return configUnreadable(i, err)
	}
	for _, kv := range changes {
		cfg.Set(kv[0], kv[1])
	}
	return WriteIni(path, cfg)
}

// Delete removes mas_<i>.avd\ (with the data disk), mas_<i>.ini and instances.<i>. The caller checks it is stopped.
// Like AUTO-MAS delete_instance_files it takes whatever of the three is there, so it also clears what a delete or a
// create that stopped half-way left behind (an .ini without the directory, a directory without config.ini).
//
// mas-avd.json must be readable first (it is never overwritten when it is not, so instances.<i> could not go). An
// instance directory that is a link or junction is refused, as Python's shutil.rmtree refuses it: deleting through it
// would wipe data stored elsewhere. Runs under the instance's root.LockInstanceConfig, whose file goes too once the
// delete succeeded.
func Delete(r *root.Root, i int) error {
	if i < 0 {
		return errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", i)
	}
	if _, err := r.ReadMetadata(); err != nil {
		return err
	}
	l, noDir, err := lockInstance(r, i)
	if err != nil {
		return err
	}
	if noDir {
		// no avd directory: only instances.<i> can be left
		return r.RemoveInstanceMeta(i)
	}
	err = deleteInstanceFiles(r, i)
	l.Unlock()
	if err == nil {
		// Best effort, and only once nobody holds it: a lock file another writer still has open cannot be deleted
		// (that writer then finds no instance).
		os.Remove(r.ConfigLockPath(i))
	}
	return err
}

func deleteInstanceFiles(r *root.Root, i int) error {
	dir := Dir(r, i)
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return errs.Newf(errs.Unsupported, "实例目录 %s 是链接，不能跟着它删除", dir).
				WithAdvice("请手动删除这个链接和它指向的数据")
		}
		if err := os.RemoveAll(dir); err != nil {
			return errs.Wrap(errs.Internal, err, fmt.Sprintf("删除 %s 失败：%v", dir, err)).
				WithAdvice("可能有文件正被占用（实例还开着？）；关掉占用它的程序后再试")
		}
	}
	if err := os.Remove(IniPath(r, i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("删除 %s 失败：%v", IniPath(r, i), err))
	}
	return r.RemoveInstanceMeta(i)
}

// ---- display -----------------------------------------------------------------------------------------------------

// DisplayChanges is the fixed 720p for cfg keeping its orientation (portrait when height > width), plus
// hw.mainKeys=yes: hw.lcd.width, hw.lcd.height, hw.lcd.density, hw.mainKeys in that order.
func DisplayChanges(cfg *Ini) [][2]string {
	w, _ := intOrNone(cfg.Value("hw.lcd.width"))
	h, _ := intOrNone(cfg.Value("hw.lcd.height"))
	width, height := ScreenWidth, ScreenHeight
	if h > w {
		width, height = ScreenHeight, ScreenWidth
	}
	return [][2]string{
		{"hw.lcd.width", strconv.Itoa(width)},
		{"hw.lcd.height", strconv.Itoa(height)},
		{"hw.lcd.density", strconv.Itoa(ScreenDensity)},
		{"hw.mainKeys", "yes"},
	}
}

// ApplyDisplay writes DisplayChanges into config.ini when any differs; returns whether it wrote. Only while stopped.
// Under the instance's root.LockInstanceConfig, like Update.
func ApplyDisplay(r *root.Root, i int) (bool, error) {
	l, noDir, err := lockInstance(r, i)
	if err != nil {
		return false, err
	}
	if noDir {
		return false, configUnreadable(i, notFound(i))
	}
	defer l.Unlock()
	path := ConfigPath(r, i)
	cfg, err := ReadIni(path)
	if err != nil || cfg.Len() == 0 {
		return false, configUnreadable(i, err)
	}
	changes := DisplayChanges(cfg)
	same := true
	for _, kv := range changes {
		if v, ok := cfg.Get(kv[0]); !ok || v != kv[1] {
			same = false
			break
		}
	}
	if same {
		return false, nil
	}
	for _, kv := range changes {
		cfg.Set(kv[0], kv[1])
	}
	if err := WriteIni(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}
