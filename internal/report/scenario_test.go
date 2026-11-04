package report

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake computers and roots of testdata/scenarios.json (gen_golden.ps1 feeds the same data to the scripts).

// flexStr is a JSON string or number, as text.
type flexStr string

func (s *flexStr) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*s = ""
	case string:
		*s = flexStr(x)
	default:
		*s = flexStr(strings.TrimSpace(string(b)))
	}
	return nil
}

type scenarioShell struct {
	Out, Err string
	Exit     *int
}

type scenarioSerial struct {
	Features string
	Shell    map[string]scenarioShell
	Exec     map[string]struct{ B64 string }
}

type scenarioAdb struct {
	Devices string
	Serials map[string]scenarioSerial
}

type scenarioFile struct {
	Path, Text, B64, Mtime string
	Size                   *int
	Hidden, Dir            bool
}

type scenario struct {
	CollectOnly  bool
	Now          string
	Root         string
	Env          map[string]string
	Limits       *struct{ LogCap, MasTail, DumpMax int64 }
	ClipboardErr string

	OS *struct {
		Caption, Version, BuildNumber, OSArchitecture string
		TotalVisibleMemorySize, FreePhysicalMemory    float64
	}
	CurrentVersion *struct{ DisplayVersion, ReleaseId, CurrentBuild, UBR flexStr }
	CPUs           []struct {
		Name                                     string
		NumberOfCores, NumberOfLogicalProcessors *int64
		VirtualizationFirmwareEnabled            *bool
	}
	CS   *struct{ HypervisorPresent *bool }
	GPUs []struct {
		Name, DriverVersion, PNPDeviceID string
		DriverDate                       *string
	}
	Features []struct {
		Name         string
		InstallState *int64
	}
	AV []struct {
		DisplayName  string
		ProductState int64
	}
	Disk  struct{ BusType, MediaType string }
	Drive struct {
		AvailableFreeSpace, TotalSize uint64
		DriveFormat                   string
	}
	Uninstall struct{ HKLM, WOW, HKCU []UninstallEntry }
	Processes []struct {
		ProcessId                                       uint32
		Name, CreationDate, CommandLine, ExecutablePath string
	}
	Listeners []struct {
		LocalAddress  string
		LocalPort     flexStr
		OwningProcess uint32
	}
	Events []struct {
		TimeCreated, ProviderName, Message string
		Id                                 int
	}
	Run map[string]struct {
		Stdout, Stderr, StartErr string
		Exit                     int
		Timeout                  bool
	}
	Adb   scenarioAdb
	Files []scenarioFile

	// <query>Err: the query fails with this message
	OSErr, CurrentVersionErr, CPUsErr, CSErr, GPUsErr, FeaturesErr, AVErr, DiskErr, DriveErr, ProcessesErr,
	ListenersErr, EventsErr string
}

// scenarioData resolves shallow overrides so each failure scenario changes just the query being tested.
func scenarioData(t *testing.T, all map[string]json.RawMessage, name string) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(all[name], &fields); err != nil {
		t.Fatal(err)
	}
	if raw, ok := fields["extends"]; ok {
		var base string
		if err := json.Unmarshal(raw, &base); err != nil {
			t.Fatal(err)
		}
		var inherited map[string]json.RawMessage
		if err := json.Unmarshal(scenarioData(t, all, base), &inherited); err != nil {
			t.Fatal(err)
		}
		for key, value := range fields {
			inherited[key] = value
		}
		fields = inherited
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collectScenarios(t *testing.T) []string {
	b, err := os.ReadFile("testdata/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range all {
		if name != "comment" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// fixture is one scenario set up on disk.
type fixture struct {
	sc      *scenario
	base    string
	root    string
	env     map[string]string
	adb     *fakeAdb
	adbPort int
	host    *fakeHost
	now     time.Time
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// loadScenario reads scenario name of testdata/scenarios.json for base (placeholders filled in), with rootRel
// instead of its root when not "".
func loadScenario(t *testing.T, name, base, rootRel string, adbPort int) *scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	var head struct{ Root string }
	if err := json.Unmarshal(scenarioData(t, probe, name), &head); err != nil {
		t.Fatalf("scenario %s: %v", name, err)
	}
	if rootRel == "" {
		rootRel = head.Root
	}
	text := string(raw)
	for _, kv := range [][2]string{
		{"ROOT", filepath.Join(base, rootRel)}, {"MAS", filepath.Join(base, "mas")}, {"BASE", base},
	} {
		text = strings.ReplaceAll(text, "{{"+kv[0]+"}}", jsonEscape(kv[1]))
		text = strings.ReplaceAll(text, "{{"+kv[0]+"_J}}", jsonEscape(jsonEscape(kv[1])))
	}
	text = strings.ReplaceAll(text, "{{ADBPORT}}", strconv.Itoa(adbPort))
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &all); err != nil {
		t.Fatal(err)
	}
	sc := &scenario{}
	if err := json.Unmarshal(scenarioData(t, all, name), sc); err != nil {
		t.Fatalf("scenario %s: %v", name, err)
	}
	sc.Root = rootRel
	return sc
}

const wallClock = "2006-01-02 15:04:05"

// buildFiles writes the scenario's files under base, as gen_golden.ps1 does.
func buildFiles(t *testing.T, sc *scenario, base, root string) {
	t.Helper()
	for _, f := range sc.Files {
		var p string
		switch {
		case f.Path == "root":
			p = root
		case strings.HasPrefix(f.Path, "root/"):
			p = filepath.Join(root, filepath.FromSlash(f.Path[5:]))
		default:
			p = filepath.Join(base, filepath.FromSlash(f.Path))
		}
		if f.Dir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var data []byte
		switch {
		case f.Size != nil:
			data = []byte(strings.Repeat("x", *f.Size))
		case f.B64 != "":
			var err error
			if data, err = base64.StdEncoding.DecodeString(f.B64); err != nil {
				t.Fatal(err)
			}
		default:
			data = []byte(f.Text)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := f.Mtime
		if mt == "" {
			mt = "2026-01-10 08:00:00"
		}
		tm, err := time.ParseInLocation(wallClock, mt, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, tm, tm); err != nil {
			t.Fatal(err)
		}
		if f.Hidden {
			setHidden(t, p)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
}

// newFixture sets scenario name up under base: its files, a fake adb server and the fake host.
func newFixture(t *testing.T, name, base, rootRel string) *fixture {
	t.Helper()
	// the port is only known once the server listens; the scenario is read again with it
	sc0 := loadScenario(t, name, base, rootRel, 0)
	adb := startFakeAdb(t, sc0.Adb)
	sc := loadScenario(t, name, base, rootRel, adb.port())
	root := filepath.Join(base, sc.Root)
	buildFiles(t, sc, base, root)
	env := map[string]string{}
	for k, v := range sc.Env {
		if m := shortEnv.FindStringSubmatch(v); m != nil {
			dir := filepath.Join(base, m[1])
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			s := shortName(t, dir)
			if s == "" {
				t.Skipf("scenario %s: no 8.3 short name for %s on this volume", name, dir)
			}
			v = s + m[2]
		}
		env[k] = filepath.Join(base, v)
	}
	now, err := time.ParseInLocation(wallClock, sc.Now, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{sc: sc, base: base, root: root, env: env, adb: adb, adbPort: adb.port(), host: &fakeHost{sc: sc},
		now: now}
}

// shortEnv is an environment value "<dir>\rest" of a scenario: dir (directly under base) by its 8.3 short name, the
// way %TEMP% is the 8.3 short form (PROFIL~1 style) of %LOCALAPPDATA%\Temp.
var shortEnv = regexp.MustCompile(`^<([^>]+)>(.*)$`)

// getenv is the scenario's environment (LOCALAPPDATA, TEMP under base).
func (f *fixture) getenv(k string) string { return f.env[k] }

// ---- the fake host --------------------------------------------------------------------------------------------

type fakeHost struct {
	sc *scenario

	mu   sync.Mutex
	runs []string // the programs Run was asked to start
}

func failIf(msg string) error {
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (h *fakeHost) OS(context.Context) (OSInfo, error) {
	if err := failIf(h.sc.OSErr); err != nil {
		return OSInfo{}, err
	}
	o := h.sc.OS
	return OSInfo{Caption: o.Caption, Version: o.Version, BuildNumber: o.BuildNumber, OSArchitecture: o.OSArchitecture,
		TotalVisibleKB: o.TotalVisibleMemorySize, FreePhysicalKB: o.FreePhysicalMemory}, nil
}

func (h *fakeHost) Processors(context.Context) ([]Processor, error) {
	if err := failIf(h.sc.CPUsErr); err != nil {
		return nil, err
	}
	var out []Processor
	for _, c := range h.sc.CPUs {
		out = append(out, Processor{Name: c.Name, Cores: c.NumberOfCores, Threads: c.NumberOfLogicalProcessors,
			VirtFirmware: c.VirtualizationFirmwareEnabled})
	}
	return out, nil
}

func (h *fakeHost) ComputerSystem(context.Context) (ComputerSystem, error) {
	if err := failIf(h.sc.CSErr); err != nil {
		return ComputerSystem{}, err
	}
	return ComputerSystem{HypervisorPresent: h.sc.CS.HypervisorPresent}, nil
}

func (h *fakeHost) VideoControllers(context.Context) ([]VideoController, error) {
	if err := failIf(h.sc.GPUsErr); err != nil {
		return nil, err
	}
	var out []VideoController
	for _, g := range h.sc.GPUs {
		v := VideoController{Name: g.Name, DriverVersion: g.DriverVersion, PNPDeviceID: g.PNPDeviceID}
		if g.DriverDate != nil {
			v.DriverDate = *g.DriverDate
		}
		out = append(out, v)
	}
	return out, nil
}

func (h *fakeHost) OptionalFeatures(context.Context) ([]OptionalFeature, error) {
	if err := failIf(h.sc.FeaturesErr); err != nil {
		return nil, err
	}
	var out []OptionalFeature
	for _, f := range h.sc.Features {
		out = append(out, OptionalFeature{Name: f.Name, InstallState: f.InstallState})
	}
	return out, nil
}

func (h *fakeHost) Antivirus(context.Context) ([]Antivirus, error) {
	if err := failIf(h.sc.AVErr); err != nil {
		return nil, err
	}
	var out []Antivirus
	for _, a := range h.sc.AV {
		out = append(out, Antivirus{DisplayName: a.DisplayName, ProductState: a.ProductState})
	}
	return out, nil
}

func (h *fakeHost) DiskKind(context.Context, string) (string, string, error) {
	if err := failIf(h.sc.DiskErr); err != nil {
		return "", "", err
	}
	return h.sc.Disk.BusType, h.sc.Disk.MediaType, nil
}

func (h *fakeHost) AppEvents(context.Context) ([]AppEvent, error) {
	if err := failIf(h.sc.EventsErr); err != nil {
		return nil, err
	}
	var out []AppEvent
	for _, e := range h.sc.Events {
		out = append(out, AppEvent{Time: e.TimeCreated, ID: e.Id, Provider: e.ProviderName, Message: e.Message})
	}
	return out, nil
}

func (h *fakeHost) CurrentVersion() (CurrentVersion, error) {
	if err := failIf(h.sc.CurrentVersionErr); err != nil {
		return CurrentVersion{}, err
	}
	cv := h.sc.CurrentVersion
	return CurrentVersion{DisplayVersion: string(cv.DisplayVersion), ReleaseID: string(cv.ReleaseId),
		CurrentBuild: string(cv.CurrentBuild), UBR: string(cv.UBR)}, nil
}

func (h *fakeHost) UninstallEntries() ([]UninstallEntry, error) {
	u := h.sc.Uninstall
	return append(append(append([]UninstallEntry(nil), u.HKLM...), u.WOW...), u.HKCU...), nil
}

func (h *fakeHost) Processes(match func(string) bool) ([]Process, error) {
	if err := failIf(h.sc.ProcessesErr); err != nil {
		return nil, err
	}
	var out []Process
	for _, p := range h.sc.Processes {
		if !match(p.Name) {
			continue
		}
		q := Process{PID: p.ProcessId, Name: p.Name, CommandLine: p.CommandLine, ExePath: p.ExecutablePath}
		if p.CreationDate != "" {
			tm, err := time.ParseInLocation(wallClock, p.CreationDate, time.Local)
			if err != nil {
				return nil, err
			}
			q.Started = tm
		}
		out = append(out, q)
	}
	return out, nil
}

func (h *fakeHost) Listeners() ([]Listener, error) {
	if err := failIf(h.sc.ListenersErr); err != nil {
		return nil, err
	}
	var out []Listener
	for _, l := range h.sc.Listeners {
		port, err := strconv.Atoi(string(l.LocalPort))
		if err != nil {
			return nil, err
		}
		out = append(out, Listener{Addr: l.LocalAddress, Port: port, PID: l.OwningProcess})
	}
	return out, nil
}

func (h *fakeHost) ProductVersion(string) (string, error) { return "", nil }

func (h *fakeHost) Run(_ context.Context, exe string, args []string, _ time.Duration) (RunResult, error) {
	h.mu.Lock()
	h.runs = append(h.runs, exe)
	h.mu.Unlock()
	r, ok := h.sc.Run[strings.ToLower(filepath.Base(exe))]
	if !ok {
		return RunResult{}, fmt.Errorf("test: no program %s in the scenario", exe)
	}
	if r.StartErr != "" {
		return RunResult{}, errors.New(r.StartErr)
	}
	if r.Timeout {
		return RunResult{TimedOut: true}, nil
	}
	return RunResult{ExitCode: r.Exit, Stdout: []byte(r.Stdout), Stderr: []byte(r.Stderr)}, nil
}

func (h *fakeHost) Drive(root string) (Drive, error) {
	if err := failIf(h.sc.DriveErr); err != nil {
		return Drive{}, err
	}
	d := h.sc.Drive
	return Drive{Name: root, Free: d.AvailableFreeSpace, Total: d.TotalSize, Format: d.DriveFormat}, nil
}

func (h *fakeHost) programs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.runs...)
}
