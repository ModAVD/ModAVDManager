//go:build windows

package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"modavd/internal/appproxy"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
)

// The fake adb client (portplan_test.go) plays a guest for the per-app proxy when MODAVD_EMU_FAKE_ADB_PROXY names a
// directory, which holds that guest's state as files:
//
//	sum      the SHA-256 of the guest's modavd-proxyd ("" or missing: not there)
//	pending  the SHA-256 of the modavd-proxyd.new pushed and not yet put in place
//	config   the config.json last put in place
//	tamper   when present, putting the pushed daemon in place leaves another file there instead
//	rules    the rules a start reports (default ok)
//	root     one line per root command: "<cmd>" when it ran, "refused <cmd>" when its hash check stopped it,
//	         "unchecked <cmd>" when it came without one, "replace" for the swap
//	pushes   one line per push: the remote path
const fakeProxyEnv = "MODAVD_EMU_FAKE_ADB_PROXY"

var (
	fakeCheckRE = regexp.MustCompile(`\[ "\$1" = "([0-9a-f]{64})" \] \|\| exit 126; exec (\S+) (\w+)$`)
	fakeRunRE   = regexp.MustCompile(`(\S*modavd-proxyd) (start|stop|status)$`)
)

// fakeProxyGuest answers the per-app proxy's adb calls; handled is false for the others.
func fakeProxyGuest(args []string) (handled bool, code int) {
	dir := os.Getenv(fakeProxyEnv)
	if dir == "" {
		return false, 0
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	write := func(name, s string) { os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644) }
	appendLine := func(name, s string) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err == nil {
			f.WriteString(s + "\n")
			f.Close()
		}
	}
	k := slices.IndexFunc(args, func(a string) bool { return a == "shell" || a == "push" })
	if k < 0 || k+1 >= len(args) {
		return false, 0
	}
	if args[k] == "push" && k+2 < len(args) {
		local, remote := args[k+1], args[k+2]
		b, err := os.ReadFile(local)
		if err != nil {
			return true, 1
		}
		appendLine("pushes", remote)
		switch {
		case strings.HasSuffix(remote, "/"+appproxy.DaemonName+".new"):
			s := sha256.Sum256(b)
			write("pending", hex.EncodeToString(s[:]))
		case strings.HasSuffix(remote, "/"+appproxy.DaemonName):
			s := sha256.Sum256(b)
			write("sum", hex.EncodeToString(s[:]))
		case strings.HasSuffix(remote, "/"+appproxy.ConfigName+".new"):
			write("config.new", string(b))
		case strings.HasSuffix(remote, "/"+appproxy.ConfigName):
			write("config", string(b))
		}
		return true, 0
	}
	cmd := strings.Join(args[k+1:], " ")
	switch {
	case strings.HasPrefix(cmd, "mkdir -p "+appproxy.GuestDir):
		return true, 0
	case strings.HasPrefix(cmd, "sha256sum "+proxyExe):
		if s := read("sum"); s != "" {
			fmt.Printf("%s  %s\n", s, proxyExe)
			return true, 0
		}
		fmt.Printf("sha256sum: %s: No such file or directory\n", proxyExe)
		return true, 1
	case strings.HasPrefix(cmd, "test -x "+proxyExe):
		if read("sum") != "" {
			return true, 0
		}
		return true, 1
	case strings.HasPrefix(cmd, "mv -f "+appproxy.GuestDir+"/"+appproxy.ConfigName+".new"):
		write("config", read("config.new"))
		return true, 0
	case !strings.HasPrefix(cmd, "su 0 sh -c '") || !strings.HasSuffix(cmd, "'"):
		return false, 0
	}
	script := strings.TrimSuffix(strings.TrimPrefix(cmd, "su 0 sh -c '"), "'")
	if strings.Contains(script, "mv -f "+proxyExe+".new "+proxyExe) {
		appendLine("root", "replace")
		if _, err := os.Stat(filepath.Join(dir, "tamper")); err == nil {
			write("sum", strings.Repeat("ab", 32))
		} else {
			write("sum", read("pending"))
		}
		return true, 0
	}
	var verb string
	if m := fakeCheckRE.FindStringSubmatch(script); m != nil {
		verb = m[3]
		if read("sum") == "" {
			return true, 127
		}
		if m[1] != read("sum") {
			appendLine("root", "refused "+verb)
			return true, 126
		}
		appendLine("root", verb)
	} else if m := fakeRunRE.FindStringSubmatch(script); m != nil {
		verb = m[2]
		appendLine("root", "unchecked "+verb)
	} else {
		return false, 0
	}
	st := appproxy.Status{Version: "test", Running: verb != "stop", PID: 4242, Rules: appproxy.RulesOK,
		Apps: []appproxy.AppStatus{}}
	switch {
	case verb == "stop":
		st.Rules = appproxy.RulesOff
	case read("rules") != "":
		st.Rules = read("rules")
	}
	b, _ := json.Marshal(st)
	fmt.Println(string(b))
	if st.Rules != appproxy.RulesOK && verb == "start" {
		return true, 1
	}
	return true, 0
}

// proxyGuest is a fake guest for instance 6 of a port plan root (portPlanRoot), with daemon as the daemon this
// modavd carries.
type proxyGuest struct {
	dir    string
	daemon carried
}

func newProxyGuest(t *testing.T, content string) *proxyGuest {
	t.Helper()
	g := &proxyGuest{dir: t.TempDir()}
	local := filepath.Join(t.TempDir(), appproxy.DaemonName)
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256([]byte(content))
	g.daemon = carried{local: local, sum: hex.EncodeToString(s[:])}
	saved := daemonFile
	daemonFile = func() (carried, error) { return g.daemon, nil }
	t.Cleanup(func() { daemonFile = saved })
	t.Setenv(fakeProxyEnv, g.dir)
	return g
}

func (g *proxyGuest) get(name string) string {
	b, _ := os.ReadFile(filepath.Join(g.dir, name))
	return strings.TrimSpace(string(b))
}

func (g *proxyGuest) set(name, s string) {
	os.WriteFile(filepath.Join(g.dir, name), []byte(s), 0o644)
}

func (g *proxyGuest) lines(name string) []string {
	if s := g.get(name); s != "" {
		return strings.Split(s, "\n")
	}
	return nil
}

// reset forgets what was done so far (the guest's files stay).
func (g *proxyGuest) reset() {
	os.Remove(filepath.Join(g.dir, "root"))
	os.Remove(filepath.Join(g.dir, "pushes"))
}

func setProxyMeta(t *testing.T, m *Manager, i int, s appproxy.Setting) {
	t.Helper()
	if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(appproxy.MetaKey, settingValue(meta.Get(appproxy.MetaKey), s))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPutAppProxyGuest goes through putting the per-app proxy into a (fake) guest: the daemon only by way of .new,
// never run as root unless the hash check in the same command passes, its config by way of .new, rules that are not in
// place reported as unsupported, an old version replaced before it is stopped, a boot with the setting off stopping a
// daemon that is there.
func TestPutAppProxyGuest(t *testing.T) {
	r, _ := portPlanRoot(t)
	ctx := context.Background()
	col := &logx.Collector{}
	m := New(r, logx.Nop()).withLog(col.Sink())
	six := 6
	if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	g := newProxyGuest(t, "the daemon of this modavd")
	on := appproxy.Setting{Enabled: true, Server: "socks5://10.0.2.2:7890", Apps: []string{"com.example.game"}}

	// a fresh guest: pushed as .new, put in place, the config by way of .new, then a checked start
	setProxyMeta(t, m, six, on)
	s, st, err := m.applyAppProxy(ctx, six)
	if err != nil || !s.Active() || st == nil || st.Rules != appproxy.RulesOK {
		t.Fatalf("fresh: %+v %+v %v", s, st, err)
	}
	if got, want := g.lines("pushes"), []string{proxyExe + ".new", appproxy.GuestDir + "/" + appproxy.ConfigName + ".new"}; !slices.Equal(got, want) {
		t.Errorf("pushes %q, want %q", got, want)
	}
	if got := g.lines("root"); !slices.Equal(got, []string{"replace", "start"}) {
		t.Errorf("root commands %q", got)
	}
	var cfg appproxy.Config
	if err := json.Unmarshal([]byte(g.get("config")), &cfg); err != nil || !slices.Equal(cfg.Apps, on.Apps) || cfg.Server != on.Server {
		t.Errorf("config %q %v", g.get("config"), err)
	}

	// the same daemon there: not pushed again
	g.reset()
	if _, _, err := m.applyAppProxy(ctx, six); err != nil {
		t.Fatal(err)
	}
	if got := g.lines("pushes"); len(got) != 1 || !strings.HasSuffix(got[0], appproxy.ConfigName+".new") {
		t.Errorf("pushes %q, want only the config", got)
	}

	// another file there, and the swap leaves another one again (it was changed under modavd): not run as root
	g.reset()
	g.set("sum", strings.Repeat("cd", 32))
	g.set("tamper", "1")
	_, _, err = m.applyAppProxy(ctx, six)
	if !errors.Is(err, errOtherDaemon) || errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("tampered: %v", err)
	}
	if got := g.lines("root"); !slices.Equal(got, []string{"replace", "refused start"}) {
		t.Errorf("tampered: root commands %q", got)
	}
	os.Remove(filepath.Join(g.dir, "tamper"))

	// rules that did not go in place: unsupported, not success
	g.reset()
	g.set("sum", g.daemon.sum)
	g.set("rules", appproxy.RulesFailed)
	if _, _, err := m.applyAppProxy(ctx, six); errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "规则") {
		t.Errorf("rules failed: %v", err)
	}
	os.Remove(filepath.Join(g.dir, "rules"))

	// the boot's step with the setting off: an old daemon there is replaced by this one, then stopped
	g.reset()
	g.set("sum", strings.Repeat("ef", 32))
	setProxyMeta(t, m, six, appproxy.Setting{Server: on.Server, Apps: on.Apps})
	m.setupAppProxy(ctx, six)
	if got := g.lines("root"); !slices.Equal(got, []string{"replace", "stop"}) {
		t.Errorf("off at boot: root commands %q", got)
	}
	// no daemon there: nothing to stop, nothing run
	g.reset()
	g.set("sum", "")
	m.setupAppProxy(ctx, six)
	if got := g.lines("root"); len(got) != 0 {
		t.Errorf("off at boot, no daemon: root commands %q", got)
	}

	// a failed apply at boot: a warning (start's warnings)
	g.reset()
	g.set("tamper", "1")
	setProxyMeta(t, m, six, on)
	m.setupAppProxy(ctx, six)
	if msgs := col.Messages(); !slices.ContainsFunc(msgs, func(s string) bool { return strings.Contains(s, "这次开机直连") }) {
		t.Errorf("no warning: %q", msgs)
	}
	os.Remove(filepath.Join(g.dir, "tamper"))

	// a query: another daemon there is not run, and the notes say why
	g.reset()
	g.set("sum", strings.Repeat("12", 32))
	var notes []string
	if st := m.guestStatus(ctx, six, &notes); st != nil || len(notes) != 1 || !strings.Contains(notes[0], "不一样") {
		t.Errorf("query of another daemon: %+v %q", st, notes)
	}
	if got := g.lines("root"); !slices.Equal(got, []string{"refused status"}) {
		t.Errorf("query: root commands %q", got)
	}
}

// TestSetupAppProxyUnset: a boot of an instance that never had the setting does not touch the guest at all.
func TestSetupAppProxyUnset(t *testing.T) {
	r, log := portPlanRoot(t)
	ctx := context.Background()
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	newProxyGuest(t, "the daemon")
	before, _ := os.ReadFile(log)
	m.setupAppProxy(ctx, six)
	after, _ := os.ReadFile(log)
	if len(after) != len(before) {
		t.Errorf("adb called: %s", after[len(before):])
	}
}
