package bosskey

import (
	"testing"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

func TestDefault(t *testing.T) {
	for i, want := range map[int]string{0: "Ctrl+Alt+Shift+0", 1: "Ctrl+Alt+Shift+1", 9: "Ctrl+Alt+Shift+9", 10: "", 12: "", -1: ""} {
		if got := Default(i); got != want {
			t.Errorf("Default(%d) = %q, want %q", i, got, want)
		}
	}
	k, err := Parse(Default(1))
	if err != nil || k.VK != '1' || k.Modifiers() != ModControl|ModAlt|ModShift|ModNoRepeat {
		t.Fatalf("Default(1) parses to %+v %v", k, err)
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in, want string
		mods     uint32
		vk       uint32
	}{
		{"Ctrl+Alt+Shift+1", "Ctrl+Alt+Shift+1", ModControl | ModAlt | ModShift, '1'},
		{" shift + alt + control + 7 ", "Ctrl+Alt+Shift+7", ModControl | ModAlt | ModShift, '7'},
		{"ctrl+a", "Ctrl+A", ModControl, 'A'},
		{"Alt+Z", "Alt+Z", ModAlt, 'Z'},
		{"Shift+F12", "Shift+F12", ModShift, 0x7B},
		{"ctrl+f1", "Ctrl+F1", ModControl, 0x70},
		{"Alt+F24", "Alt+F24", ModAlt, 0x87},
		{"Ctrl+Alt+num0", "Ctrl+Alt+Num0", ModControl | ModAlt, 0x60},
		{"Ctrl+Num9", "Ctrl+Num9", ModControl, 0x69},
		{"ctrl+pageup", "Ctrl+PageUp", ModControl, 0x21},
		{"Ctrl+Shift+space", "Ctrl+Shift+Space", ModControl | ModShift, 0x20},
		{"Alt+Delete", "Alt+Delete", ModAlt, 0x2E},
	} {
		k, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if k.String() != c.want || k.Modifiers() != c.mods|ModNoRepeat || k.VK != c.vk {
			t.Errorf("Parse(%q) = %q mods %#x vk %#x, want %q %#x %#x", c.in, k, k.Modifiers(), k.VK, c.want, c.mods|ModNoRepeat, c.vk)
		}
	}
	for _, in := range []string{
		"", "   ", "1", "A", "F5", // no modifier
		"Win+1", "Ctrl+Win+1", "Ctrl+Alt+Shift+Meta+1", // Win is Windows' own
		"Ctrl+Ctrl+1", "Alt+alt+1", "Shift+Shift+1", // twice
		"Ctrl+", "Ctrl+Alt", "Ctrl++1", "+1", // no key / empty part
		"Ctrl+1+2", "Ctrl+A+Alt", // the key must come last, and only one
		"Ctrl+F0", "Ctrl+F25", "Ctrl+F01", "Ctrl+Num10", "Ctrl+Tab", "Ctrl+é", "Ctrl+-", "Ctrl+Esc", // not a key we take
	} {
		if k, err := Parse(in); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("Parse(%q) = %+v, %v; want invalid_argument", in, k, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"": "", "none": "", " NONE ": "", "  ": "", "alt+shift+3": "Alt+Shift+3"} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Normalize("Win+1"); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("Normalize(Win+1): %v", err)
	}
}

func TestOf(t *testing.T) {
	v, err := jsonord.Parse([]byte(`{"instances":{
		"1":{"memoryMb":6144},
		"2":{"bossKey":""},
		"3":{"bossKey":"ctrl+alt+shift+9"},
		"4":{"bossKey":"Win+4"},
		"6":{"bossKey":7},
		"7":{"bossKey":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	inst := v.Obj().Get("instances").Obj()
	meta := func(i string) *jsonord.Obj { return inst.Get(i).Obj() }
	for _, c := range []struct {
		meta    *jsonord.Obj
		i       int
		want    string
		invalid bool
	}{
		{nil, 0, "Ctrl+Alt+Shift+0", false},       // no instances.<i>: default
		{meta("1"), 1, "Ctrl+Alt+Shift+1", false}, // not written: default, so nothing to migrate
		{meta("1"), 10, "", false},                // above 9: none by default
		{meta("2"), 2, "", false},                 // cleared
		{meta("3"), 3, "Ctrl+Alt+Shift+9", false}, // set, read canonical
		{meta("4"), 4, "", true},                  // invalid: none
		{meta("6"), 6, "", true},                  // not a string: none
		{meta("7"), 7, "", true},                  // null: none
	} {
		got, err := Of(c.meta, c.i)
		if got != c.want || (err != nil) != c.invalid {
			t.Errorf("Of(%d) = %q, %v; want %q (invalid %v)", c.i, got, err, c.want, c.invalid)
		}
	}
}
