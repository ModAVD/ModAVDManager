//go:build windows

package cli

import (
	"reflect"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
)

func TestParseArgs(t *testing.T) {
	type want struct {
		cmd     string
		pos     []string
		rest    []string
		g       globals
		flags   map[string]any // expected values of flags that must be set
		unset   []string       // flags that must not be set
		errCode string
	}
	tests := []struct {
		name string
		args []string
		want want
	}{
		{"plain", []string{"list"}, want{cmd: "list"}},
		{"globals before", []string{"--root", `D:\x`, "--json", "list"},
			want{cmd: "list", g: globals{root: `D:\x`, json: true}}},
		{"globals after, = form", []string{"list", "--json", `--root=D:\x`, "--verbose"},
			want{cmd: "list", g: globals{root: `D:\x`, json: true, verbose: true}}},
		{"single dash", []string{"-json", "-quiet", "list"}, want{cmd: "list", g: globals{json: true, quiet: true}}},
		{"flag after positional", []string{"start", "3", "--window"},
			want{cmd: "start", pos: []string{"3"}, flags: map[string]any{"window": true}, unset: []string{"timeout"}}},
		{"flag before positional", []string{"start", "--timeout", "120", "mas_3"},
			want{cmd: "start", pos: []string{"mas_3"}, flags: map[string]any{"timeout": 120}, unset: []string{"window"}}},
		{"int is decimal", []string{"start", "3", "--timeout=060"},
			want{cmd: "start", pos: []string{"3"}, flags: map[string]any{"timeout": 60}}},
		{"bool =false", []string{"start", "3", "--window=false"},
			want{cmd: "start", pos: []string{"3"}, flags: map[string]any{"window": false}}},
		{"global =false", []string{"list", "--json=false"}, want{cmd: "list"}},
		{"string value starting with dash", []string{"create", "--title", "-x-", "--memory", "6G", "--cpu", "4"},
			want{cmd: "create", flags: map[string]any{"title": "-x-", "memory": "6G", "cpu": 4},
				unset: []string{"index", "fps", "data-gb"}}},
		{"empty string value", []string{"set", "3", "--title="},
			want{cmd: "set", pos: []string{"3"}, flags: map[string]any{"title": ""}}},
		{"delete yes", []string{"delete", "3", "--yes"},
			want{cmd: "delete", pos: []string{"3"}, flags: map[string]any{"yes": true}}},
		{"stop all", []string{"stop", "--all", "--force"},
			want{cmd: "stop", flags: map[string]any{"all": true, "force": true}}},
		{"double dash ends options", []string{"list", "--", "--json"}, want{cmd: "list", pos: []string{"--json"}}},
		{"adb tail verbatim", []string{"adb", "3", "shell", "ls", "--json"},
			want{cmd: "adb", pos: []string{"3"}, rest: []string{"shell", "ls", "--json"}}},
		{"adb globals before tail", []string{"adb", "--json", "3", "--root", `D:\r`, "devices"},
			want{cmd: "adb", pos: []string{"3"}, rest: []string{"devices"}, g: globals{json: true, root: `D:\r`}}},
		{"adb double dash", []string{"adb", "3", "--", "--version"},
			want{cmd: "adb", pos: []string{"3"}, rest: []string{"--version"}}},
		{"adb double dash before index", []string{"adb", "--", "3", "shell", "id"},
			want{cmd: "adb", pos: []string{"3"}, rest: []string{"shell", "id"}}},
		{"adb unknown flag starts tail", []string{"adb", "3", "-d", "shell"},
			want{cmd: "adb", pos: []string{"3"}, rest: []string{"-d", "shell"}}},
		{"shell timeout then command", []string{"shell", "3", "--timeout", "10", "ls", "-l", "/data"},
			want{cmd: "shell", pos: []string{"3"}, rest: []string{"ls", "-l", "/data"}, flags: map[string]any{"timeout": 10}}},
		{"shell command looks like flag", []string{"shell", "3", "--", "--timeout"},
			want{cmd: "shell", pos: []string{"3"}, rest: []string{"--timeout"}, unset: []string{"timeout"}}},
		{"no command", []string{"--json"}, want{g: globals{json: true}}},
		{"help flag", []string{"-h"}, want{g: globals{help: true}}},
		{"command help flag", []string{"start", "--help"}, want{cmd: "start", g: globals{help: true}}},
		{"help command", []string{"help", "start"}, want{cmd: "help", pos: []string{"start"}}},

		{"unknown command", []string{"bogus"}, want{errCode: errs.Usage}},
		{"unknown command keeps json", []string{"bogus", "--json"}, want{errCode: errs.Usage, g: globals{json: true}}},
		{"command flag before command", []string{"--window", "start", "3"}, want{errCode: errs.Usage}},
		{"unknown command flag", []string{"start", "3", "--bogus"}, want{errCode: errs.Usage}},
		{"flag of another command", []string{"list", "--force"}, want{errCode: errs.Usage}},
		{"int not a number", []string{"start", "3", "--timeout", "abc"}, want{errCode: errs.Usage}},
		{"missing value", []string{"start", "3", "--timeout"}, want{errCode: errs.Usage}},
		{"bad bool", []string{"start", "3", "--window=maybe"}, want{errCode: errs.Usage}},
		{"root without value", []string{"--root"}, want{errCode: errs.Usage}},
		{"root empty", []string{"--root=", "list"}, want{errCode: errs.Usage}},
		{"root only a quote", []string{"list", "--root", `"`}, want{errCode: errs.Usage}},
		{"bad global bool", []string{"list", "--json=nope"}, want{errCode: errs.Usage}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := parseArgs(tc.args)
			if inv == nil {
				t.Fatal("nil invocation")
			}
			if tc.want.errCode != "" {
				if errs.CodeOf(err) != tc.want.errCode {
					t.Fatalf("error code = %q (%v), want %q", errs.CodeOf(err), err, tc.want.errCode)
				}
				if inv.json != tc.want.g.json {
					t.Errorf("json = %v, want %v", inv.json, tc.want.g.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotCmd := ""
			if inv.cmd != nil {
				gotCmd = inv.cmd.name
			}
			if gotCmd != tc.want.cmd {
				t.Fatalf("cmd = %q, want %q", gotCmd, tc.want.cmd)
			}
			if inv.globals != tc.want.g {
				t.Errorf("globals = %+v, want %+v", inv.globals, tc.want.g)
			}
			if !equalStrings(inv.pos, tc.want.pos) {
				t.Errorf("pos = %q, want %q", inv.pos, tc.want.pos)
			}
			if !equalStrings(inv.rest, tc.want.rest) {
				t.Errorf("rest = %q, want %q", inv.rest, tc.want.rest)
			}
			for name, v := range tc.want.flags {
				if !inv.isSet(name) {
					t.Errorf("flag %s not set", name)
				}
				if got := inv.get(name); !reflect.DeepEqual(got, v) {
					t.Errorf("flag %s = %#v, want %#v", name, got, v)
				}
			}
			for _, name := range tc.want.unset {
				if inv.isSet(name) {
					t.Errorf("flag %s set, want unset", name)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestCheckArgs(t *testing.T) {
	tests := []struct {
		args    []string
		errCode string
		msg     string // substring of the message
	}{
		{[]string{"info"}, errs.Usage, "缺少实例编号"},
		{[]string{"info", "3", "4"}, errs.Usage, "多余的参数：4"},
		{[]string{"window", "3"}, errs.Usage, "缺少 show 或 hide"},
		{[]string{"install", "3"}, errs.Usage, "缺少 APK 路径"},
		{[]string{"shell", "3"}, errs.Usage, "缺少要执行的命令"},
		{[]string{"adb", "3"}, errs.Usage, "缺少 adb 参数"},
		{[]string{"adb"}, errs.Usage, "缺少实例编号"},
		{[]string{"list", "x"}, errs.Usage, "多余的参数：x"},
		{[]string{"help", "a", "b"}, errs.Usage, "多余的参数：b"},
		{[]string{"stop"}, "", ""}, // checked by the command itself (--all)
		{[]string{"info", "3"}, "", ""},
		{[]string{"window", "3", "show"}, "", ""},
		{[]string{"shell", "3", "id"}, "", ""},
	}
	for _, tc := range tests {
		inv, err := parseArgs(tc.args)
		if err != nil {
			t.Fatalf("%q: parse: %v", tc.args, err)
		}
		err = inv.checkArgs()
		if errs.CodeOf(err) != tc.errCode {
			t.Errorf("%q: code = %q (%v), want %q", tc.args, errs.CodeOf(err), err, tc.errCode)
			continue
		}
		if err != nil {
			e := errs.As(err)
			if !strings.Contains(e.Message, tc.msg) {
				t.Errorf("%q: message %q does not contain %q", tc.args, e.Message, tc.msg)
			}
			if !strings.HasPrefix(e.Advice, "用法：modavd "+inv.cmd.name) {
				t.Errorf("%q: advice %q is not the usage line", tc.args, e.Advice)
			}
		}
	}
}

func TestParseIndex(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		errCode string
	}{
		{"3", 3, ""},
		{"0", 0, ""},
		{"9", 9, ""},
		{"mas_3", 3, ""},
		{"MAS_4", 4, ""},
		{" 6 ", 6, ""},
		{"03", 3, ""},
		{"5", 0, errs.InvalidArgument},
		{"mas_5", 0, errs.InvalidArgument},
		{"10", 0, errs.InvalidArgument},
		{"-1", 0, errs.Usage},
		{"", 0, errs.Usage},
		{"mas_", 0, errs.Usage},
		{"abc", 0, errs.Usage},
		{"3a", 0, errs.Usage},
		{"mas3", 0, errs.Usage},
		{"emulator-20030", 0, errs.Usage},
		{"99999999999999999999", 0, errs.Usage},
	}
	for _, tc := range tests {
		got, err := parseIndex(tc.in)
		if errs.CodeOf(err) != tc.errCode {
			t.Errorf("parseIndex(%q) error = %v, want code %q", tc.in, err, tc.errCode)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseIndex(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// fakeParseMemory stands in for avd.ParseMemoryMB: "6G" / "6144" / "6144M".
func fakeParseMemory(s string) (int, error) {
	switch strings.ToUpper(s) {
	case "6G", "6144", "6144M":
		return 6144, nil
	case "4G", "4096":
		return 4096, nil
	}
	return 0, errs.Newf(errs.InvalidArgument, "内存写法不对：%s", s)
}

func withFakeMemory(t *testing.T) {
	t.Helper()
	old := parseMemory
	parseMemory = fakeParseMemory
	t.Cleanup(func() { parseMemory = old })
}

func mustParse(t *testing.T, args ...string) *invocation {
	t.Helper()
	inv, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return inv
}

func TestBuildCreateRequest(t *testing.T) {
	withFakeMemory(t)
	req, err := buildCreateRequest(mustParse(t, "create"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Index != nil || req.MemoryMB != nil || req.CPU != nil || req.DataPartitionGB != nil || req.FPS != nil || req.Title != "" {
		t.Errorf("defaults: %+v, want all nil", req)
	}
	req, err = buildCreateRequest(mustParse(t, "create", "--index", "mas_4", "--title", "星铁", "--memory", "6G",
		"--cpu", "4", "--data-gb", "128", "--fps", "30"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Index == nil || *req.Index != 4 || req.Title != "星铁" || req.MemoryMB == nil || *req.MemoryMB != 6144 ||
		req.CPU == nil || *req.CPU != 4 || req.DataPartitionGB == nil || *req.DataPartitionGB != 128 ||
		req.FPS == nil || *req.FPS != 30 {
		t.Errorf("all options: %+v", req)
	}
	// Zero is passed on (emu rejects it with the AUTO-MAS wording), not mistaken for "not given".
	req, _ = buildCreateRequest(mustParse(t, "create", "--cpu", "0"))
	if req.CPU == nil || *req.CPU != 0 {
		t.Errorf("--cpu 0 lost: %+v", req.CPU)
	}
	if _, err := buildCreateRequest(mustParse(t, "create", "--index", "5")); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("--index 5: %v", err)
	}
	if _, err := buildCreateRequest(mustParse(t, "create", "--memory", "lots")); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("--memory lots: %v", err)
	}
}

func TestMemoryOptWrapsForeignErrors(t *testing.T) {
	old := parseMemory
	defer func() { parseMemory = old }()
	parseMemory = func(string) (int, error) { return 0, errStr("strconv: bad") }
	_, err := memoryOpt("x")
	if errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("code = %q, want invalid_argument", errs.CodeOf(err))
	}
}

// The real parser behind --memory (skipped while the avd package is still a placeholder).
func TestMemoryOptRealParser(t *testing.T) {
	if _, err := avd.ParseMemoryMB("6G"); errs.CodeOf(err) == errs.NotImplemented {
		t.Skip("avd.ParseMemoryMB not implemented yet")
	}
	for in, want := range map[string]int{"6G": 6144, "6144": 6144, "6144M": 6144, "4gb": 4096, " 3072MB ": 3072} {
		got, err := memoryOpt(in)
		if err != nil || got != want {
			t.Errorf("memoryOpt(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := memoryOpt("lots"); errs.ExitCode(err) != 2 {
		t.Errorf("memoryOpt(lots): %v, want a usage / invalid argument error", err)
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

func TestBuildUpdateRequest(t *testing.T) {
	withFakeMemory(t)
	if _, err := buildUpdateRequest(mustParse(t, "set", "3")); errs.CodeOf(err) != errs.Usage {
		t.Errorf("no option: %v, want usage", err)
	}
	req, err := buildUpdateRequest(mustParse(t, "set", "3", "--title", ""))
	if err != nil || req.Title == nil || *req.Title != "" {
		t.Errorf("empty title: %+v %v", req, err)
	}
	req, err = buildUpdateRequest(mustParse(t, "set", "3", "--memory=4G", "--fps", "60"))
	if err != nil || req.MemoryMB == nil || *req.MemoryMB != 4096 || req.FPS == nil || *req.FPS != 60 || req.CPU != nil ||
		req.Title != nil || req.DataPartitionGB != nil {
		t.Errorf("memory+fps: %+v %v", req, err)
	}
	req, err = buildUpdateRequest(mustParse(t, "set", "3", "--data-gb", "96"))
	if err != nil || req.DataPartitionGB == nil || *req.DataPartitionGB != 96 || req.MemoryMB != nil {
		t.Errorf("data-gb alone: %+v %v", req, err)
	}
}

func TestBuildStartOptions(t *testing.T) {
	opt, err := buildStartOptions(mustParse(t, "start", "3"))
	if err != nil || opt.Window || opt.Timeout != 0 {
		t.Errorf("defaults: %+v %v", opt, err)
	}
	opt, err = buildStartOptions(mustParse(t, "start", "3", "--window", "--timeout", "120"))
	if err != nil || !opt.Window || opt.Timeout.Seconds() != 120 {
		t.Errorf("window+timeout: %+v %v", opt, err)
	}
	for _, v := range []string{"0", "-5"} {
		if _, err := buildStartOptions(mustParse(t, "start", "3", "--timeout", v)); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("--timeout %s: %v", v, err)
		}
	}
}

func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands {
		if seen[c.name] {
			t.Errorf("duplicate command %s", c.name)
		}
		seen[c.name] = true
		if c.run == nil || c.summary == "" {
			t.Errorf("%s: missing run or summary", c.name)
		}
		if !c.raw && c.human == nil {
			t.Errorf("%s: no human formatter", c.name)
		}
		for _, f := range c.flags {
			if isGlobalName(f.name) {
				t.Errorf("%s: flag --%s shadows a global flag", c.name, f.name)
			}
		}
	}
	for _, name := range []string{"list", "info", "status", "create", "set", "delete", "start", "stop", "restart",
		"check", "window", "install", "shell", "adb", "servers", "version", "mcp", "help"} {
		if !seen[name] {
			t.Errorf("command %s missing", name)
		}
	}
}
