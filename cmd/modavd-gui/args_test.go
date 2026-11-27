package main

import (
	"runtime"
	"strings"
	"testing"

	"modavd/internal/update"
)

type reported struct {
	msgs  []string
	isErr []bool
}

func (r *reported) report(msg string, isErr bool) {
	r.msgs = append(r.msgs, msg)
	r.isErr = append(r.isErr, isErr)
}

// Every way the command line can stop the start, with its exit code and whether it is an error (stderr) or the
// usage (stdout) (ModAVD/ModAVD#258).
func TestParseArgsStops(t *testing.T) {
	chrome := "flag provided but not defined: -chrome-window"
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		chrome = "多余的参数：x" // an option there: only the extra argument is wrong
	}
	for _, c := range []struct {
		args  []string
		code  int
		isErr bool
		want  string
	}{
		{[]string{"--help"}, 0, false, "--root 根目录："},
		{[]string{"-h"}, 0, false, usage},
		{[]string{"--bogus"}, 2, true, "命令行参数不对：flag provided but not defined: -bogus"},
		{[]string{"--port"}, 2, true, "命令行参数不对：flag needs an argument: -port"},
		{[]string{"help"}, 2, true, "多余的参数：help"},
		{[]string{"--port", "70000"}, 2, true, "端口 70000 不对"},
		{[]string{"--port", "-1"}, 2, true, "端口 -1 不对"},
		{[]string{update.HandoffArg, "not-an-id"}, 2, true, "更新交接 ID 必须是 32 位小写十六进制字符"},
		{[]string{update.HandoffArg, strings.Repeat("A", 32)}, 2, true, "更新交接 ID 必须是 32 位小写十六进制字符"},
		{[]string{update.HandoffArg}, 2, true, "flag needs an argument: -update-handoff"},
		{[]string{"--chrome-window", "x"}, 2, true, chrome},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			var r reported
			_, code, ok := parseArgs(c.args, r.report)
			if ok || code != c.code {
				t.Fatalf("ok %v, exit code %d; want false, %d", ok, code, c.code)
			}
			if len(r.msgs) != 1 || r.isErr[0] != c.isErr || !strings.Contains(r.msgs[0], c.want) {
				t.Fatalf("reported %q (isErr %v); want one message with %q, isErr %v", r.msgs, r.isErr, c.want, c.isErr)
			}
		})
	}
}

func TestParseArgsStarts(t *testing.T) {
	id := strings.Repeat("0a", 16)
	args := []string{"--root", `E:\r`, "--port", "65535", "--no-window", "--dev", update.HandoffArg, id}
	wantChrome := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if wantChrome {
		args = append(args, "--chrome-window")
	}
	var r reported
	a, code, ok := parseArgs(args, r.report)
	if !ok || code != 0 || len(r.msgs) != 0 {
		t.Fatalf("ok %v, exit code %d, reported %q; want a start", ok, code, r.msgs)
	}
	want := guiArgs{root: `E:\r`, port: 65535, noWindow: true, chromeWindow: wantChrome, dev: true,
		handoffID: update.HandoffID(id)}
	if a != want {
		t.Fatalf("parsed %+v, want %+v", a, want)
	}
}

// The update hand-off ID is not an option a person uses: --help does not list it.
func TestHelpHidesHandoff(t *testing.T) {
	var r reported
	parseArgs([]string{"--help"}, r.report)
	if len(r.msgs) != 1 || strings.Contains(r.msgs[0], "update-handoff") || strings.Contains(r.msgs[0], "更新交接") {
		t.Fatalf("help %q", r.msgs)
	}
}
