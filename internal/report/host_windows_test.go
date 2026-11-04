package report

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/win"
)

// TestCIMPartsApart: every WMI class runs in a cim.ps1 process of its own, once, on first use: one that fails or
// times out (here av) loses only its own rows, and a part nobody asks for never runs.
func TestCIMPartsApart(t *testing.T) {
	answers := map[string]string{
		"os":       `[{"caption":"Microsoft Windows 11 Pro","version":"10.0.26200","build":"26200","arch":"64 位","totalKB":33554432,"freeKB":16777216}]`,
		"cpu":      `[{"name":"Test CPU","cores":8,"threads":16,"vt":true}]`,
		"cs":       `[{"hypervisor":true}]`,
		"gpu":      `[{"name":"Test GPU","driver":"32.0.15.1","pnp":"PCI\\VEN_10DE","date":"2026-01-02"}]`,
		"features": `[{"name":"HypervisorPlatform","state":1}]`,
	}
	old := cimRunner
	t.Cleanup(func() { cimRunner = old })
	var mu sync.Mutex
	runs := map[string]int{}
	cimRunner = func(ctx context.Context, part, letter string) (map[string]json.RawMessage, error) {
		mu.Lock()
		runs[part+"|"+letter]++
		mu.Unlock()
		if part == "av" {
			return nil, errors.New("WMI 查询（av）60 秒没有结果")
		}
		a, ok := answers[part]
		if !ok {
			return nil, errors.New("unexpected part " + part)
		}
		return map[string]json.RawMessage{part: json.RawMessage(a)}, nil
	}
	h := &winHost{cim: map[string]*cimRun{}}
	ctx := context.Background()
	if _, err := h.Antivirus(ctx); err == nil || !strings.Contains(err.Error(), "av") {
		t.Fatalf("av: %v", err)
	}
	o, err := h.OS(ctx)
	if err != nil || o.Caption != "Microsoft Windows 11 Pro" || o.TotalVisibleKB != 33554432 {
		t.Fatalf("os after av failed: %+v, %v", o, err)
	}
	if cpus, err := h.Processors(ctx); err != nil || len(cpus) != 1 || *cpus[0].Cores != 8 {
		t.Fatalf("cpu: %+v, %v", cpus, err)
	}
	if cs, err := h.ComputerSystem(ctx); err != nil || cs.HypervisorPresent == nil || !*cs.HypervisorPresent {
		t.Fatalf("cs: %+v, %v", cs, err)
	}
	if gpus, err := h.VideoControllers(ctx); err != nil || len(gpus) != 1 || gpus[0].DriverDate != "2026-01-02" {
		t.Fatalf("gpu: %+v, %v", gpus, err)
	}
	h.OS(ctx)
	h.Antivirus(ctx)
	mu.Lock()
	defer mu.Unlock()
	want := map[string]int{"av|": 1, "os|": 1, "cpu|": 1, "cs|": 1, "gpu|": 1} // features never asked for
	if len(runs) != len(want) {
		t.Fatalf("runs %v, want %v", runs, want)
	}
	for k, n := range want {
		if runs[k] != n {
			t.Fatalf("runs %v, want %v", runs, want)
		}
	}
}

// TestPebHelper is the child of TestProcessPathAsStarted: it waits to be killed.
func TestPebHelper(t *testing.T) {
	if os.Getenv("MODAVD_REPORT_PEB_HELPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// The path of a process is the one it was started with, as Win32_Process.ExecutablePath shows it, not the target of
// a junction on the way (a root's sdk\platform-tools is often a junction to a shared SDK).
func TestProcessPathAsStarted(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := pebImagePath(uint32(os.Getpid())); err != nil || !strings.EqualFold(p, self) {
		t.Fatalf("own path %q, %v; want %q", p, err, self)
	}

	base := testTempDir(t)
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(target, "pebhelper.exe")
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(exe)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, in)
	in.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("no junction: %v %s", err, b)
	}
	started := filepath.Join(link, "pebhelper.exe")
	cmd := exec.Command(started, "-test.run=^TestPebHelper$")
	cmd.Env = append(os.Environ(), "MODAVD_REPORT_PEB_HELPER=1")
	win.HideConsoleCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	h := &winHost{}
	var got []Process
	for i := 0; i < 50 && len(got) == 0; i++ {
		ps, err := h.Processes(func(n string) bool { return strings.EqualFold(n, "pebhelper.exe") })
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.PID == uint32(cmd.Process.Pid) {
				got = append(got, p)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatal("helper not in the process list")
	}
	if !strings.EqualFold(got[0].ExePath, started) {
		t.Errorf("path %q, want %q (as started)", got[0].ExePath, started)
	}
	if !strings.Contains(got[0].CommandLine, "-test.run=^TestPebHelper$") {
		t.Errorf("command line %q", got[0].CommandLine)
	}
}

// longPath is what [IO.Path]::GetFullPath of Windows PowerShell 5.1 gives for an existing directory: 8.3 short names
// expanded, a path without '~' as it is (case included).
func TestLongPath(t *testing.T) {
	base := testTempDir(t)
	long := filepath.Join(base, "Long Name Dir", "AndroidEmulator")
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	if p := strings.ToUpper(long); longPath(p) != p {
		t.Errorf("longPath(%q) = %q, want it unchanged", p, longPath(p))
	}
	missing := filepath.Join(base, "NOSUCH~1", "x")
	if p := longPath(missing); p != missing {
		t.Errorf("longPath(%q) = %q, want it unchanged", missing, p)
	}
	s := shortName(t, filepath.Dir(long))
	if s == "" {
		t.Skipf("no 8.3 short name for %s on this volume", filepath.Dir(long))
	}
	if p := longPath(filepath.Join(base, s, "AndroidEmulator")); p != long {
		t.Errorf("longPath of %s\\AndroidEmulator = %q, want %q", s, p, long)
	}
}
