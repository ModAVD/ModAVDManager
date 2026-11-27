package emu

import (
	"strconv"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/jsonord"
)

const gb = int64(1) << 30

func TestDataLow(t *testing.T) {
	cases := []struct {
		free, total int64
		want        bool
	}{
		{5 * gb, 16 * gb, false},    // exactly 5 GB on a small disk: not below either line
		{5*gb - 1, 16 * gb, true},   // under 5 GB
		{6*gb + 1, 64 * gb, true},   // 6 GB of 64 is under 10%
		{10 * gb, 100 * gb, false},  // exactly 10%
		{10*gb - 1, 100 * gb, true}, // just under 10%
		{30 * gb, 256 * gb, false},
		{25 * gb, 256 * gb, true}, // 25 of 256 is under 10%
		{0, 16 * gb, true},
	}
	for _, c := range cases {
		if got := dataLow(c.free, c.total); got != c.want {
			t.Errorf("dataLow(%d, %d) = %v, want %v", c.free, c.total, got, c.want)
		}
	}
}

func TestAutoDiskTarget(t *testing.T) {
	cases := []struct {
		name      string
		auto      bool
		current   int
		currentOK bool
		want      int
	}{
		{"old 64 GB instance", true, 64, true, avd.AutoDataPartitionGB},
		{"old 16 GB instance", true, 16, true, avd.AutoDataPartitionGB},
		{"one short", true, avd.AutoDataPartitionGB - 1, true, avd.AutoDataPartitionGB},
		{"at the automatic size", true, avd.AutoDataPartitionGB, true, 0},
		{"larger already", true, 512, true, 0},
		{"manual", false, 64, true, 0},
		{"size unreadable", true, 0, false, 0},
	}
	for _, c := range cases {
		if got := autoDiskTarget(c.auto, c.current, c.currentOK); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseDataSizeStatfs(t *testing.T) {
	out := "dev: /dev/block/dm-40\r\ndevice: 34359738368\r\nBlock count: 8388608\r\nBlock size: 4096\r\nstatfs: 4096 8211956 7966798\r\n"
	got, ok := parseDataSize(out)
	if !ok || got.FreeBytes != 7966798*4096 || got.TotalBytes != 8211956*4096 || got.FSSize != 8388608*4096 {
		t.Fatalf("got %+v %v", got, ok)
	}
	for _, bad := range []string{"statfs: \n", "statfs: 4096 x 1\n", "statfs: 4096 10 11\n", "statfs: 0 10 1\n", "statfs: 4096 10\n"} {
		got, ok := parseDataSize("dev: /dev/block/dm-40\ndevice: 34359738368\nBlock count: 8388608\nBlock size: 4096\n" + bad)
		if !ok || got.FreeBytes != 0 || got.TotalBytes != 0 {
			t.Errorf("%q: %+v %v", bad, got, ok)
		}
	}
}

func metaOf(t *testing.T, s string) *jsonord.Obj {
	t.Helper()
	v, err := jsonord.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v.Obj()
}

func TestDataSpaceInfo(t *testing.T) {
	// 1.8 GB free of a disk of size GB
	low := func(size int64) string {
		return `"dataSpace": {"freeBytes": 1932735283, "totalBytes": ` + strconv.FormatInt(size*gb, 10) + `, "sizeGb": ` +
			strconv.FormatInt(size, 10) + `, "at": 1800000000}`
	}
	fine256 := `"dataSpace": {"freeBytes": 214748364800, "totalBytes": 270000000000, "sizeGb": 256, "at": 1800000000}`
	cases := []struct {
		name, meta string
		current    int
		hostFree   int64
		state      string
		in         []string // in Message + Advice
		notIn      []string
	}{
		{"never read", `{}`, 256, 500 * gb, "", nil, nil},
		{"automatic, plenty", `{` + fine256 + `}`, 256, 500 * gb, "", nil, nil},
		{"automatic, host low", `{` + fine256 + `}`, 256, 19 * gb, dataStateHostLow, []string{"19.0 GB", "宿主盘"}, nil},
		{"automatic, host exactly at the line", `{` + fine256 + `}`, 256, hostLowBytes, "", nil, nil},
		{"automatic, host unknown", `{` + fine256 + `}`, 256, -1, "", nil, nil},
		{"manual, host low: not said", `{"dataDiskMode": "manual", "dataSpace": {"freeBytes": 32000000000, "totalBytes": 64000000000, "sizeGb": 64, "at": 1}}`,
			64, 10 * gb, "", nil, nil},
		{"manual, low", `{"dataDiskMode": "manual", ` + low(48) + `}`, 48, 500 * gb, dataStateLow,
			[]string{"1.8 GB", "--data-gb auto", "modavd set 3 --data-gb 72"}, nil},
		{"automatic at 256, low", `{"dataDiskMode": "auto", ` + low(256) + `}`, 256, 500 * gb, dataStateLow,
			[]string{"256 GB", "modavd set 3 --data-gb 384"}, []string{"--data-gb auto"}},
		{"old instance without a mode, low before its growth", `{` + low(64) + `}`, 64, 500 * gb, dataStateLow,
			[]string{"自动扩到 256 GB"}, nil},
		{"manual at 512, low", `{"dataDiskMode": "manual", ` + low(512) + `}`, 512, 500 * gb, dataStateLow, []string{"已是最大"}, nil},
		{"file system behind", `{"dataSpace": {"freeBytes": 1073741824, "totalBytes": 32212254720, "sizeGb": 256, "fsBehind": true, "at": 1}}`,
			256, 500 * gb, dataStateLow, []string{"新建一台实例"}, nil},
		{"low reading of another size (raised since)", `{"dataDiskMode": "manual", ` + low(48) + `}`, 96, 500 * gb, "", nil, nil},
		{"low wins over host low", `{` + low(256) + `}`, 256, gb, dataStateLow, nil, nil},
	}
	for _, c := range cases {
		d := dataSpaceInfo(3, metaOf(t, c.meta), c.current, c.hostFree)
		if d.State != c.state {
			t.Errorf("%s: state %q, want %q (%+v)", c.name, d.State, c.state, d)
			continue
		}
		if (d.State == "") != (d.Message == "") || d.HostFreeBytes != c.hostFree {
			t.Errorf("%s: %+v", c.name, d)
		}
		for _, s := range c.in {
			if !strings.Contains(d.Message+" "+d.Advice, s) {
				t.Errorf("%s: %q not in %q / %q", c.name, s, d.Message, d.Advice)
			}
		}
		for _, s := range c.notIn {
			if strings.Contains(d.Message+" "+d.Advice, s) {
				t.Errorf("%s: %q in %q / %q", c.name, s, d.Message, d.Advice)
			}
		}
	}
}
