//go:build windows

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/update"
	"modavd/internal/win"
)

func dpapiArg(t *testing.T, cdk string) string {
	t.Helper()
	blob, err := win.Protect([]byte(cdk), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(blob)
}

// updateHarness: install goes to a recording fake (nothing reads the user's update settings or the network).
func updateHarness(t *testing.T) (*harness, *[]update.Options, *int) {
	t.Helper()
	h := newHarness()
	resolved := 0
	h.resolve = func(string) (*root.Root, error) { resolved++; return &root.Root{Dir: t.TempDir()}, nil }
	var opts []update.Options
	h.updateInstall = func(ctx context.Context, u *update.Updater) (*update.Status, error) {
		opts = append(opts, u.Opt)
		return &update.Status{Updated: true, Message: "已更新到 0.40.0", Notices: []string{}}, nil
	}
	return h, &opts, &resolved
}

func TestUpdateHelpListsCallerFlags(t *testing.T) {
	res := newHarness().run(t, nil, "help", "update", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	var flags []string
	for _, o := range envelope(t, res.stdout)["data"].(map[string]any)["commands"].([]any)[0].(map[string]any)["options"].([]any) {
		flags = append(flags, strings.Fields(o.(map[string]any)["flag"].(string))[0])
	}
	got := strings.Join(flags, " ")
	if !strings.Contains(got, "--source") || !strings.Contains(got, "--cdk-dpapi") {
		t.Fatalf("options %s", got)
	}
}

func TestUpdateCallerFlagErrors(t *testing.T) {
	secret := "QUJDREVGR0hJSktMTU5PUA=="
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"update", "recover", "--source", "github"}, errs.Usage},
		{[]string{"update", "recover", "--cdk-dpapi", secret}, errs.Usage},
		{[]string{"update", "check", "--source", "ftp"}, errs.InvalidArgument},
		{[]string{"update", "check", "--source"}, errs.Usage},
		{[]string{"update", "check", "--cdk-dpapi"}, errs.Usage},
		{[]string{"update", "check", "--cdk-dpapi", "%%%" + secret}, errs.InvalidArgument},
		{[]string{"update", "prepare", "--cdk-dpapi", secret}, errs.InvalidArgument}, // base64, but not DPAPI
		{[]string{"update", "install", "--cdk-dpapi=" + dpapiArg(t, "bad&cdk")}, errs.InvalidArgument},
		{[]string{"list", "--cdk-dpapi=" + secret}, errs.Usage},
		// malformed lines that used to leave the value as an extra positional argument, quoted back
		{[]string{"update", "install", "--source", "--cdk-dpapi", secret}, errs.Usage},
		{[]string{"update", "status", "--automas-version", "--cdk-dpapi", secret}, errs.Usage},
		{[]string{"update", "install", "--source", "--cdk-dpapi=" + secret}, errs.Usage},
		{[]string{"update", "install", "--after-pid", "--cdk-dpapi=" + secret}, errs.Usage},
		{[]string{"update", "install", "--cdk-dpapi=", secret}, errs.Usage},
		{[]string{"update", "status", "--cdk-dpapi=" + secret, "xx"}, errs.Usage},
		{[]string{"update", "--cdk-dpapi=", secret}, errs.Usage},
		{[]string{"--cdk-dpapi=" + secret, "update"}, errs.Usage},
	} {
		h, opts, resolved := updateHarness(t)
		res := h.run(t, nil, append([]string{"--json"}, c.args...)...)
		if strings.Count(res.stdout, "\n") != 1 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", c.args, res.code, res.stdout, res.stderr)
			continue
		}
		env := envelope(t, res.stdout)
		e, _ := env["error"].(map[string]any)
		if e == nil || e["code"] != c.code || res.code != 2 || *resolved != 0 || len(*opts) != 0 {
			t.Errorf("%v: exit %d, %s, resolved %d", c.args, res.code, res.stdout, *resolved)
		}
		for _, a := range c.args {
			if v, ok := strings.CutPrefix(a, "--cdk-dpapi="); ok {
				a = v
			}
			if len(a) > 12 && !strings.HasPrefix(a, "--") && (strings.Contains(res.stdout, a) || strings.Contains(res.stderr, a)) {
				t.Errorf("%v: the value is echoed: %s %s", c.args, res.stdout, res.stderr)
			}
		}
		if strings.Contains(res.stdout+res.stderr, secret) || strings.Contains(res.stdout+res.stderr, "bad&cdk") {
			t.Errorf("%v: the CDK is echoed", c.args)
		}
	}
}

// The flags reach the updater, beat nothing they should not, and neither the blob nor the CDK reaches any output.
func TestUpdateCallerSettingsPassed(t *testing.T) {
	cdk := "caller-cdk-secret-31"
	arg := dpapiArg(t, cdk)
	h, opts, _ := updateHarness(t)
	res := h.run(t, nil, "update", "install", "--source", "MirrorChyan", "--cdk-dpapi", arg, "--automas-version", "5.9.0", "--json")
	if res.code != 0 || len(*opts) != 1 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	o := (*opts)[0]
	if o.Source != update.SourceMirrorChyan || o.CDK == nil || *o.CDK != cdk || o.AutoMASVersion != "5.9.0" {
		t.Fatalf("options %+v", o)
	}
	for _, s := range []string{cdk, arg} {
		if strings.Contains(res.stdout+res.stderr, s) {
			t.Fatalf("leaked: %s %s", res.stdout, res.stderr)
		}
	}

	// "" (or only blanks) is no CDK this run; no flag is no override
	for _, empty := range []string{"", "   ", "\t\r\n"} {
		h, opts, _ = updateHarness(t)
		if res := h.run(t, nil, "update", "install", "--cdk-dpapi", empty, "--json"); res.code != 0 {
			t.Fatalf("%q: exit %d: %s", empty, res.code, res.stdout)
		}
		if o := (*opts)[0]; o.CDK == nil || *o.CDK != "" || o.Source != "" {
			t.Fatalf(`--cdk-dpapi %q: %+v`, empty, o)
		}
	}
	// a repeatable flag still forwards a global or own option name verbatim (the manager's --reopen-arg --root)
	h, _, _ = updateHarness(t)
	var reopened []string
	h.reopen = func(exe string, args []string) error { reopened = args; return nil }
	if res := h.run(t, nil, "update", "install", "--reopen", `C:\M\ModAVD.exe`, "--reopen-arg", "--root",
		"--reopen-arg", `C:\M`, "--reopen-arg", "--source", "--json"); res.code != 0 ||
		strings.Join(reopened, " ") != `--root C:\M --source` {
		t.Fatalf("--reopen-arg --root: exit %d: %s %v", res.code, res.stdout, reopened)
	}
	h, opts, _ = updateHarness(t)
	if res := h.run(t, nil, "update", "install", "--source", "github", "--json"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
	if o := (*opts)[0]; o.CDK != nil || o.Source != update.SourceGitHub {
		t.Fatalf("no --cdk-dpapi: %+v", o)
	}
}

// With --json every download event that passes the 2 s throttle is a log line with the progress object; without
// --json the text lines stay as they were.
func TestUpdateProgressLines(t *testing.T) {
	h, _, _ := updateHarness(t)
	h.updateInstall = func(ctx context.Context, u *update.Updater) (*update.Status, error) {
		for _, done := range []int64{0, 100, 200, 1000} { // 100, 200: within 2 s of the first
			u.Opt.Progress(update.Progress{Stage: "downloading", Message: "下载 ModAVD 0.40.0", Percent: float64(done) / 10, Done: done, Total: 1000})
		}
		u.Opt.Progress(update.Progress{Stage: "extracting", Message: "正在解包", Percent: -1})
		return &update.Status{Notices: []string{}}, nil
	}
	res := h.run(t, nil, "update", "install", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	envelope(t, res.stdout)
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(res.stderr), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
		if p, ok := m["progress"].(map[string]any); ok {
			if m["level"] != "info" || !strings.HasPrefix(m["msg"].(string), "下载 ModAVD 0.40.0：") {
				t.Errorf("line %q", line)
			}
			got = append(got, p)
		}
	}
	if len(got) != 2 || got[0]["stage"] != "downloading" || got[0]["percent"] != 0.0 || got[0]["done_bytes"] != 0.0 ||
		got[1]["percent"] != 100.0 || got[1]["done_bytes"] != 1000.0 || got[1]["total_bytes"] != 1000.0 || len(got[1]) != 4 {
		t.Fatalf("progress %v\n%s", got, res.stderr)
	}

	res = h.run(t, nil, "update", "install")
	if res.code != 0 || strings.Contains(res.stderr, "progress") || strings.Contains(res.stderr, "{") ||
		!strings.Contains(res.stderr, "下载 ModAVD 0.40.0：100.0%") {
		t.Fatalf("text: %d %s", res.code, res.stderr)
	}
}
