package host

import "testing"

func TestCoreClassesText(t *testing.T) {
	cases := []struct {
		name    string
		classes []CoreClass
		want    string
	}{
		{"unreadable", nil, "读不到"},
		{"uniform", []CoreClass{{Class: 0, Cores: 16, Logical: 32}}, "没有大小核（16 核 32 个逻辑处理器，效率等级都是 0）"},
		{"hybrid", []CoreClass{{Class: 0, Cores: 8, Logical: 8}, {Class: 1, Cores: 8, Logical: 16}},
			"有大小核（效率等级越高越快）：等级 0：8 核 8 个逻辑处理器；等级 1：8 核 16 个逻辑处理器"},
	}
	for _, c := range cases {
		if got := CoreClassesText(c.classes); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
