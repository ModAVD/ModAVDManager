//go:build windows

package cli

import (
	"flag"
	"io"
	"strconv"
	"strings"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// The flag package stores and type-checks the command flags; the splitting below is ours because flag stops at the
// first positional argument ("start 3 --window" must work) and its messages are English.

type flagKind int

const (
	kindBool flagKind = iota
	kindInt
	kindString
)

type flagSpec struct {
	name string
	kind flagKind
	arg  string // value placeholder in help, "" for bool flags
	help string
}

type globals struct {
	root    string
	json    bool
	quiet   bool
	verbose bool
	help    bool
}

// invocation is one parsed command line.
type invocation struct {
	globals
	cmd  *command
	pos  []string      // positional arguments
	rest []string      // passthrough commands: everything after <i>, verbatim
	fs   *flag.FlagSet // the command's flags (nil without a command)
}

func isFlag(a string) bool { return len(a) > 1 && a[0] == '-' && a != "--" }

// splitFlag: "--name=value" / "-name" -> name, value, whether "=" was there.
func splitFlag(a string) (name, value string, hasValue bool) {
	a = strings.TrimPrefix(a, "-")
	a = strings.TrimPrefix(a, "-")
	return strings.Cut(a, "=")
}

func isGlobalName(name string) bool {
	switch name {
	case "root", "json", "quiet", "verbose", "help", "h":
		return true
	}
	return false
}

// known: a flag this command line understands at this point (global or the command's own).
func (inv *invocation) known(a string) bool {
	if !isFlag(a) {
		return false
	}
	name, _, _ := splitFlag(a)
	return isGlobalName(name) || (inv.cmd != nil && inv.cmd.spec(name) != nil)
}

// parseArgs splits a command line. The returned invocation is never nil: on error it holds what was parsed so far,
// with --json taken from anywhere on the line, so an error found before the parser reached --json is still JSON.
func parseArgs(args []string) (*invocation, error) {
	inv, err := parseStrict(args)
	if err != nil {
		for _, a := range args {
			if a == "--" {
				break
			}
			if isFlag(a) {
				if name, v, has := splitFlag(a); name == "json" {
					b, perr := strconv.ParseBool(v)
					inv.json = !has || (perr == nil && b)
				}
			}
		}
	}
	return inv, err
}

func parseStrict(args []string) (*invocation, error) {
	inv := &invocation{}
	k := 0
	for k < len(args) && isFlag(args[k]) {
		ok, err := inv.global(args, &k)
		if err != nil {
			return inv, err
		}
		if !ok {
			return inv, errs.Newf(errs.Usage, "未知选项 %s（命令名前面只能放 --root / --json / --quiet / --verbose）", args[k]).
				WithAdvice(helpAdvice)
		}
	}
	if k >= len(args) {
		return inv, nil
	}
	name := args[k]
	k++
	inv.cmd = findCommand(name)
	if inv.cmd == nil {
		return inv, errs.Newf(errs.Usage, "未知命令 %s", name).WithAdvice(helpAdvice)
	}
	inv.fs = inv.cmd.newFlagSet()
	for k < len(args) {
		a := args[k]
		if inv.cmd.passthrough && len(inv.pos) > 0 && !inv.known(a) {
			if a == "--" {
				k++
			}
			inv.rest = append([]string{}, args[k:]...)
			return inv, nil
		}
		if a == "--" {
			tail := args[k+1:]
			if inv.cmd.passthrough {
				if len(tail) > 0 {
					inv.pos = append(inv.pos, tail[0])
					tail = tail[1:]
				}
				inv.rest = append([]string{}, tail...)
			} else {
				inv.pos = append(inv.pos, tail...)
			}
			return inv, nil
		}
		if !isFlag(a) {
			inv.pos = append(inv.pos, a)
			k++
			continue
		}
		ok, err := inv.global(args, &k)
		if err != nil {
			return inv, err
		}
		if ok {
			continue
		}
		if err := inv.commandFlag(args, &k); err != nil {
			return inv, err
		}
	}
	return inv, nil
}

// global consumes args[*k] (and its value) when it is a global flag.
func (inv *invocation) global(args []string, k *int) (bool, error) {
	name, value, has := splitFlag(args[*k])
	if !isGlobalName(name) {
		return false, nil
	}
	if name == "root" {
		if !has {
			if *k+1 >= len(args) {
				return true, errs.New(errs.Usage, "选项 --root 后面缺少根目录").WithAdvice(helpAdvice)
			}
			*k++
			value = args[*k]
		}
		// Same trimming as root.Resolve (`--root "D:\ModAVD\"` arrives as `D:\ModAVD"`): blank after it means no root.
		if strings.Trim(strings.TrimSpace(value), `"`) == "" {
			return true, errs.New(errs.Usage, "选项 --root 的根目录是空的").WithAdvice(helpAdvice)
		}
		inv.root = value
		*k++
		return true, nil
	}
	b := true
	if has {
		var err error
		if b, err = strconv.ParseBool(value); err != nil {
			return true, errs.Newf(errs.Usage, "选项 --%s 只能是 true 或 false，收到「%s」", name, value)
		}
	}
	switch name {
	case "json":
		inv.json = b
	case "quiet":
		inv.quiet = b
	case "verbose":
		inv.verbose = b
	default:
		inv.help = b
	}
	*k++
	return true, nil
}

// commandFlag consumes args[*k] (and its value) as one of the command's flags.
func (inv *invocation) commandFlag(args []string, k *int) error {
	name, value, has := splitFlag(args[*k])
	spec := inv.cmd.spec(name)
	if spec == nil {
		return errs.Newf(errs.Usage, "命令 %s 没有选项 %s", inv.cmd.name, args[*k]).WithAdvice(inv.cmd.usageAdvice())
	}
	switch spec.kind {
	case kindBool:
		if !has {
			value = "true"
		}
		if _, err := strconv.ParseBool(value); err != nil {
			return errs.Newf(errs.Usage, "选项 --%s 只能是 true 或 false，收到「%s」", name, value).
				WithAdvice(inv.cmd.usageAdvice())
		}
	default:
		if !has {
			if *k+1 >= len(args) {
				return errs.Newf(errs.Usage, "选项 --%s 后面缺少值", name).WithAdvice(inv.cmd.usageAdvice())
			}
			*k++
			value = args[*k]
		}
		if spec.kind == kindInt {
			// Decimal only: flag's own int parsing would read "060" as octal.
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return errs.Newf(errs.Usage, "选项 --%s 要给整数，收到「%s」", name, value).WithAdvice(inv.cmd.usageAdvice())
			}
			value = strconv.Itoa(n)
		}
	}
	if err := inv.fs.Set(name, value); err != nil {
		return errs.Newf(errs.Usage, "选项 --%s 的值不对：「%s」", name, value).WithAdvice(inv.cmd.usageAdvice())
	}
	*k++
	return nil
}

func (c *command) newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, f := range c.flags {
		switch f.kind {
		case kindBool:
			fs.Bool(f.name, false, f.help)
		case kindInt:
			fs.Int(f.name, 0, f.help)
		default:
			fs.String(f.name, "", f.help)
		}
	}
	return fs
}

func (c *command) spec(name string) *flagSpec {
	for k := range c.flags {
		if c.flags[k].name == name {
			return &c.flags[k]
		}
	}
	return nil
}

// isSet: the flag was given on the command line.
func (inv *invocation) isSet(name string) bool {
	found := false
	if inv.fs != nil {
		inv.fs.Visit(func(f *flag.Flag) {
			if f.Name == name {
				found = true
			}
		})
	}
	return found
}

func (inv *invocation) get(name string) any {
	if inv.fs == nil {
		return nil
	}
	f := inv.fs.Lookup(name)
	if f == nil {
		return nil
	}
	return f.Value.(flag.Getter).Get()
}

func (inv *invocation) boolOpt(name string) bool {
	b, _ := inv.get(name).(bool)
	return b
}

func (inv *invocation) intOpt(name string) int {
	n, _ := inv.get(name).(int)
	return n
}

func (inv *invocation) strOpt(name string) string {
	s, _ := inv.get(name).(string)
	return s
}

// checkArgs checks the number of positional arguments and the passthrough tail.
func (inv *invocation) checkArgs() error {
	c := inv.cmd
	n := len(inv.pos)
	if n < c.minPos {
		what := "参数"
		if n < len(c.posNames) {
			what = c.posNames[n]
		}
		return errs.New(errs.Usage, missing(what)).WithAdvice(c.usageAdvice())
	}
	if c.maxPos >= 0 && n > c.maxPos {
		return errs.Newf(errs.Usage, "多余的参数：%s", strings.Join(inv.pos[c.maxPos:], " ")).WithAdvice(c.usageAdvice())
	}
	if c.restName != "" && len(inv.rest) == 0 {
		return errs.New(errs.Usage, missing(c.restName)).WithAdvice(c.usageAdvice())
	}
	return nil
}

// missing is "缺少<what>", with the space Chinese text puts before a Latin word ("缺少 APK 路径").
func missing(what string) string {
	if what != "" && what[0] < 0x80 {
		return "缺少 " + what
	}
	return "缺少" + what
}

// parseIndex accepts "3" and "mas_3".
func parseIndex(s string) (int, error) {
	t := strings.TrimSpace(s)
	if len(t) >= len(root.AvdNamePrefix) && strings.EqualFold(t[:len(root.AvdNamePrefix)], root.AvdNamePrefix) {
		t = t[len(root.AvdNamePrefix):]
	}
	if t == "" || len(t) > 9 || strings.Trim(t, "0123456789") != "" {
		return 0, errs.Newf(errs.Usage, "实例编号要写成 3 或 mas_3，收到「%s」", s)
	}
	n, _ := strconv.Atoi(t)
	if !root.ValidIndex(n) {
		return 0, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", n)
	}
	return n, nil
}

// parseMemory is avd.ParseMemoryMB; a variable so tests do not depend on the avd package.
var parseMemory = avd.ParseMemoryMB

func memoryOpt(s string) (int, error) {
	mb, err := parseMemory(strings.TrimSpace(s))
	if err != nil {
		if errs.CodeOf(err) == errs.Internal {
			return 0, errs.Wrap(errs.InvalidArgument, err, "内存要写成 6144、6144M 或 6G，收到「"+s+"」")
		}
		return 0, err
	}
	return mb, nil
}
