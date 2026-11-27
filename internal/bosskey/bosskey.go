// Package bosskey is the boss key of an instance (ModAVD/ModAVD#101): a global hot key that hides or shows the
// windows of the instances using it. This package only knows the key itself: how it is written, what it means to
// RegisterHotKey, and each instance's default. The manager registers the keys (internal/ui), emu toggles the windows.
//
// A key is written "Ctrl+Alt+Shift+1": the modifiers in this order, then one key. It needs at least one of Ctrl, Alt
// and Shift; Windows keeps the Win combinations for itself, so they are refused. Stored in mas-avd.json as
// instances.<i>.bossKey; "" there means the instance has no boss key, and an absent value means Default(i).
package bosskey

import (
	"fmt"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

// MetaKey is the key of the setting in mas-avd.json (instances.<i>.bossKey).
const MetaKey = "bossKey"

// Modifier flags of RegisterHotKey.
const (
	ModAlt     = 0x1
	ModControl = 0x2
	ModShift   = 0x4
	// ModNoRepeat: holding the key down does not repeat the hot key (Windows 7 on); without it a long press would
	// toggle the windows back and forth.
	ModNoRepeat = 0x4000
)

// Key is a parsed boss key.
type Key struct {
	Ctrl, Alt, Shift bool
	// Name is the key without modifiers as written: "1", "A", "F5", "Num0", "PageUp".
	Name string
	// VK is its Windows virtual-key code.
	VK uint32
}

// String is the canonical form, "Ctrl+Alt+Shift+1".
func (k Key) String() string {
	var b strings.Builder
	for _, m := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "Ctrl"}, {k.Alt, "Alt"}, {k.Shift, "Shift"}} {
		if m.on {
			b.WriteString(m.name + "+")
		}
	}
	b.WriteString(k.Name)
	return b.String()
}

// Modifiers is the fsModifiers of RegisterHotKey, with ModNoRepeat.
func (k Key) Modifiers() uint32 {
	m := uint32(ModNoRepeat)
	if k.Ctrl {
		m |= ModControl
	}
	if k.Alt {
		m |= ModAlt
	}
	if k.Shift {
		m |= ModShift
	}
	return m
}

// named are the keys besides digits, letters, F1–F24 and Num0–Num9, with their virtual-key codes.
var named = map[string]uint32{
	"Space": 0x20, "PageUp": 0x21, "PageDown": 0x22, "End": 0x23, "Home": 0x24,
	"Left": 0x25, "Up": 0x26, "Right": 0x27, "Down": 0x28, "Insert": 0x2D, "Delete": 0x2E,
}

// keyName is the canonical name and virtual-key code of a key written without modifiers ("1", "a", "f5", "num0",
// "pageup"); ok=false when it is not a key a boss key can use.
func keyName(s string) (string, uint32, bool) {
	if len(s) == 1 {
		c := s[0]
		switch {
		case c >= '0' && c <= '9':
			return s, uint32(c), true
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
			fallthrough
		case c >= 'A' && c <= 'Z':
			return string(c), uint32(c), true
		}
		return "", 0, false
	}
	upper := strings.ToUpper(s)
	if n, ok := strings.CutPrefix(upper, "F"); ok {
		if v, err := strconv.Atoi(n); err == nil && v >= 1 && v <= 24 && strconv.Itoa(v) == n {
			return "F" + n, 0x70 + uint32(v-1), true
		}
	}
	if n, ok := strings.CutPrefix(upper, "NUM"); ok && len(n) == 1 && n[0] >= '0' && n[0] <= '9' {
		return "Num" + n, 0x60 + uint32(n[0]-'0'), true
	}
	for name, vk := range named {
		if strings.EqualFold(name, s) {
			return name, vk, true
		}
	}
	return "", 0, false
}

// Parse reads a key such as "Ctrl+Alt+Shift+1" (case and spaces around the + do not matter; Control is Ctrl).
func Parse(s string) (Key, error) {
	var k Key
	parts := strings.Split(s, "+")
	if strings.TrimSpace(s) == "" {
		return k, errs.New(errs.InvalidArgument, "老板键是空的")
	}
	bad := func(format string, a ...any) (Key, error) {
		return Key{}, errs.Newf(errs.InvalidArgument, "老板键 %q 不对：%s", s, fmt.Sprintf(format, a...)).
			WithAdvice("写成 Ctrl+Alt+Shift+1 这样：Ctrl、Alt、Shift 至少带一个，最后是一个数字、字母、F1–F24、Num0–Num9 等按键；不要老板键写 none")
	}
	for n, p := range parts {
		p = strings.TrimSpace(p)
		last := n == len(parts)-1
		switch strings.ToLower(p) {
		case "ctrl", "control":
			if k.Ctrl {
				return bad("Ctrl 写了两次")
			}
			k.Ctrl = true
			continue
		case "alt":
			if k.Alt {
				return bad("Alt 写了两次")
			}
			k.Alt = true
			continue
		case "shift":
			if k.Shift {
				return bad("Shift 写了两次")
			}
			k.Shift = true
			continue
		case "win", "windows", "meta", "super", "cmd", "command":
			return bad("Win 的组合 Windows 自己留着用，不能当老板键")
		case "":
			return bad("有空的一段")
		}
		if !last {
			return bad("%q 不是 Ctrl、Alt、Shift，只能放在最后", p)
		}
		name, vk, ok := keyName(p)
		if !ok {
			return bad("不认识按键 %q", p)
		}
		k.Name, k.VK = name, vk
	}
	if k.Name == "" {
		return bad("缺少 Ctrl、Alt、Shift 以外的那个按键")
	}
	if !k.Ctrl && !k.Alt && !k.Shift {
		return bad("至少要带 Ctrl、Alt、Shift 中的一个")
	}
	return k, nil
}

// Normalize is the stored form of what a user gave: "" for no boss key ("", "none"), else Parse's canonical form.
func Normalize(s string) (string, error) {
	if t := strings.TrimSpace(s); t == "" || strings.EqualFold(t, "none") {
		return "", nil
	}
	k, err := Parse(s)
	if err != nil {
		return "", err
	}
	return k.String(), nil
}

// Default is the boss key of instance i when it never set one: Ctrl+Alt+Shift+<i> with the digits of the main
// keyboard for 0–9 (why this combination: ModAVD/ModAVD#101), none above 9.
func Default(i int) string {
	if i < 0 || i > 9 {
		return ""
	}
	return "Ctrl+Alt+Shift+" + strconv.Itoa(i)
}

// Of is the boss key of instance i from its instances.<i> (meta may be nil): bossKey when it is set, else
// Default(i). "" = no boss key. A value that is not a string or not a valid key counts as none (err says why), so
// a broken hand edit never takes a key nobody asked for.
func Of(meta *jsonord.Obj, i int) (string, error) {
	if meta == nil || !meta.Has(MetaKey) {
		return Default(i), nil
	}
	v := meta.Get(MetaKey)
	s, ok := v.Str()
	if !ok || v.Kind() != jsonord.String {
		return "", errs.Newf(errs.InvalidArgument, "实例 %d 的 %s 不是字符串，当作不要老板键", i, MetaKey)
	}
	key, err := Normalize(s)
	if err != nil {
		return "", errs.Newf(errs.InvalidArgument, "实例 %d 的老板键 %q 不对，当作不要老板键", i, s)
	}
	return key, nil
}
