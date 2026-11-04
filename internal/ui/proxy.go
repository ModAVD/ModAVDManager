package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/report"
)

// The per-app proxy (ModAVD/ModAVD#81) in the manager: the endpoints of its dialog, and the 代理 mark on the cards of
// the instances that have it on. A running instance's mark comes from its guest daemon's status, asked every
// proxyEvery (one root command, see emu.Manager.AppProxy) while a page is connected, and right away when the
// instance's state or setting changes.

const (
	proxyEvery     = 30 * time.Second
	proxyCheckWait = 20 * time.Second
	packagesWait   = 15 * time.Second
	// gmsPackage is the one system app the dialog offers: Google sign-in in a game goes through it.
	gmsPackage = "com.google.android.gms"
	gmsName    = "Google Play 服务"
)

// proxyMark is how the per-app proxy of a running instance is doing, for its card and the dialog.
type proxyMark struct {
	Index int    `json:"index"`
	Tone  string `json:"tone"` // "ok", "warn" (to look at, not known to cut the listed apps off or send them direct), "err"
	Text  string `json:"text"`
}

// proxyView is GET and PUT /api/instances/{i}/proxy: AppProxyResult with the mark while the guest has it on.
type proxyView struct {
	*emu.AppProxyResult
	Mark *proxyMark `json:"mark"`
}

// markOf is the mark of res. online: the instance runs, has booted and no start, stop or after-boot step is under
// way (the guest is asked only then); nil when it has nothing to show. probe asks the host whether the proxy
// answers, only when the guest's last connection through it failed: one that failed may be a target that never
// answered, not the proxy.
func markOf(res *emu.AppProxyResult, online bool, probe func() error) *proxyMark {
	if !res.Enabled || len(res.Apps) == 0 || !online || res.Applied == "booting" {
		return nil
	}
	m := &proxyMark{Index: res.Index, Tone: "err"}
	g := res.Guest
	switch {
	case res.Applied == "failed":
		m.Text = "没生效：" + res.Error
	case g == nil && res.GuestError != "":
		m.Tone, m.Text = "warn", "问不到客体里的状态："+res.GuestError
	case g == nil:
		m.Text = "客体里没有按应用走代理在跑：名单里的应用现在直连"
		if len(res.Notes) > 0 {
			m.Text += "（" + strings.Join(res.Notes, "；") + "）"
		}
	case !g.Running && g.Rules == appproxy.RulesPartial:
		m.Text = "客体程序停了，规则还在：名单里的应用现在断网"
	case !g.Running:
		m.Text = "客体程序没在运行：名单里的应用现在直连"
		if g.Error != "" {
			m.Text += "（" + g.Error + "）"
		}
	case g.Rules == appproxy.RulesFailed:
		m.Text = "规则挂不上：" + orText(g.RulesError, "原因不明") + "；名单里的应用现在直连"
	case g.Rules == appproxy.RulesPartial:
		m.Tone, m.Text = "warn", "规则缺了几条，正在补上"
	case g.Rules != appproxy.RulesOK:
		m.Text = "规则没挂上：名单里的应用现在直连"
	case g.Server.TCP == "failed":
		if err := probe(); err != nil {
			m.Text = "代理连不上（名单里的应用现在断网）：" + errs.As(err).Message
		} else {
			m.Tone, m.Text = "warn", "最近一次经代理的连接没成功："+orText(g.Server.LastError, "原因不明")+"（这台电脑上能连到代理）"
		}
	default:
		apps := guestApps(g)
		arrow := " → "
		if strings.HasSuffix(apps, "）") { // no space after a full-width bracket
			arrow = "→ "
		}
		m.Tone, m.Text = "ok", "已生效："+apps+arrow+res.Server
		if g.Server.UDP == "unsupported" {
			m.Text += "（UDP 不通）"
		}
	}
	return m
}

// guestApps is "2 个应用" or "2 个应用（1 个还没装）".
func guestApps(g *appproxy.Status) string {
	missing := 0
	for _, a := range g.Apps {
		if a.UID == 0 {
			missing++
		}
	}
	s := fmt.Sprintf("%d 个应用", len(g.Apps))
	if missing > 0 {
		s += fmt.Sprintf("（%d 个还没装）", missing)
	}
	return s
}

func orText(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// watched: the instance's guest is asked for its mark (see markOf's online).
func watched(in emu.InstanceInfo) bool {
	ap := in.AppProxy
	return ap != nil && ap.Enabled && ap.Apps > 0 && in.Status == emu.StatusOnline && in.Busy == ""
}

// watchKey changes when the guest is to be asked again before proxyEvery is up.
func watchKey(in emu.InstanceInfo) string {
	ap := in.AppProxy
	return fmt.Sprint(in.Status, in.Busy, in.StartedAtMS, ap.Enabled, ap.Server, ap.Apps)
}

// probeFor asks the host whether the proxy of res answers (TCP only).
func probeFor(ctx context.Context, b backend, res *emu.AppProxyResult) func() error {
	return func() error {
		_, err := b.ProbeAppProxy(ctx, res.Server, false)
		return err
	}
}

// ---- the marks ---------------------------------------------------------------------------------------------------

// proxyMarks holds the marks by instance; every change goes out as the proxy event (all of them, API.md).
type proxyMarks struct {
	mu   sync.Mutex
	by   map[int]*markEntry
	raw  []byte        // the last proxy event sent; nil before the first
	wake chan struct{} // a pending "look at the list" (capacity 1: requests coalesce)
}

type markEntry struct {
	key  string // watchKey when the guest was asked
	at   time.Time
	mark *proxyMark
}

func newProxyMarks() *proxyMarks {
	return &proxyMarks{by: map[int]*markEntry{}, wake: make(chan struct{}, 1)}
}

func (p *proxyMarks) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// due: the guest of instance i is to be asked now.
func (p *proxyMarks) due(i int, key string, now time.Time, every time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.by[i]
	return e == nil || e.key != key || now.Sub(e.at) >= every
}

// put records instance i's mark; a nil mark with an empty key forgets the instance.
func (p *proxyMarks) put(i int, key string, m *proxyMark, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m == nil && key == "" {
		delete(p.by, i)
		return
	}
	p.by[i] = &markEntry{key: key, at: now, mark: m}
}

// event is the proxy event of the marks held, and whether it differs from the last one sent (which it then is).
func (p *proxyMarks) event() ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	marks := []proxyMark{}
	for _, e := range p.by {
		if e.mark != nil {
			marks = append(marks, *e.mark)
		}
	}
	sort.Slice(marks, func(a, b int) bool { return marks[a].Index < marks[b].Index })
	data, err := json.Marshal(struct {
		Marks []proxyMark `json:"marks"`
	}{marks})
	if err != nil || string(data) == string(p.raw) {
		return data, false
	}
	p.raw = data
	return data, true
}

// last is the last proxy event sent, for a stream that connects.
func (p *proxyMarks) last() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.raw
}

// setMark records the mark a request found (the dialog's GET and PUT) and sends the event when that changed it. The
// key stays: the next look at the list decides on its own whether to ask again.
func (s *server) setMark(i int, m *proxyMark) {
	p := s.marks
	p.mu.Lock()
	e := p.by[i]
	switch {
	case e != nil:
		e.mark, e.at = m, time.Now()
	case m != nil:
		p.by[i] = &markEntry{at: time.Now(), mark: m}
	}
	p.mu.Unlock()
	s.sendMarks()
}

func (s *server) sendMarks() {
	if data, changed := s.marks.event(); changed {
		s.hub.send("proxy", data)
	}
}

// proxyLoop asks the guests whose marks are due, after every instance list (refreshInstances kicks it).
func (s *server) proxyLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.marks.wake:
		}
		s.checkProxies()
	}
}

func (s *server) checkProxies() {
	list := s.hub.latest()
	if list == nil {
		return
	}
	seen := map[int]bool{}
	for _, in := range list.Instances {
		i := in.Index
		seen[i] = true
		if !watched(in) {
			s.marks.put(i, "", nil, time.Time{})
			continue
		}
		key, now := watchKey(in), time.Now()
		if !s.marks.due(i, key, now, s.proxyEvery) {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, proxyCheckWait)
		res, err := s.poll.AppProxy(ctx, i)
		var m *proxyMark
		switch {
		case s.ctx.Err() != nil:
			cancel()
			return
		case err != nil:
			m = &proxyMark{Index: i, Tone: "warn", Text: "问不到客体里的状态：" + errs.As(err).Message}
		default:
			m = markOf(res, true, probeFor(ctx, s.poll, res))
		}
		cancel()
		s.marks.put(i, key, m, now)
	}
	s.marks.mu.Lock()
	for i := range s.marks.by {
		if !seen[i] {
			delete(s.marks.by, i)
		}
	}
	s.marks.mu.Unlock()
	s.sendMarks()
}

// online is markOf's online for a request: from the polled list when it is recent, else what the answer shows.
func (s *server) online(i int, res *emu.AppProxyResult) bool {
	if in, ok := s.hub.cachedInstance(i, statusMaxAge); ok {
		return in.Status == emu.StatusOnline && in.Busy == ""
	}
	return res.Guest != nil
}

// ---- endpoints ---------------------------------------------------------------------------------------------------

func (s *server) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.AppProxy(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	m := markOf(res, s.online(i, res), probeFor(r.Context(), s.mgr, res))
	s.setMark(i, m)
	writeJSON(w, http.StatusOK, proxyView{res, m})
}

func (s *server) handleProxyPut(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool     `json:"enabled"`
		Server  *string   `json:"server"`
		Apps    *[]string `json:"apps"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil && body.Server == nil && body.Apps == nil {
		writeError(w, errs.New(errs.Usage, "没有要改的设置").WithAdvice("至少给出 enabled、server、apps 中的一项"))
		return
	}
	ctx := detached(r)
	res, err := s.mgr.SetAppProxy(ctx, i, appproxy.Change{Enabled: body.Enabled, Server: body.Server, Apps: body.Apps})
	if err != nil {
		writeError(w, err)
		return
	}
	online := res.Applied == "now" || res.Applied == "failed"
	m := markOf(res, online, probeFor(ctx, s.mgr, res))
	s.setMark(i, m)
	s.hub.requestRefresh() // the cards' app_proxy summary
	writeJSON(w, http.StatusOK, proxyView{res, m})
}

func (s *server) handleProxySuggest(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.SuggestAppProxy(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleProxyProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server string `json:"server"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := s.mgr.ProbeAppProxy(r.Context(), body.Server, true)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// appPackage is one app the dialog offers for the list.
type appPackage struct {
	Package string `json:"package"`
	Name    string `json:"name"` // a game's Chinese name (report.GameName), 「Google Play 服务」; "" for the others
	UID     int    `json:"uid"`
	System  bool   `json:"system"` // Google Play 服务, the one system app offered
}

// packagesCmd lists the third-party apps, then (after the marker) Google Play 服务 when it is there; `pm list
// packages` matches names by substring, so the second part is filtered to the exact name.
const packagesCmd = "pm list packages -3 -U; echo " + systemMarker + "; pm list packages -s -U " + gmsPackage

const systemMarker = "--modavd-system--"

func notRunning(i int) error {
	// 409 per API.md, as for the screenshot
	return errs.Newf(errs.Busy, "实例 %d 没有开机", i).WithAdvice("开机后才能列出装了哪些应用；没开机时可以手填包名")
}

// handleProxyPackages is GET /api/instances/{i}/packages: the apps the dialog lists, from the running guest.
func (s *server) handleProxyPackages(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	// A recent list that says it is not booted saves an adb call (pm needs Android up anyway).
	if in, ok := s.hub.cachedInstance(i, statusMaxAge); ok && !in.Booted {
		writeError(w, notRunning(i))
		return
	}
	res, err := s.mgr.Shell(r.Context(), i, packagesCmd, packagesWait)
	if err != nil {
		if errs.CodeOf(err) == errs.Unsupported { // emu: not running
			err = notRunning(i)
		}
		writeError(w, err)
		return
	}
	pkgs, ok := parsePackages(res.Output)
	if !ok {
		writeError(w, errs.Newf(errs.Internal, "列不出实例 %d 装的应用：%s", i, orText(clip(strings.TrimSpace(res.Output), 300), "没有输出")))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Packages []appPackage `json:"packages"`
	}{pkgs})
}

// parsePackages reads packagesCmd's output ("package:<name> uid:<uid>" lines). false when the shell did not get to the
// marker, or pm said something other than packages for the third-party part (it fails while Android is not up); a
// guest with no third-party app at all says nothing there. Games gameNames knows come first, then by name; the system
// app last.
func parsePackages(out string) ([]appPackage, bool) {
	pkgs := []appPackage{}
	system, marker, other := false, false, false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == systemMarker {
			system, marker = true, true
			continue
		}
		rest, found := strings.CutPrefix(line, "package:")
		if !found {
			other = other || (line != "" && !system)
			continue
		}
		name, uid, _ := strings.Cut(rest, " uid:")
		name = strings.TrimSpace(name)
		if !appproxy.ValidPackage(name) || (system && name != gmsPackage) {
			continue
		}
		// "uid:10123", or "uid:10123,1010123" with more users
		uid = strings.TrimSpace(uid)
		if k := strings.IndexFunc(uid, func(r rune) bool { return r < '0' || r > '9' }); k >= 0 {
			uid = uid[:k]
		}
		p := appPackage{Package: name, System: system}
		p.UID, _ = strconv.Atoi(uid)
		p.Name, _ = report.GameName(name)
		if system {
			p.Name = gmsName
		}
		pkgs = append(pkgs, p)
	}
	if !marker || (other && !slices.ContainsFunc(pkgs, func(p appPackage) bool { return !p.System })) {
		return nil, false
	}
	slices.SortStableFunc(pkgs, func(a, b appPackage) int {
		rank := func(p appPackage) int {
			switch {
			case p.System:
				return 2
			case p.Name != "":
				return 0
			}
			return 1
		}
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.Package, b.Package)
	})
	return pkgs, true
}
