package emu

import (
	"errors"
	"strings"
	"testing"

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

func TestPlanAutoGrow(t *testing.T) {
	low := dataSpaceRecord{FreeBytes: 2 * gb, TotalBytes: 47 * gb, SizeGB: 48}
	enough := func(free uint64) func() (uint64, error) { return func() (uint64, error) { return free, nil } }
	need := uint64(autoGrowStepGB)<<30 + autoGrowHostReserve
	cases := []struct {
		name      string
		on        bool
		space     dataSpaceRecord
		spaceOK   bool
		current   int
		currentOK bool
		host      func() (uint64, error)
		want      growPlan
	}{
		{"low, room on the host", true, low, true, 48, true, enough(500 << 30),
			growPlan{State: dataStateGrown, From: 48, To: 64, HostFree: 500 << 30, HostNeed: need}},
		{"host exactly enough", true, low, true, 48, true, enough(need),
			growPlan{State: dataStateGrown, From: 48, To: 64, HostFree: int64(need), HostNeed: need}},
		{"host one byte short", true, low, true, 48, true, enough(need - 1),
			growPlan{State: dataStateHostLow, From: 48, To: 64, HostFree: int64(need - 1), HostNeed: need}},
		{"host unreadable", true, low, true, 48, true, func() (uint64, error) { return 0, errors.New("x") },
			growPlan{State: dataStateHostLow, From: 48, To: 64, HostFree: -1, HostNeed: need}},
		{"not low", true, dataSpaceRecord{FreeBytes: 20 * gb, TotalBytes: 47 * gb, SizeGB: 48}, true, 48, true, nil, growPlan{}},
		{"no reading", true, dataSpaceRecord{}, false, 48, true, nil, growPlan{}},
		{"reading of another size (raised since)", true, low, true, 64, true, nil, growPlan{}},
		{"reading before the disk reached its size", true, dataSpaceRecord{FreeBytes: gb, TotalBytes: 16 * gb}, true, 32, true, nil, growPlan{}},
		{"size unknown", true, low, true, 0, false, nil, growPlan{}},
		{"off", false, low, true, 48, true, nil, growPlan{State: dataStateLow, From: 48, HostFree: -1}},
		{"file system behind", true, dataSpaceRecord{FreeBytes: gb, TotalBytes: 30 * gb, SizeGB: 32, FSBehind: true}, true, 32, true, nil,
			growPlan{State: dataStateFSBehind, From: 32, HostFree: -1}},
		{"at the limit", true, dataSpaceRecord{FreeBytes: gb, TotalBytes: 250 * gb, SizeGB: 256}, true, 256, true, nil,
			growPlan{State: dataStateAtMax, From: 256, HostFree: -1}},
		{"above the limit (raised by hand)", true, dataSpaceRecord{FreeBytes: gb, TotalBytes: 300 * gb, SizeGB: 300}, true, 300, true, nil,
			growPlan{State: dataStateAtMax, From: 300, HostFree: -1}},
		{"last step stops at the limit", true, dataSpaceRecord{FreeBytes: gb, TotalBytes: 245 * gb, SizeGB: 250}, true, 250, true, enough(500 << 30),
			growPlan{State: dataStateGrown, From: 250, To: 256, HostFree: 500 << 30, HostNeed: 6<<30 + autoGrowHostReserve}},
	}
	for _, c := range cases {
		host := c.host
		if host == nil {
			host = func() (uint64, error) { t.Errorf("%s: host asked", c.name); return 0, nil }
		}
		if got := planAutoGrow(c.on, c.space, c.spaceOK, c.current, c.currentOK, host); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
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
	lowAt48 := `"dataSpace": {"freeBytes": 1932735283, "totalBytes": 50465865728, "sizeGb": 48, "fsBehind": false, "at": 1800000000}`
	cases := []struct {
		name, meta string
		current    int
		state      string
		inMessage  []string // in Message + Advice
	}{
		{"never read", `{}`, 48, "", nil},
		{"pending", `{` + lowAt48 + `}`, 48, dataStatePending, []string{"1.8 GB", "64 GB", "--auto-grow off"}},
		{"off", `{"dataAutoGrow": false, ` + lowAt48 + `}`, 48, dataStateLow, []string{"自动扩容已关", "modavd set 3 --data-gb 64", "--auto-grow on"}},
		{"grown, not read since", `{` + lowAt48 + `, "dataGrow": {"state": "grown", "fromGb": 48, "toGb": 64, "at": 1}}`, 64, dataStateGrown,
			[]string{"48 GB", "64 GB"}},
		{"grown and read again, fine", `{"dataSpace": {"freeBytes": 17179869184, "totalBytes": 67108864000, "sizeGb": 64, "at": 2},` +
			` "dataGrow": {"state": "grown", "fromGb": 48, "toGb": 64, "at": 1}}`, 64, dataStateGrown, nil},
		{"grown and still low: pending again", `{"dataSpace": {"freeBytes": 1073741824, "totalBytes": 67108864000, "sizeGb": 64, "at": 2},` +
			` "dataGrow": {"state": "grown", "fromGb": 48, "toGb": 64, "at": 1}}`, 64, dataStatePending, []string{"80 GB"}},
		{"host low", `{` + lowAt48 + `, "dataGrow": {"state": "host_low", "fromGb": 48, "toGb": 64, "hostFreeBytes": 10737418240, "at": 1}}`, 48,
			dataStateHostLow, []string{"10.0 GB", "36.0 GB", "modavd set 3 --data-gb 64"}},
		{"at max", `{"dataSpace": {"freeBytes": 1073741824, "totalBytes": 270000000000, "sizeGb": 256, "at": 2}}`, 256, dataStateAtMax,
			[]string{"256 GB", "modavd set 3 --data-gb 272"}},
		{"failed", `{` + lowAt48 + `, "dataGrow": {"state": "failed", "fromGb": 48, "toGb": 64, "error": "写不了", "at": 1}}`, 48,
			dataStateFailed, []string{"写不了", "modavd set 3 --data-gb 64"}},
		{"raised by hand since the reading", `{` + lowAt48 + `}`, 96, "", nil},
		{"old record of another size", `{"dataSpace": {"freeBytes": 17179869184, "totalBytes": 67108864000, "sizeGb": 64, "at": 2},` +
			` "dataGrow": {"state": "host_low", "fromGb": 48, "toGb": 64, "at": 1}}`, 64, "", nil},
	}
	for _, c := range cases {
		d := dataSpaceInfo(3, metaOf(t, c.meta), c.current)
		if d.State != c.state {
			t.Errorf("%s: state %q, want %q (%+v)", c.name, d.State, c.state, d)
			continue
		}
		if (d.State == "") != (d.Message == "") {
			t.Errorf("%s: message %q for state %q", c.name, d.Message, d.State)
		}
		for _, s := range c.inMessage {
			if !strings.Contains(d.Message+" "+d.Advice, s) {
				t.Errorf("%s: %q not in %q / %q", c.name, s, d.Message, d.Advice)
			}
		}
	}
	if d := dataSpaceInfo(0, nil, 64); !d.AutoGrow || d.State != "" || d.TotalBytes != 0 {
		t.Errorf("no metadata: %+v", d)
	}
}
