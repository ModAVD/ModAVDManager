//go:build windows

package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/appproxy"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// TestCollectAppProxy: a running instance with a per-app proxy set also gets the daemon's state and log; the others
// get nothing more (so the scenarios without one stay what collect-info.ps1 writes).
func TestCollectAppProxy(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	setLimits(t, f.sc)
	guardDials(t, f.adbPort)

	meta := filepath.Join(f.root, root.MetadataFile)
	b, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonord.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	set := jsonord.NewObject()
	set.Obj().Set("enabled", jsonord.NewBool(true))
	v.Obj().Get("instances").Obj().Get("1").Obj().Set(appproxy.MetaKey, set)
	if err := os.WriteFile(meta, v.MarshalPython(), 0o644); err != nil {
		t.Fatal(err)
	}
	state := `{"version":"0.4.0","running":true,"rules":"ok"}`
	f.adb.data.Serials["emulator-20010"].Shell[appProxyCommand[0]] = scenarioShell{Out: state + "\n\nlog line\n"}

	_, names, files := runCollect(t, f)
	if got := string(files["live/mas_1-app-proxy.txt"]); !strings.Contains(got, state) || !strings.Contains(got, "log line") {
		t.Errorf("mas_1 app-proxy: %q", got)
	}
	for _, n := range names {
		if strings.HasSuffix(n, "-app-proxy.txt") && n != "live/mas_1-app-proxy.txt" {
			t.Errorf("%s without a per-app proxy set", n)
		}
	}
	if s := string(files["summary.txt"]); !strings.Contains(s, "  app-proxy：退出码 0，") {
		t.Errorf("summary has no app-proxy line:\n%s", s)
	}
}

func TestAppProxyInstances(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, root.MetadataFile)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"instances": {"1": {"appProxy": {"enabled": false}}, "3": {"memoryMb": 4096}, "5": {"appProxy": {}},
		"12": {"appProxy": {}}, "x": {"appProxy": {}}, "7": "bad"}}`)
	got := appProxyInstances(p)
	if len(got) != 1 || !got["mas_1"] {
		t.Errorf("%v", got)
	}
	write(`not json`)
	if got := appProxyInstances(p); len(got) != 0 {
		t.Errorf("bad file: %v", got)
	}
	if got := appProxyInstances(filepath.Join(dir, "missing.json")); len(got) != 0 {
		t.Errorf("missing file: %v", got)
	}
}
