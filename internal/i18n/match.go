package i18n

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// segment is a piece of a format string: literal text, or a verb (arg is its 0-based argument).
type segment struct {
	lit  string
	verb string // "" for literal text
	arg  int
}

var verbRE = regexp.MustCompile(`^%(\[(\d+)\])?[+\-# 0]*(\d+|\*)?(\.(\d+|\*)?)?([a-zA-Z])`)

// parseFormat splits a Go format string into literal text and verbs ("%%" is literal). Arguments are numbered as fmt
// does, an explicit index %[n]v resetting the count.
func parseFormat(s string) []segment {
	var out []segment
	var lit strings.Builder
	next := 0
	for i := 0; i < len(s); {
		if s[i] != '%' {
			lit.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "%%") {
			lit.WriteByte('%')
			i += 2
			continue
		}
		m := verbRE.FindStringSubmatch(s[i:])
		if m == nil || m[3] == "*" || m[5] == "*" {
			lit.WriteByte('%')
			i++
			continue
		}
		if lit.Len() > 0 {
			out = append(out, segment{lit: lit.String()})
			lit.Reset()
		}
		arg := next
		if m[2] != "" {
			n, _ := strconv.Atoi(m[2])
			arg = n - 1
		}
		next = arg + 1
		out = append(out, segment{verb: m[6], arg: arg})
		i += len(m[0])
	}
	if lit.Len() > 0 {
		out = append(out, segment{lit: lit.String()})
	}
	return out
}

// hasVerb: the format string takes arguments.
func hasVerb(segs []segment) bool {
	for _, s := range segs {
		if s.verb != "" {
			return true
		}
	}
	return false
}

// pattern is a key with verbs, compiled to match a message made from it.
type pattern struct {
	re      *regexp.Regexp
	args    []int // argument index of each capture group
	litLen  int   // literal characters, for preferring the most specific key
	longest string
	out     []segment
}

type catalog struct {
	exact    map[string]string
	patterns []*pattern
	suffixes []string // exact keys in parentheses, glued after other text: "（可选）"
}

func newCatalog(entries map[string]string) *catalog {
	c := &catalog{exact: map[string]string{}}
	for k, v := range entries {
		// Messages are matched without their surrounding white space (kept from the message itself).
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		segs := parseFormat(k)
		if !hasVerb(segs) {
			k = unescapePercent(k)
			c.exact[k] = unescapePercent(v)
			if strings.HasPrefix(k, "（") && strings.HasSuffix(k, "）") {
				c.suffixes = append(c.suffixes, k)
			}
			continue
		}
		if p := compile(segs, parseFormat(v)); p != nil {
			c.patterns = append(c.patterns, p)
		}
	}
	sort.Slice(c.suffixes, func(a, b int) bool { return len(c.suffixes[a]) > len(c.suffixes[b]) })
	sort.SliceStable(c.patterns, func(a, b int) bool {
		if c.patterns[a].litLen != c.patterns[b].litLen {
			return c.patterns[a].litLen > c.patterns[b].litLen
		}
		return c.patterns[a].re.String() < c.patterns[b].re.String()
	})
	return c
}

// unescapePercent: a key without verbs is matched as the text it prints ("50%%" prints "50%").
func unescapePercent(s string) string {
	if !strings.Contains(s, "%%") {
		return s
	}
	var b strings.Builder
	for _, seg := range parseFormat(s) {
		b.WriteString(seg.lit)
	}
	return b.String()
}

func compile(segs, out []segment) *pattern {
	var re strings.Builder
	re.WriteString(`(?s)^`)
	p := &pattern{out: out}
	for _, s := range segs {
		if s.verb == "" {
			re.WriteString(regexp.QuoteMeta(s.lit))
			p.litLen += len([]rune(s.lit))
			if len(s.lit) > len(p.longest) {
				p.longest = s.lit
			}
			continue
		}
		switch s.verb {
		case "d":
			re.WriteString(`(-?\d+)`)
		default:
			re.WriteString(`(.+?)`)
		}
		p.args = append(p.args, s.arg)
	}
	re.WriteString(`$`)
	r, err := regexp.Compile(re.String())
	if err != nil {
		return nil
	}
	p.re = r
	return p
}

const maxDepth = 4

func (c *catalog) translate(s string, depth int) string {
	if !HasHan(s) {
		return s
	}
	core := strings.TrimSpace(s)
	lead := s[:strings.Index(s, core)]
	trail := s[len(lead)+len(core):]
	if v, ok := c.lookup(core, depth); ok {
		return lead + v + trail
	}
	if depth >= maxDepth {
		return s
	}
	if strings.Contains(core, "\n") {
		lines := strings.Split(core, "\n")
		for i, l := range lines {
			lines[i] = c.translate(l, depth+1)
		}
		return lead + strings.Join(lines, "\n") + trail
	}
	// A bullet or mark before the text ("✓ 目录路径", "- 说明").
	if m := markRE.FindString(core); m != "" && m != core {
		if v := c.translate(core[len(m):], depth+1); v != core[len(m):] {
			return lead + m + v + trail
		}
	}
	// A remark glued on: "轻量桌面 0.2.12（可选）".
	for _, suf := range c.suffixes {
		if head, ok := strings.CutSuffix(core, suf); ok && head != "" {
			return lead + strings.TrimRight(c.translate(head, depth+1), " ") + " " + c.exact[suf] + trail
		}
	}
	if v, ok := c.split(core, depth); ok {
		return lead + v + trail
	}
	// Last: words joined by spaces ("平台工具（adb） 37.0.1").
	// Usage lines: "[--timeout 秒] <命令...>", "host|preserve|语言" (brackets and marks around a word kept).
	if strings.ContainsAny(core, " |") {
		words := wordRE.FindAllString(core, -1)
		changed := false
		for i, w := range words {
			m := wordMarksRE.FindStringSubmatch(w)
			if m == nil || !HasHan(m[2]) {
				continue
			}
			if t := c.translate(m[2], depth+1); t != m[2] {
				words[i], changed = m[1]+t+m[3], true
			}
		}
		if changed {
			return lead + strings.Join(words, "") + trail
		}
	}
	return s
}

var (
	wordRE      = regexp.MustCompile(`[^ |]+|[ |]+`)
	wordMarksRE = regexp.MustCompile(`^([<\[(（「"'-]*)(.*?)([>\])）」"'.…:：,，]*)$`)
)

var markRE = regexp.MustCompile(`^[\p{S}\p{P}\s]+`)

// lookup is an exact key, then the most specific pattern that matches all of s.
func (c *catalog) lookup(s string, depth int) (string, bool) {
	if v, ok := c.exact[s]; ok {
		return v, true
	}
	for _, p := range c.patterns {
		if p.longest != "" && !strings.Contains(s, p.longest) {
			continue
		}
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		args := map[int]string{}
		for gi, a := range p.args {
			v := m[gi+1]
			if depth < maxDepth {
				v = c.translate(v, depth+1)
			}
			args[a] = v
		}
		var b strings.Builder
		for _, seg := range p.out {
			if seg.verb == "" {
				b.WriteString(seg.lit)
			} else {
				b.WriteString(args[seg.arg])
			}
		}
		return b.String(), true
	}
	return "", false
}

// separators a message is built of, with what they become in a translation.
var separators = []struct{ zh, out string }{
	{"；", "; "},
	{"。", ". "},
	{"：", ": "},
	{": ", ": "},
	{"，", ", "},
	{"、", ", "},
}

// split translates a message that no key covers by its parts: "外层：内层", "甲；乙", "说明（细节）".
func (c *catalog) split(s string, depth int) (string, bool) {
	for _, sep := range separators {
		if !strings.Contains(s, sep.zh) {
			continue
		}
		parts := strings.Split(s, sep.zh)
		changed := false
		for i, p := range parts {
			if p == "" {
				continue
			}
			t := c.translate(p, depth+1)
			if t != p {
				changed = true
			}
			parts[i] = t
		}
		if !changed {
			continue
		}
		out := strings.Join(parts, sep.out)
		return strings.TrimRight(out, " "), true
	}
	// "正文（说明）" with no key for the whole.
	if open := strings.LastIndex(s, "（"); open > 0 && strings.HasSuffix(s, "）") {
		head, inner := s[:open], s[open+len("（"):len(s)-len("）")]
		th, ti := c.translate(head, depth+1), c.translate(inner, depth+1)
		if th != head || ti != inner {
			return th + " (" + ti + ")", true
		}
	}
	return "", false
}
