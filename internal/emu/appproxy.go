package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/errs"
	"modavd/internal/guest"
	"modavd/internal/hostproxy"
	"modavd/internal/jsonord"
)

// Per-app proxy (ModAVD/ModAVD#81): the listed apps of an instance go through a proxy, the rest direct. The setting is
// mas-avd.json instances.<i>.appProxy (appproxy.Setting); the guest daemon modavd-proxyd (internal/appproxy, embedded
// by build.ps1) does the routing. With the proxy down the listed apps have no network: they never fall back to direct.
//
// The daemon goes in at every boot through modavd (the first step after boot, see afterBoot) and at once when the
// setting changes while the instance runs. A guest that reboots on its own gets it from the boot watch; a boot that
// modavd does not see (AUTO-MAS booting by itself) has none.

// AppProxyResult is `modavd proxy` and the MCP tools' answer.
type AppProxyResult struct {
	Index   int      `json:"index"`
	Enabled bool     `json:"enabled"`
	Server  string   `json:"server"` // as the guest reaches it ("socks5://10.0.2.2:7890"); "" when never set
	Apps    []string `json:"apps"`   // never null
	DNS     string   `json:"dns"`    // "" = appproxy.DefaultDNS
	// Applied: "now" (the running guest has it), "booting" (the boot under way puts it in once Android is up),
	// "next_boot" (not running: the next boot through modavd puts it in), "off" (off, and nothing of it runs in the
	// guest), "failed" (running, but the guest could not take it: Error says why). Empty for a plain query.
	Applied string `json:"applied"`
	// Notes: what was found or changed on the way, for people ("代理地址记成了 socks5://10.0.2.2:7890"). Never null.
	Notes []string `json:"notes"`
	// Guest is the daemon's state while the instance runs and the guest answers; null otherwise.
	Guest *appproxy.Status `json:"guest"`
	Error string           `json:"error"`
}

// AppProxySummary is the per-app proxy in InstanceInfo: from mas-avd.json only, no adb.
type AppProxySummary struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Apps    int    `json:"apps"`
}

// AppProxyTestResult is `modavd proxy <i> test`: what speaks at the address, probed from the host.
type AppProxyTestResult struct {
	Index    int    `json:"index"`
	Server   string `json:"server"`    // as the guest reaches it
	HostAddr string `json:"host_addr"` // where the host probed it (10.0.2.2 is the host's 127.0.0.1)
	appproxy.ProbeResult
	Guest *appproxy.Status `json:"guest"` // as for AppProxyResult
	Notes []string         `json:"notes"` // never null; as for AppProxyResult
}

const (
	proxyLockWait   = 60 * time.Second
	proxyGuestWait  = 20 * time.Second // one daemon command (start waits up to 3 s for its rules)
	proxyStatusWait = 10 * time.Second
)

// settingOf reads instances.<i>.appProxy; a missing one is the zero Setting. A value that is not a valid setting is
// an error (it is not overwritten silently).
func settingOf(meta *jsonord.Obj) (appproxy.Setting, error) {
	var s appproxy.Setting
	v := meta.Get(appproxy.MetaKey)
	switch v.Kind() {
	case jsonord.Null:
		return s, nil
	case jsonord.Object:
	default:
		return s, errs.Newf(errs.MetadataUnreadable, "mas-avd.json 里 %s 不是对象", appproxy.MetaKey)
	}
	o := v.Obj()
	s.Enabled, _ = o.Get("enabled").BoolVal()
	s.Server, _ = o.Get("server").Str()
	s.DNS, _ = o.Get("dns").Str()
	for _, a := range o.Get("apps").Items() {
		if p, ok := a.Str(); ok {
			s.Apps = append(s.Apps, p)
		}
	}
	if err := s.Validate(); err != nil {
		return s, errs.Newf(errs.MetadataUnreadable, "mas-avd.json 里的 %s 用不了：%v", appproxy.MetaKey, err).
			WithAdvice("用 modavd proxy <i> --server、--apps 重新设一次")
	}
	return s, nil
}

// settingValue is s as stored: enabled, server, apps, then dns when set. Other keys of an existing object (a newer
// modavd's) are kept.
func settingValue(old *jsonord.Value, s appproxy.Setting) *jsonord.Value {
	v := jsonord.NewObject()
	if old.Kind() == jsonord.Object {
		v = old.Clone()
	}
	o := v.Obj()
	o.Set("enabled", jsonord.NewBool(s.Enabled))
	o.Set("server", jsonord.NewString(s.Server))
	apps := make([]*jsonord.Value, 0, len(s.Apps))
	for _, p := range s.Apps {
		apps = append(apps, jsonord.NewString(p))
	}
	o.Set("apps", jsonord.NewArray(apps...))
	if s.DNS != "" {
		o.Set("dns", jsonord.NewString(s.DNS))
	} else {
		o.Delete("dns")
	}
	return v
}

// appProxySummary is InstanceInfo.AppProxy; nil when the instance never had one.
func appProxySummary(meta *jsonord.Obj) *AppProxySummary {
	if !meta.Has(appproxy.MetaKey) {
		return nil
	}
	s, _ := settingOf(meta)
	return &AppProxySummary{Enabled: s.Enabled, Server: s.Server, Apps: len(s.Apps)}
}

func newAppProxyResult(i int, s appproxy.Setting) *AppProxyResult {
	apps := slices.Clone(s.Apps)
	if apps == nil {
		apps = []string{}
	}
	return &AppProxyResult{Index: i, Enabled: s.Enabled, Server: s.Server, Apps: apps, DNS: s.DNS, Notes: []string{}}
}

// AppProxy is instance i's per-app proxy, with the guest daemon's state while the instance runs.
func (m *Manager) AppProxy(ctx context.Context, i int) (*AppProxyResult, error) {
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	s, err := settingOf(m.Root.InstanceMeta(i))
	if err != nil {
		return nil, err
	}
	res := newAppProxyResult(i, s)
	if m.guestUp(ctx, i) {
		res.Guest = m.guestStatus(ctx, i, &res.Notes)
	}
	return res, nil
}

// guestUp: instance i runs and Android has booted.
func (m *Manager) guestUp(ctx context.Context, i int) bool {
	if _, ok := m.qemuOf(i); !ok {
		return false
	}
	return m.bootCompleted(ctx, i, statusAdbTimeout, true)
}

// SetAppProxy changes instance i's per-app proxy and, while it runs, applies it at once. An address the host has to
// find (appproxy.Auto, or one without its kind) is probed on the host first.
func (m *Manager) SetAppProxy(ctx context.Context, i int, c appproxy.Change) (*AppProxyResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能修改"); err != nil {
		return nil, err
	}
	if c.Empty() {
		return nil, errs.New(errs.Usage, "没有要改的：on、off、--server、--apps、--add、--remove 至少给一项")
	}
	// The probing (seconds when nothing answers) is done before the metadata lock is taken; the edit under the lock
	// then gets its answer from here.
	var found struct {
		server, note string
		err          error
		done         bool
	}
	resolve := func() (string, string, error) {
		if !found.done {
			found.server, found.note, found.err = autoServer(ctx)
			found.done = true
		}
		return found.server, found.note, found.err
	}
	if c.Server != nil && !strings.EqualFold(strings.TrimSpace(*c.Server), appproxy.Auto) &&
		!strings.Contains(*c.Server, "://") {
		typed, err := withKind(ctx, *c.Server)
		if err != nil {
			return nil, err
		}
		c.Server = &typed
	}
	cur, err := settingOf(m.Root.InstanceMeta(i))
	if err != nil {
		return nil, err
	}
	if _, _, err := cur.Apply(c, resolve); err != nil {
		return nil, appProxyError(err, found.err)
	}

	var next appproxy.Setting
	var notes []string
	err = m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		cur, err := settingOf(meta)
		if err != nil {
			return err
		}
		if next, notes, err = cur.Apply(c, resolve); err != nil {
			return appProxyError(err, found.err)
		}
		meta.Set(appproxy.MetaKey, settingValue(meta.Get(appproxy.MetaKey), next))
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := newAppProxyResult(i, next)
	res.Notes = append(res.Notes, notes...)
	m.Log.Infof("已写入实例 %d 的按应用走代理：%s", i, settingText(next))
	if next.Active() {
		if srv, err := appproxy.ParseServer(next.Server); err == nil {
			if _, err := appproxy.Probe(ctx, hostAddr(srv.Addr), false, netip.AddrPort{}); err != nil {
				res.Notes = append(res.Notes, "现在连不上这个代理，名单里的应用会断网："+err.Error())
			}
		}
	}

	switch _, running := m.qemuOf(i); {
	case !running:
		res.Applied = "next_boot"
		if !next.Active() {
			res.Applied = "off"
		}
		return res, nil
	case !m.bootCompleted(ctx, i, statusAdbTimeout, true):
		res.Applied = "booting"
		return res, nil
	}
	applied, st, err := m.applyAppProxy(ctx, i)
	res.Guest = st
	switch {
	case err != nil:
		res.Applied, res.Error = "failed", errs.As(err).Message
		m.Log.Warnf("实例 %d 按应用走代理没生效：%s", i, res.Error)
	case !applied.Active():
		res.Applied = "off"
	default:
		res.Applied = "now"
	}
	return res, nil
}

// appProxyError is a refused Change as the caller's error: the reason of a failed host lookup, else a bad value.
func appProxyError(err, autoErr error) error {
	if autoErr != nil && errors.Is(err, autoErr) {
		return autoErr
	}
	if e := (*errs.Error)(nil); errors.As(err, &e) {
		return err
	}
	return errs.New(errs.InvalidArgument, err.Error())
}

// TestAppProxy probes instance i's proxy from the host (its kind, and UDP through a SOCKS5 proxy), with the guest
// daemon's state while the instance runs.
func (m *Manager) TestAppProxy(ctx context.Context, i int) (*AppProxyTestResult, error) {
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	s, err := settingOf(m.Root.InstanceMeta(i))
	if err != nil {
		return nil, err
	}
	if s.Server == "" {
		return nil, errs.Newf(errs.Usage, "实例 %d 还没设代理地址", i).
			WithAdvice(fmt.Sprintf("modavd proxy %d --server socks5://127.0.0.1:端口（或 auto：取这台电脑的代理设置）", i))
	}
	srv, err := appproxy.ParseServer(s.Server)
	if err != nil {
		return nil, errs.New(errs.InvalidArgument, err.Error())
	}
	cfg := s.GuestConfig()
	resolver, _ := cfg.Resolver()
	res := &AppProxyTestResult{Index: i, Server: s.Server, HostAddr: hostAddr(srv.Addr).String(), Notes: []string{}}
	if res.ProbeResult, err = appproxy.Probe(ctx, hostAddr(srv.Addr), true, resolver); err != nil {
		return nil, errs.New(errs.ProxyUnavailable, err.Error())
	}
	if res.Kind != srv.Scheme {
		res.UDPError = strings.TrimSpace(res.UDPError + fmt.Sprintf("（设的是 %s，那个端口说的是 %s）", srv.Scheme, res.Kind))
	}
	if m.guestUp(ctx, i) {
		res.Guest = m.guestStatus(ctx, i, &res.Notes)
	}
	return res, nil
}

// hostAddr is where the host reaches a proxy the guest reaches at a: the emulator's 10.0.2.2 is the host's loopback.
func hostAddr(a netip.AddrPort) netip.AddrPort {
	if a.Addr() == netip.MustParseAddr(appproxy.HostLoopback) {
		return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), a.Port())
	}
	return a
}

// autoServer is appproxy.Auto: the host's own proxy setting, its kind probed when the setting does not say it (a
// Windows entry for every protocol) or says HTTP (a mixed port speaks SOCKS5 too, which also carries UDP).
func autoServer(ctx context.Context) (string, string, error) {
	sg, ok := hostproxy.GuestSuggestion()
	if !ok {
		return "", "", errs.New(errs.ProxyUnavailable,
			"这台电脑没设代理（ALL_PROXY、HTTPS_PROXY、HTTP_PROXY 环境变量和 Windows 的手动代理都没有）").
			WithAdvice("用 --server 给出代理地址，比如 socks5://127.0.0.1:7890（代理软件里 SOCKS5 或混合端口的地址）")
	}
	note := "代理地址取自" + sg.From + "：" + sg.Addr.String()
	if sg.AuthDropped {
		note += "（去掉了设置里的账号密码：按应用走代理不支持要认证的代理，名单里的应用会连不上）"
	}
	kind := sg.Kind
	if kind != "socks5" {
		r, err := appproxy.Probe(ctx, sg.Addr, false, netip.AddrPort{})
		switch {
		case err == nil:
			kind = r.Kind
		case kind == "":
			return "", "", errs.Newf(errs.ProxyUnavailable, "%s，但%v", note, err)
		default:
			note += "（现在连不上，先照设置记成 " + kind + "）"
		}
	}
	return kind + "://" + sg.Addr.String(), note, nil
}

// withKind is a typed address without its kind, completed by probing it ("127.0.0.1:7890" -> "socks5://...").
func withKind(ctx context.Context, typed string) (string, error) {
	v := strings.TrimSpace(typed)
	host, port, ok := strings.Cut(v, ":")
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ap, err := netip.ParseAddrPort(host + ":" + port)
	if !ok || err != nil {
		return "", errs.Newf(errs.InvalidArgument, "代理地址要写成 socks5://IP:端口 或 http://IP:端口，收到「%s」", typed)
	}
	r, err := appproxy.Probe(ctx, hostAddr(ap), false, netip.AddrPort{})
	if err != nil {
		return "", errs.New(errs.ProxyUnavailable, err.Error()).
			WithAdvice("代理软件开着时再试，或者写全：socks5://" + v + " 或 http://" + v)
	}
	return r.Kind + "://" + v, nil
}

func settingText(s appproxy.Setting) string {
	state := "关"
	if s.Enabled {
		state = "开"
	}
	return fmt.Sprintf("%s，%s，%d 个应用", state, orText(s.Server, "没有地址"), len(s.Apps))
}

// ---- the guest

const proxyExe = appproxy.GuestDir + "/" + guest.ProxyDaemonName

// applyAppProxy puts s into the running guest of instance i: the daemon (pushed when the guest's copy differs) and
// config.json, then `start` (which reloads a running daemon); `stop` when it is not active. One at a time per
// instance (the proxy lock), and the setting is read under that lock: when a change and a boot both apply it, the
// later one puts in the newer setting, never the one the other read before it changed.
func (m *Manager) applyAppProxy(ctx context.Context, i int) (appproxy.Setting, *appproxy.Status, error) {
	l, err := m.lock(ctx, i, "proxy", proxyLockWait, "正在设置按应用走代理")
	if err != nil {
		return appproxy.Setting{}, nil, err
	}
	defer l.Unlock()
	s, err := settingOf(m.Root.InstanceMeta(i))
	if err != nil {
		return s, nil, err
	}
	st, err := m.putAppProxy(ctx, i, s)
	return s, st, err
}

// putAppProxy is applyAppProxy's work, under its lock.
func (m *Manager) putAppProxy(ctx context.Context, i int, s appproxy.Setting) (*appproxy.Status, error) {
	if !s.Active() {
		return m.stopAppProxy(ctx, i)
	}
	d, err := daemonFile()
	if err != nil {
		return nil, err
	}
	if err := m.ensureDaemon(ctx, i, d); err != nil {
		return nil, err
	}
	cfg := s.GuestConfig().Marshal()
	f, err := os.CreateTemp("", "modavd-proxy-config-*.json")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "写不了临时文件: "+err.Error())
	}
	_, werr := f.Write(cfg)
	f.Close()
	defer os.Remove(f.Name())
	if werr != nil {
		return nil, errs.Wrap(errs.Internal, werr, "写不了临时文件: "+werr.Error())
	}
	conf := appproxy.GuestDir + "/" + appproxy.ConfigName
	if !m.push(ctx, i, f.Name(), conf+".new") {
		return nil, errs.Newf(errs.Internal, "实例 %d 推送代理设置失败", i)
	}
	if r := m.shell(ctx, i, "mv -f "+conf+".new "+conf, 0); r.Code != 0 {
		return nil, errs.Newf(errs.Internal, "实例 %d 换不了代理设置：%s", i, tail(r.Out, 200))
	}
	st, err := m.proxyDaemon(ctx, i, d.sum, "start", proxyGuestWait)
	if err != nil {
		return st, err
	}
	if !st.Running || st.Rules != appproxy.RulesOK || st.Error != "" {
		why := orText(st.Error, st.RulesError)
		if why == "" {
			why = "规则状态 " + orText(st.Rules, "未知")
		}
		return st, errs.Newf(errs.Unsupported, "实例 %d 的客体程序没把规则挂上：%s", i, why).
			WithAdvice("这个系统镜像可能不支持按应用走代理；modavd collect-info 收集信息后反馈")
	}
	return st, nil
}

// carried is the guest daemon this modavd carries: a host copy to push and its SHA-256.
type carried struct {
	local, sum string
}

// daemonFile is the daemon of this modavd (a variable for the tests: a test build carries none).
var daemonFile = func() (carried, error) {
	b, err := guest.ProxyDaemon()
	if err != nil {
		return carried{}, errs.Wrap(errs.NotReady, err, err.Error())
	}
	local, err := guest.LocalProxyDaemon(filepath.Join(os.TempDir(), "modavd-guest"))
	if err != nil {
		return carried{}, errs.Wrap(errs.Internal, err, "写不了客体程序的本机副本: "+err.Error())
	}
	sum := sha256.Sum256(b)
	return carried{local: local, sum: hex.EncodeToString(sum[:])}, nil
}

// ensureDaemon makes the guest's daemon the one d is, pushing it when the guest's copy differs. Another version may
// be running then: it is killed, not stopped, so its rules stay and the listed apps have no network for the moment
// until the new one replaces them (a stop would let them go direct meanwhile).
func (m *Manager) ensureDaemon(ctx context.Context, i int, d carried) error {
	if r := m.shell(ctx, i, "mkdir -p "+appproxy.GuestDir, 0); r.Code != 0 {
		return errs.Newf(errs.Internal, "实例 %d 里建不了 %s：%s", i, appproxy.GuestDir, tail(r.Out, 200))
	}
	if m.guestDaemonSum(ctx, i) == d.sum {
		return nil
	}
	if !m.push(ctx, i, d.local, proxyExe+".new") {
		return errs.Newf(errs.Internal, "实例 %d 推送 %s 失败", i, guest.ProxyDaemonName)
	}
	if r := m.su(ctx, i, replaceDaemonScript(proxyExe), proxyGuestWait); r.Code != 0 {
		return suError(i, r.Out)
	}
	return nil
}

// guestDaemonSum is the SHA-256 of the guest's daemon, "" when it is not there or cannot be read.
func (m *Manager) guestDaemonSum(ctx context.Context, i int) string {
	return sumOf(m.shell(ctx, i, "sha256sum "+proxyExe+" 2>/dev/null", 0).Out)
}

// sumOf is the hash of `sha256sum <file>`'s output: 64 lower-case hex digits, else "".
func sumOf(out string) string {
	f := strings.Fields(out)
	if len(f) == 0 || len(f[0]) != 64 || strings.Trim(f[0], "0123456789abcdef") != "" {
		return ""
	}
	return f[0]
}

// replaceDaemonScript puts the pushed daemon in place of the old one (root; no single quotes, see Manager.su),
// without running the old file (it is being replaced because its hash is not this modavd's): the running daemon's pid
// is in its lock file, and it is killed only when /proc says that pid runs this path (a lock file left by a dead
// daemon could name another process by now; an old daemon whose file was replaced before shows it deleted).
func replaceDaemonScript(exe string) string {
	lock := path.Dir(exe) + "/" + appproxy.LockName
	return "chmod 755 " + exe + ".new && { p=$(cat " + lock + " 2>/dev/null); case \"$p\" in \"\"|*[!0-9]*) ;; *) " +
		"case \"$(readlink /proc/$p/exe)\" in \"" + exe + "\"|\"" + exe + " (deleted)\") kill -9 \"$p\" ;; esac ;; esac; } && " +
		"mv -f " + exe + ".new " + exe
}

// rootDaemonScript runs the guest's daemon with cmd as root (no single quotes, see Manager.su) only when the file
// there is the one this modavd carries: the check and the run are one command, so nothing can be put in its place in
// between (the execution bridge checks its payload the same way). Exit 126 when it is not.
func rootDaemonScript(exe, sum, cmd string) string {
	return "[ -x " + exe + " ] || exit 127; set -- $(sha256sum " + exe + " 2>/dev/null); [ \"$1\" = \"" + sum +
		"\" ] || exit 126; exec " + exe + " " + cmd
}

// stopAppProxy stops the daemon and removes its rules, when the guest has it at all. A daemon of another version is
// first replaced by this modavd's, as only that one is run as root.
func (m *Manager) stopAppProxy(ctx context.Context, i int) (*appproxy.Status, error) {
	if m.shell(ctx, i, "test -x "+proxyExe, 0).Code != 0 {
		return nil, nil
	}
	d, err := daemonFile()
	if err != nil {
		return nil, err
	}
	if err := m.ensureDaemon(ctx, i, d); err != nil {
		return nil, err
	}
	return m.proxyDaemon(ctx, i, d.sum, "stop", proxyGuestWait)
}

// guestStatus is the daemon's status for a query, nil when it cannot be asked; a daemon other than this modavd's is
// not run (it is replaced the next time the setting is applied), which notes says.
func (m *Manager) guestStatus(ctx context.Context, i int, notes *[]string) *appproxy.Status {
	d, err := daemonFile()
	if err != nil {
		return nil
	}
	st, err := m.proxyDaemon(ctx, i, d.sum, "status", proxyStatusWait)
	if errors.Is(err, errOtherDaemon) {
		*notes = append(*notes, errs.As(err).Message+"；下次开机或改设置时换上")
	}
	return st
}

// errOtherDaemon: the guest's daemon is not the one this modavd carries, so it was not run.
var errOtherDaemon = errors.New("the guest's modavd-proxyd is not this modavd's")

// proxyDaemon runs `modavd-proxyd <cmd>` as root, after checking it is the daemon whose SHA-256 is sum, and reads the
// Status it prints (its last line).
func (m *Manager) proxyDaemon(ctx context.Context, i int, sum, cmd string, timeout time.Duration) (*appproxy.Status, error) {
	r := m.su(ctx, i, rootDaemonScript(proxyExe, sum, cmd), timeout)
	switch {
	case r.Code == 127 && !strings.Contains(r.Out, "{"):
		return nil, errs.Newf(errs.NotFound, "实例 %d 里没有按应用走代理的客体程序", i)
	case r.Code == 126 && !strings.Contains(r.Out, "{"):
		return nil, errs.Wrap(errs.Unsupported, errOtherDaemon, fmt.Sprintf("实例 %d 里的 %s 和这个 modavd 带的不一样，不以 root 运行它",
			i, guest.ProxyDaemonName))
	}
	lines := strings.Split(strings.TrimSpace(r.Out), "\n")
	var st appproxy.Status
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &st); err != nil {
		return nil, suError(i, r.Out)
	}
	return &st, nil
}

// suError is a failed root command: "not found" for su itself means an image without root.
func suError(i int, out string) error {
	if strings.Contains(out, "su: not found") || strings.Contains(out, "su: inaccessible") {
		return errs.Newf(errs.Unsupported, "实例 %d 的系统镜像没有 su，不支持按应用走代理", i)
	}
	return errs.Newf(errs.Internal, "实例 %d 的客体程序没有正常回答：%s", i, orText(tail(out, 300), "没有输出"))
}

// setupAppProxy is the boot's step for the per-app proxy: in when the setting is active, otherwise a daemon an
// earlier change left running (the guest kept running meanwhile) is stopped. Failures are warnings; the listed apps
// then go direct until it is set again (a boot without rules is like one before the setting existed).
func (m *Manager) setupAppProxy(ctx context.Context, i int) {
	if !m.Root.InstanceMeta(i).Has(appproxy.MetaKey) {
		return
	}
	t := time.Now()
	s, st, err := m.applyAppProxy(ctx, i)
	switch {
	case err != nil && s.Active():
		m.Log.Warnf("实例 %d 按应用走代理没生效（名单里的应用这次开机直连）：%s", i, errs.As(err).Message)
	case err != nil:
		m.Log.Warnf("实例 %d 按应用走代理没设上：%s", i, errs.As(err).Message)
	case !s.Active():
		if st != nil {
			m.Log.Infof("实例 %d 按应用走代理：已关，客体程序已停", i)
		}
	default:
		m.Log.Infof("实例 %d 按应用走代理：%s（%.1f 秒）", i, guestSummary(s, st), time.Since(t).Seconds())
	}
}

// guestSummary is "2 个应用（1 个已装）→ socks5://10.0.2.2:7890，UDP 可用，规则就位".
func guestSummary(s appproxy.Setting, st *appproxy.Status) string {
	installed := 0
	for _, a := range st.Apps {
		if a.UID != 0 {
			installed++
		}
	}
	udp := map[string]string{"ok": "UDP 可用", "unsupported": "UDP 不通（代理不转 UDP）", "failed": "UDP 失败"}[st.Server.UDP]
	if udp == "" {
		udp = "UDP 还没用过"
	}
	rules := "规则就位"
	if st.Rules != appproxy.RulesOK {
		rules = "规则 " + orText(st.Rules, "未知")
	}
	return fmt.Sprintf("%d 个应用（%d 个已装）→ %s，%s，%s", len(s.Apps), installed, s.Server, udp, rules)
}
