//go:build linux

package appproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The guest's tools (Android's iptables is the legacy one; ip is iproute2).
var (
	ipCmd       = "/system/bin/ip"
	iptablesCmd = "/system/bin/iptables"
	restoreCmd  = "/system/bin/iptables-restore"
	saveCmd     = "/system/bin/iptables-save"
)

func run(stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s：%v %s", path.Base(name), strings.Join(args, " "), err,
			strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// iptables runs iptables on the mangle table, waiting for the lock netd also takes.
func iptables(args ...string) error {
	_, err := run("", iptablesCmd, append([]string{"-w", "-t", "mangle"}, args...)...)
	return err
}

// setupRules puts everything in place for these uids, changing only what differs.
func setupRules(uids []UIDRange) error {
	if _, err := run(restoreInput(uids), restoreCmd, "-w", "--noflush"); err != nil {
		return err
	}
	for _, j := range jumps {
		if iptables("-C", j.builtin, "-j", j.chain) != nil {
			if err := iptables("-I", j.builtin, "1", "-j", j.chain); err != nil {
				return err
			}
		}
	}
	if _, err := run("", ipCmd, append([]string{"-4", "route"}, routeArgs...)...); err != nil {
		return err
	}
	have, err := currentRules()
	if err != nil {
		return err
	}
	del, add := planRules(have, wantRules(uids))
	for _, r := range add { // before deleting: no gap for uids that stay
		if dnsRule(r) {
			if err := addRuleNetlink(r); err != nil {
				return err
			}
			continue
		}
		if _, err := run("", ipCmd, append([]string{r.family(), "rule"}, r.addArgs()...)...); err != nil {
			return err
		}
	}
	for _, r := range del {
		run("", ipCmd, append([]string{r.family(), "rule"}, r.delArgs()...)...)
	}
	return nil
}

var nlSeq atomic.Uint32

// addRuleNetlink adds a port-53 rule (ruleMessage) and waits for the kernel's answer.
func addRuleNetlink(r ipRule) error {
	fail := func(err error) error {
		return fmt.Errorf("ip rule add pref %d uidrange %s（netlink）：%v", r.pref, r.uids, err)
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return fail(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fail(err)
	}
	tv := syscall.Timeval{Sec: 5}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return fail(err)
	}
	seq := nlSeq.Add(1)
	if err := syscall.Sendto(fd, ruleMessage(r, seq), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fail(err)
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return fail(err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fail(err)
		}
		for _, m := range msgs {
			if m.Header.Seq != seq || m.Header.Type != syscall.NLMSG_ERROR || len(m.Data) < 4 {
				continue
			}
			if code := int32(binary.LittleEndian.Uint32(m.Data)); code != 0 {
				return fail(syscall.Errno(-code))
			}
			return nil
		}
	}
}

func currentRules() ([]ipRule, error) {
	v4, err := run("", ipCmd, "-4", "rule", "show")
	if err != nil {
		return nil, err
	}
	v6, err := run("", ipCmd, "-6", "rule", "show")
	if err != nil {
		return nil, err
	}
	return append(parseRules(v4, false), parseRules(v6, true)...), nil
}

// inspectRules compares what is there with what these uids need: RulesOK, RulesPartial or RulesOff.
func inspectRules(uids []UIDRange) (string, error) {
	save, err := run("", saveCmd, "-t", "mangle")
	if err != nil {
		return "", err
	}
	m := parseMangle(save)
	have, err := currentRules()
	if err != nil {
		return "", err
	}
	route, err := run("", ipCmd, "-4", "route", "show", "table", fmt.Sprint(Table))
	if err != nil && !strings.Contains(route, "does not exist") {
		return "", err
	}
	del, add := planRules(have, wantRules(uids))
	switch {
	case m.jumps == len(jumps) && m.rules == len(chainRules(uids)) && len(del) == 0 && len(add) == 0 && routeOK(route):
		return RulesOK, nil
	case m.jumps == 0 && m.rules == 0 && len(have) == 0 && !routeOK(route):
		return RulesOff, nil
	}
	return RulesPartial, nil
}

// teardownRules removes everything of ours; it is fine to call when nothing is there.
func teardownRules() error {
	var errs []error
	for _, j := range jumps {
		for n := 0; n < 16 && iptables("-D", j.builtin, "-j", j.chain) == nil; n++ {
		}
	}
	for _, c := range []string{chainOut, chainApp, chainPre} {
		iptables("-F", c)
	}
	for _, c := range []string{chainOut, chainApp, chainPre} {
		if err := iptables("-X", c); err != nil && !strings.Contains(err.Error(), "No chain") {
			errs = append(errs, err)
		}
	}
	if have, err := currentRules(); err != nil {
		errs = append(errs, err)
	} else {
		for _, r := range have {
			if _, err := run("", ipCmd, append([]string{r.family(), "rule"}, r.delArgs()...)...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	run("", ipCmd, "-4", "route", "flush", "table", fmt.Sprint(Table))
	return errors.Join(errs...)
}
