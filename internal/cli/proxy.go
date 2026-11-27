package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/emu"
	"modavd/internal/errs"
)

var (
	optProxyServer = flagSpec{"server", kindString, "URL",
		"代理地址：socks5://IP:端口 或 http://IP:端口（127.0.0.1、localhost 记成实例里看到的 10.0.2.2；不写协议就探测）；auto：取这台电脑的代理设置"}
	optProxyAdd    = flagSpec{"add", kindStrings, "包名", "加进名单，可写多次或用逗号隔开"}
	optProxyRemove = flagSpec{"remove", kindStrings, "包名", "从名单里去掉，可写多次或用逗号隔开"}
	optProxyApps   = flagSpec{"apps", kindStrings, "包名", "整个换掉名单，可写多次或用逗号隔开（--apps \"\" 清空）"}
)

func proxyCommand() *command {
	return &command{
		name: "proxy", args: "<i> [on|off|test] [--server URL|auto] [--add 包名] [--remove 包名] [--apps 包名]",
		summary: "按应用走代理：名单里的应用走代理，其余直连",
		details: "不带别的参数是查看：开关、地址、名单，实例开着时附上客体里的实际状态（规则、代理连没连上、各应用装没装）。" +
			"on 打开（第一次没给地址就当 --server auto），off 关掉（地址和名单留着）；--server、--add、--remove、--apps 改设置，可以和 on 一起给。" +
			"代理要是 SOCKS5（最好是代理软件的混合端口，UDP 也能转）或 HTTP（只转 TCP，名单里的应用 UDP 不通）。" +
			"代理连不上时名单里的应用断网，不会改走直连。实例开着就马上生效，关着就下次开机生效（用 modavd 或管理器开机时）。" +
			"test 在这台电脑上探测代理：是什么协议、UDP 能不能用。" +
			"海外游戏用 Google 登录的，把 Google Play 服务（com.google.android.gms）也加进名单。" +
			"探测不到代理是退出码 9（proxy_unavailable）。",
		flags:    []flagSpec{optProxyServer, optProxyAdd, optProxyRemove, optProxyApps},
		posNames: []string{"实例编号", "on、off 或 test"}, minPos: 1, maxPos: 2,
		final: true,
		run:   runProxy, human: humanProxy,
	}
}

func runProxy(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	action := ""
	if len(c.inv.pos) > 1 {
		action = strings.ToLower(c.inv.pos[1])
	}
	ch, err := buildProxyChange(c.inv, action)
	if err != nil {
		return nil, err
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	switch {
	case action == "test":
		return m.TestAppProxy(c.ctx, i)
	case ch.Empty():
		return m.AppProxy(c.ctx, i)
	}
	return m.SetAppProxy(c.ctx, i, ch)
}

// buildProxyChange is the change the options and on / off ask for; test takes no options.
func buildProxyChange(inv *invocation, action string) (appproxy.Change, error) {
	var ch appproxy.Change
	switch action {
	case "":
	case "on", "off":
		on := action == "on"
		ch.Enabled = &on
	case "test":
		for _, f := range []string{"server", "add", "remove", "apps"} {
			if inv.isSet(f) {
				return ch, errs.Newf(errs.Usage, "test 不和 --%s 一起用：先改设置，再 test", f)
			}
		}
		return ch, nil
	default:
		return ch, errs.Newf(errs.Usage, "第二个参数只能是 on、off 或 test，收到「%s」", action).
			WithAdvice(findCommand("proxy").usageAdvice())
	}
	if inv.isSet("server") {
		s := inv.strOpt("server")
		ch.Server = &s
	}
	if inv.isSet("apps") {
		apps := appproxy.SplitApps(inv.strsOpt("apps"))
		ch.Apps = &apps
	}
	ch.Add = appproxy.SplitApps(inv.strsOpt("add"))
	ch.Remove = appproxy.SplitApps(inv.strsOpt("remove"))
	return ch, nil
}

func humanProxy(w io.Writer, data any) {
	switch r := data.(type) {
	case *emu.AppProxyResult:
		formatAppProxy(w, r)
	case *emu.AppProxyTestResult:
		formatAppProxyTest(w, r)
	}
}

func formatAppProxy(w io.Writer, r *emu.AppProxyResult) {
	state := "关"
	if r.Enabled {
		state = "开"
	}
	fmt.Fprintf(w, "实例 %d 按应用走代理：%s\n", r.Index, state)
	fmt.Fprintf(w, "代理：%s\n", orDash(r.Server))
	if len(r.Apps) == 0 {
		fmt.Fprintln(w, "名单：空")
	} else {
		fmt.Fprintf(w, "名单：%s\n", strings.Join(r.Apps, "、"))
	}
	if r.DNS != "" {
		fmt.Fprintf(w, "DNS：%s（经代理用 TCP 查）\n", r.DNS)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "提示："+n)
	}
	switch r.Applied {
	case "now":
		fmt.Fprintln(w, "已生效")
	case "booting":
		fmt.Fprintln(w, "实例正在开机，开完马上生效")
	case "next_boot":
		fmt.Fprintln(w, "实例没开，下次开机生效")
	case "off":
		fmt.Fprintln(w, "已关，名单里的应用直连")
	case "failed":
		fmt.Fprintln(w, "没生效："+r.Error)
	}
	if g := r.Guest; g != nil {
		formatProxyGuest(w, g)
	} else if r.GuestError != "" {
		fmt.Fprintln(w, "客体：问不到状态（"+r.GuestError+"）")
	}
}

// formatProxyGuest is the daemon's state in a few lines.
func formatProxyGuest(w io.Writer, g *appproxy.Status) {
	if !g.Running {
		line := "客体：没在运行"
		if g.Rules == appproxy.RulesPartial {
			line += "，但规则还在（名单里的应用现在断网）"
		}
		if g.Error != "" {
			line += "（" + g.Error + "）"
		}
		fmt.Fprintln(w, line)
		return
	}
	rules := map[string]string{appproxy.RulesOK: "就位", appproxy.RulesPartial: "缺了几条，正在补",
		appproxy.RulesFailed: "挂不上：" + g.RulesError, appproxy.RulesOff: "没有"}[g.Rules]
	fmt.Fprintf(w, "客体：在运行（pid %d），规则%s\n", g.PID, orDash(rules))
	// TCP is the last connection's outcome only: one that failed may be a target that did not answer
	tcp := map[string]string{"ok": "能连", "failed": "最近一次没连上", "unknown": "还没用过"}[g.Server.TCP]
	udp := map[string]string{"ok": "可用", "unsupported": "不通（代理不转 UDP）", "failed": "失败", "unknown": "还没用过"}[g.Server.UDP]
	fmt.Fprintf(w, "代理：TCP %s，UDP %s\n", orDash(tcp), orDash(udp))
	if g.Server.TCP == "failed" {
		line := "最近的错误"
		if at := localStamp(g.Server.LastErrorAt); at != "" {
			line += "（" + at + "）"
		}
		line += "：" + orDash(g.Server.LastError)
		if at := localStamp(g.Server.LastOKAt); at != "" {
			line += "；上次连上是 " + at
		}
		fmt.Fprintln(w, line)
	}
	var missing []string
	for _, a := range g.Apps {
		if a.UID == 0 {
			missing = append(missing, a.Package)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(w, "还没装的（装上就生效）：%s\n", strings.Join(missing, "、"))
	}
	c := g.Counters
	fmt.Fprintf(w, "已转发：TCP %d 个（失败 %d），DNS %d 次（失败 %d），UDP 丢了 %d 个包\n", c.TCP, c.TCPFailed, c.DNS,
		c.DNSFailed, c.UDPDropped)
}

func formatAppProxyTest(w io.Writer, r *emu.AppProxyTestResult) {
	kind := map[string]string{"socks5": "SOCKS5", "http": "HTTP 代理"}[r.Kind]
	fmt.Fprintf(w, "实例 %d 的代理 %s（在这台电脑上是 %s）：%s\n", r.Index, r.Server, r.HostAddr, orDash(kind))
	switch r.UDP {
	case "ok":
		fmt.Fprintln(w, "UDP：可用")
	case "unsupported":
		fmt.Fprintln(w, "UDP：不通，名单里的应用只能用 TCP；换成代理软件的 SOCKS5 或混合端口就都能用")
	case "failed":
		fmt.Fprintln(w, "UDP：失败（"+r.UDPError+"）")
	}
	if r.UDP != "failed" && r.UDPError != "" {
		fmt.Fprintln(w, "提示："+r.UDPError)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "提示："+n)
	}
	if r.Guest != nil {
		formatProxyGuest(w, r.Guest)
	} else if r.GuestError != "" {
		fmt.Fprintln(w, "客体：问不到状态（"+r.GuestError+"）")
	}
}

// localStamp is an RFC 3339 time of the guest's as this computer's local "01-02 15:04:05"; "" when there is none.
func localStamp(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.Local().Format("01-02 15:04:05")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
