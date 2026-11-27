package emu

import (
	"errors"
	"strings"
	"testing"

	"modavd/internal/host"
)

func TestStaleShmMessage(t *testing.T) {
	cases := []struct {
		holders []host.Process
		err     error
		want    string
	}{
		{[]host.Process{{PID: 41, Name: "maawoa.exe"}, {PID: 7, Name: "python.exe"}}, nil,
			"拿着它的程序：maawoa.exe（pid 41）、python.exe（pid 7）"},
		{nil, nil, "没认出是哪个程序，多半是 MAA 这类用 ModAVD 截图的工具"},
		{nil, errors.New("读不到系统句柄表"), "查不出是哪个程序（读不到系统句柄表）"},
	}
	for _, c := range cases {
		got := staleShmMessage(0, "SHM_videmulator20000", c.holders, c.err)
		if !strings.HasPrefix(got, "实例 0 的截图共享内存 SHM_videmulator20000 还被") || !strings.Contains(got, c.want) ||
			!strings.HasSuffix(got, "把这些程序退出后再开机就好") {
			t.Errorf("%v %v: %s", c.holders, c.err, got)
		}
	}
}
