//go:build windows

package report

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// TestBootDiagDirs: the failed boots' directories (logs\<instance>-<time>-bootfail) go into logs\ of the package, the
// newest 3 by the time in the name, no more once bootDiagCapBytes is reached (the newest always); without any,
// nothing is written to the summary (collect-info.ps1 does not know them, so the comparison with it stays as it was).
func TestBootDiagDirs(t *testing.T) {
	rootDir := testutil.TempDir(t)
	logDir := filepath.Join(rootDir, "logs")
	mk := func(name string, size int) {
		d := filepath.Join(logDir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"logcat.txt", "summary.txt"} {
			if err := os.WriteFile(filepath.Join(d, f), []byte(strings.Repeat("x", size)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	staged := func(stage string) []string {
		var out []string
		filepath.WalkDir(filepath.Join(stage, "logs"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(stage, p)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		sort.Strings(out)
		return out
	}

	// the whole logs section, as collect runs it
	collectLogs := func() *collector {
		t.Helper()
		c := &collector{root: rootDir, stage: testutil.TempDir(t)}
		if err := c.emulatorLogs(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	diagLines := func(c *collector) []string {
		var out []string
		for _, l := range c.lines {
			if strings.Contains(l, "bootfail") {
				out = append(out, l)
			}
		}
		return out
	}

	mk("mas_6-notes", 10) // not one of them
	c := collectLogs()
	if len(diagLines(c)) != 0 || len(staged(c.stage)) != 0 {
		t.Fatalf("without directories: lines %q, staged %q", diagLines(c), staged(c.stage))
	}

	mk("mas_3-20261101-100000-bootfail", 10)
	mk("mas_6-20261103-100000-bootfail", 10)
	mk("mas_3-20261104-090000-bootfail", 10)
	mk("mas_6-20261102-100000-bootfail", 10)
	c = collectLogs()
	want := []string{
		"logs/mas_3-20261104-090000-bootfail/logcat.txt", "logs/mas_3-20261104-090000-bootfail/summary.txt",
		"logs/mas_6-20261102-100000-bootfail/logcat.txt", "logs/mas_6-20261102-100000-bootfail/summary.txt",
		"logs/mas_6-20261103-100000-bootfail/logcat.txt", "logs/mas_6-20261103-100000-bootfail/summary.txt",
	}
	if got := staged(c.stage); !reflect.DeepEqual(got, want) {
		t.Errorf("staged %q\nwant %q", got, want)
	}
	if l := diagLines(c); len(l) != 1 || !strings.Contains(l[0], "共 4 个，取了最新 3 个") ||
		!strings.Contains(l[0], "mas_3-20261104-090000-bootfail、mas_6-20261103-100000-bootfail、mas_6-20261102-100000-bootfail") {
		t.Errorf("lines %q", l)
	}

	saved := bootDiagCapBytes
	t.Cleanup(func() { bootDiagCapBytes = saved })
	bootDiagCapBytes = 15 // the newest alone (20 bytes) is over it: taken all the same, nothing after it
	c = collectLogs()
	if got := staged(c.stage); len(got) != 2 || !strings.HasPrefix(got[0], "logs/mas_3-20261104-090000-bootfail/") {
		t.Errorf("over the cap: staged %q", got)
	}
}
