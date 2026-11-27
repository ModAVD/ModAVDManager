//go:build windows

package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/root"
)

func TestDiagnosticCollectZipAndLive(t *testing.T) {
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	r, err := root.New(f.root)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	if err := diagnostics.Set(r, 1, true, since); err != nil {
		t.Fatal(err)
	}
	serial := f.adb.data.Serials["emulator-20010"]
	if serial.Shell == nil {
		serial.Shell = map[string]scenarioShell{}
	}
	for _, cmd := range []string{`export ANDROID_LOG_TAGS=''; exec logcat '-d' '-b' 'all'`, `su 0 dmesg || dmesg`, `cat /proc/meminfo`, `dumpsys activity processes | head -n 300`} {
		serial.Shell[cmd] = scenarioShell{Out: "evidence-" + cmd + "\n"}
	}
	f.adb.data.Serials["emulator-20010"] = serial
	putDiagnosticFile(t, filepath.Join(f.root, "logs", "mas_1-crashdumps", "extra.dmp"), "diagnostic-dump")
	dump := filepath.Join(f.root, "logs", "mas_1-crashdumps", "extra.dmp")
	if err := os.Chtimes(dump, f.now, f.now); err != nil {
		t.Fatal(err)
	}
	res, _, files := runCollect(t, f)
	if !strings.Contains(res.Summary, "mas_1：诊断模式 开，开启时间："+since.Format(time.RFC3339Nano)) || !strings.Contains(res.Summary, "mas_3：诊断模式 关") {
		t.Fatal(res.Summary)
	}
	if string(files["logs/mas_1-crashdumps/extra.dmp"]) != "diagnostic-dump" {
		t.Fatal("missing diagnostic dump in zip")
	}
	for _, tag := range []string{"logcat-all", "dmesg", "meminfo", "activity-processes"} {
		if !strings.Contains(string(files["live/mas_1-"+tag+".txt"]), "evidence-") {
			t.Fatalf("missing live output for %s", tag)
		}
		if _, ok := files["live/mas_3-"+tag+".txt"]; ok {
			t.Fatalf("ordinary instance got %s", tag)
		}
	}
	_, requests := f.adb.seen()
	for _, req := range requests {
		if strings.HasPrefix(req, "emulator-20030 ") && (strings.Contains(req, "/proc/meminfo") || strings.Contains(req, "su 0 dmesg")) {
			t.Fatalf("ordinary guest queried: %s", req)
		}
	}
}

func TestDiagnosticCollectBadMetadataReported(t *testing.T) {
	for _, metadata := range []string{`{`, `{"instances":{"1":{"diagnostics":"yes"}}}`} {
		t.Run(metadata, func(t *testing.T) {
			f := newFixture(t, "full", testTempDir(t), "")
			guardDials(t, f.adbPort)
			if err := os.WriteFile(filepath.Join(f.root, root.MetadataFile), []byte(metadata), 0644); err != nil {
				t.Fatal(err)
			}
			res, _, _ := runCollect(t, f)
			if !strings.Contains(strings.Join(res.Problems, "\n"), "诊断模式状态未知") || !strings.Contains(res.Summary, "诊断模式状态未知") {
				t.Fatalf("bad metadata treated as known: %v", res.Problems)
			}
			if strings.Contains(res.Summary, "诊断模式 关") {
				t.Fatal("reported unknown as off")
			}
		})
	}
}
