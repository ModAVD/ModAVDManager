//go:build windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/report"
)

// scriptPrivacyHead is the first line of collect-info.ps1's privacy note; report.PrivacyNote says it differently.
const scriptPrivacyHead = "隐私提示：问题包只保存在本机，脚本不会上传。"

// golden reads what a package script printed for a test scenario (internal/report/testdata/golden), LF line ends.
func golden(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "report", "testdata", "golden"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// report-config prints what report-config.ps1 prints: a blank line, the lines, a blank line, the clipboard line.
func TestHumanReportConfigMatchesScript(t *testing.T) {
	for _, name := range []string{"full", "bare"} {
		want := golden(t, name, "report-config.txt")
		ls := strings.Split(strings.TrimSuffix(want, "\n"), "\n")
		res := &report.ConfigResult{Lines: ls[1 : len(ls)-2], Message: ls[len(ls)-1]}
		var b bytes.Buffer
		humanReportConfig(&b, res)
		if b.String() != want {
			t.Errorf("%s:\n got  %q\n want %q", name, b.String(), want)
		}
	}
}

// collect-info ends like collect-info.ps1: the zip, its size, the privacy note, then what to add or what failed.
func TestHumanCollectInfoMatchesScript(t *testing.T) {
	for _, c := range []struct {
		name     string
		mas      bool
		problems []string
	}{{"full", true, nil}, {"bare", false, []string{"杀毒软件：无效命名空间 "}}} {
		console := golden(t, c.name, "collect-console.txt")
		want := console[strings.Index(console, "\n收集完成。\n"):]
		// but for the first line of the privacy note (report.PrivacyNote): a script no longer collects
		head, _, _ := strings.Cut(report.PrivacyNote, "\r\n")
		if strings.Count(want, scriptPrivacyHead+"\n") != 1 {
			t.Fatalf("%s: the script's privacy note head not found once", c.name)
		}
		want = strings.Replace(want, scriptPrivacyHead+"\n", head+"\n", 1)
		res := &report.CollectResult{
			ZipPath: `{{BASE}}\out\魔改AVD问题信息-20260115-120000.zip`, ZipSize: "{{SIZE}}",
			PrivacyNote: report.PrivacyNote, MasFound: c.mas, Problems: c.problems,
		}
		var b bytes.Buffer
		humanCollectInfo(&b, res)
		if got := strings.ReplaceAll(b.String(), "\r\n", "\n"); got != want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, want)
		}
	}
}
