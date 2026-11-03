package emu

import (
	"slices"
	"testing"

	"modavd/internal/win"
)

var (
	x3dBig   = win.L3Group{Size: 96 << 20, Mask: 0x0000FFFF}
	x3dSmall = win.L3Group{Size: 32 << 20, Mask: 0xFFFF0000}
	x3d      = []win.L3Group{x3dBig, x3dSmall} // 7950X3D: two groups of 16 logical processors
)

func pinned(mask uint64, cores int) otherQemu { return otherQemu{Mask: mask, Cores: cores} }

func TestHasBigL3(t *testing.T) {
	cases := map[string]struct {
		groups []win.L3Group
		want   bool
	}{
		"x3d":    {x3d, true},
		"equal":  {[]win.L3Group{{Size: 32 << 20, Mask: 0xFF}, {Size: 32 << 20, Mask: 0xFF00}}, false},
		"single": {[]win.L3Group{{Size: 36 << 20, Mask: 0xFFFFFF}}, false},
		"none":   {nil, false},
	}
	for name, c := range cases {
		if got := hasBigL3(c.groups); got != c.want {
			t.Errorf("%s: hasBigL3 = %v", name, got)
		}
	}
}

// Instances of 6 vCPUs starting one after another on a 7950X3D alternate between the groups; once 24 of the 32
// processors are taken (4 free in each group) a fifth is not pinned.
func TestPlanHostX3DSequence(t *testing.T) {
	var others []otherQemu
	wantMask := []uint64{x3dBig.Mask, x3dSmall.Mask, x3dBig.Mask, x3dSmall.Mask}
	wantSpare := []int{10, 10, 4, 4}
	for k := range wantMask {
		p := planHost(x3d, 32, others, 6)
		if p.Group < 0 || p.Mask != wantMask[k] || p.Spare != wantSpare[k] {
			t.Fatalf("instance %d: %+v, want mask 0x%x spare %d", k+1, p, wantMask[k], wantSpare[k])
		}
		others = append(others, pinned(p.Mask, 6))
	}
	p := planHost(x3d, 32, others, 6)
	if p.Group != -1 || !slices.Equal(p.Free, []int{4, 4}) || p.Spare != 32-30 {
		t.Errorf("both groups full: %+v", p)
	}
	// a sixth: not pinned either, and the host is over-committed
	if p := planHost(x3d, 32, append(others, otherQemu{Mask: 0xFFFFFFFF, Cores: 6}), 6); p.Group != -1 || p.Spare != 32-36 {
		t.Errorf("sixth: %+v", p)
	}
}

// What the instances of #34 met: production qemu pinned to 0-15 by AUTO-MAS.
func TestPlanHostProductionPinned(t *testing.T) {
	prod := []otherQemu{pinned(0xFFFF, 6), pinned(0xFFFF, 6)}
	if p := planHost(x3d, 32, prod, 6); p.Mask != x3dSmall.Mask || !slices.Equal(p.Free, []int{4, 16}) || p.Spare != 10 {
		t.Errorf("%+v", p)
	}
	// Unreadable affinity counts as the big L3, not spread: spread, 6 each and a tie, the new one would join them on
	// 0-15 (18 vCPUs on 16 processors).
	unread := []otherQemu{{Cores: 6}, {Cores: 6}}
	if p := planHost(x3d, 32, unread, 6); p.Mask != x3dSmall.Mask || p.Unread != 2 || !slices.Equal(p.Free, []int{4, 16}) {
		t.Errorf("unreadable: %+v", p)
	}
	// Also when the big group comes second in the list.
	rev := []win.L3Group{x3dSmall, x3dBig}
	if p := planHost(rev, 32, unread, 6); p.Mask != x3dSmall.Mask || !slices.Equal(p.Free, []int{16, 4}) {
		t.Errorf("unreadable, reversed list: %+v", p)
	}
}

func TestPlanHostSpreadAndTies(t *testing.T) {
	// Not pinned (all processors): 6 vCPUs spread 3 + 3; a tie goes to the big L3.
	if p := planHost(x3d, 32, []otherQemu{pinned(0xFFFFFFFF, 6)}, 6); p.Mask != x3dBig.Mask || !slices.Equal(p.Free, []int{13, 13}) {
		t.Errorf("unpinned other: %+v", p)
	}
	// Big L3 wins a tie whichever order the list has.
	if p := planHost([]win.L3Group{x3dSmall, x3dBig}, 32, nil, 6); p.Mask != x3dBig.Mask || p.Group != 1 {
		t.Errorf("reversed list: %+v", p)
	}
	// A mask over parts of both groups loads them by overlap: 8 processors in the big one, 4 in the small one.
	if p := planHost(x3d, 32, []otherQemu{pinned(0x000F0FF0, 12)}, 4); !slices.Equal(p.Free, []int{16 - 8, 16 - 4}) {
		t.Errorf("partial overlap: %+v", p)
	}
	// Each qemu counts its own vCPUs: 8 + 4 on the big one, 6 on the small one.
	if p := planHost(x3d, 32, []otherQemu{pinned(0xFFFF, 8), pinned(0xFFFF, 4), pinned(0xFFFF0000, 6)}, 4); p.Mask != x3dSmall.Mask || !slices.Equal(p.Free, []int{4, 10}) {
		t.Errorf("mixed cores: %+v", p)
	}
}

// Groups of different processor counts (Ryzen AI 300: 4 Zen5 cores / 8 threads with 16 MB L3, 8 Zen5c cores / 16
// threads with 8 MB): the first instance gets the big L3 as before the split by load, later ones go by free share.
func TestPlanHostUnequalGroups(t *testing.T) {
	zen5 := win.L3Group{Size: 16 << 20, Mask: 0x0000FF}
	zen5c := win.L3Group{Size: 8 << 20, Mask: 0xFFFF00}
	groups := []win.L3Group{zen5c, zen5}
	var others []otherQemu
	wantMask := []uint64{zen5.Mask, zen5c.Mask, zen5c.Mask}
	wantFree := [][]int{{16, 8}, {16, 2}, {10, 2}}
	wantSpare := []int{2, 10, 4}
	for k := range wantMask {
		p := planHost(groups, 24, others, 6)
		if p.Mask != wantMask[k] || !slices.Equal(p.Free, wantFree[k]) || p.Spare != wantSpare[k] {
			t.Fatalf("instance %d: %+v, want mask 0x%x free %v spare %d", k+1, p, wantMask[k], wantFree[k], wantSpare[k])
		}
		others = append(others, pinned(p.Mask, 6))
	}
	// a fourth fits in neither group (4 and 2 free): not pinned, the whole host is spoken for
	if p := planHost(groups, 24, others, 6); p.Group != -1 || !slices.Equal(p.Free, []int{4, 2}) || p.Spare != 0 {
		t.Errorf("fourth: %+v", p)
	}
}

// On groups of equal processor count (7950X3D) the free share orders like the free count: the choice is the one of
// "most free processors, the big L3 on a tie" for any mix of pinned, unpinned and unreadable qemu.
func TestPlanHostEqualGroupsUnchanged(t *testing.T) {
	old := func(free []int, cores int) int {
		best := -1
		for k := range x3d {
			if free[k] < cores {
				continue
			}
			if best < 0 || free[k] > free[best] || free[k] == free[best] && x3d[k].Size > x3d[best].Size {
				best = k
			}
		}
		return best
	}
	n := 0
	for big := 0; big <= 3; big++ {
		for small := 0; small <= 3; small++ {
			for spread := 0; spread <= 2; spread++ {
				for unread := 0; unread <= 1; unread++ {
					for _, cores := range []int{2, 4, 6, 8} {
						var others []otherQemu
						for range big {
							others = append(others, pinned(x3dBig.Mask, 4))
						}
						for range small {
							others = append(others, pinned(x3dSmall.Mask, 6))
						}
						for range spread {
							others = append(others, pinned(0xFFFFFFFF, 4))
						}
						for range unread {
							others = append(others, otherQemu{Cores: 6})
						}
						p := planHost(x3d, 32, others, cores)
						if want := old(p.Free, cores); p.Group != want {
							t.Fatalf("big %d small %d spread %d unread %d cores %d: group %d, want %d (free %v)",
								big, small, spread, unread, cores, p.Group, want, p.Free)
						}
						n++
					}
				}
			}
		}
	}
	t.Logf("%d cases", n)
}

func TestPlanHostFit(t *testing.T) {
	// 4 free in each group: 4 vCPUs fit (exactly), 5 do not.
	full := []otherQemu{pinned(0xFFFF, 4), pinned(0xFFFF, 4), pinned(0xFFFF, 4),
		pinned(0xFFFF0000, 4), pinned(0xFFFF0000, 4), pinned(0xFFFF0000, 4)}
	if p := planHost(x3d, 32, full, 4); p.Mask != x3dBig.Mask || p.Spare != 0 {
		t.Errorf("4 free, 4 needed: %+v", p)
	}
	if p := planHost(x3d, 32, full, 5); p.Group != -1 || p.Spare != 32-24-5 {
		t.Errorf("4 free, 5 needed: %+v", p)
	}
}

// Ordinary CPUs (one L3, or all L3 the same size) are never pinned, whatever runs; only Spare is computed, over the
// whole host.
func TestPlanHostOrdinary(t *testing.T) {
	cases := map[string][]win.L3Group{
		"single": {{Size: 32 << 20, Mask: 0xFFFF}},
		"equal":  {{Size: 32 << 20, Mask: 0xFF}, {Size: 32 << 20, Mask: 0xFF00}},
		"none":   nil,
	}
	others := []otherQemu{pinned(0xFF, 6), {Cores: 4}}
	for name, groups := range cases {
		p := planHost(groups, 16, others, 6)
		if p.Group != -1 || p.Mask != 0 || p.Free != nil || p.Spare != 16-16 {
			t.Errorf("%s: %+v", name, p)
		}
	}
}

func TestIdlepollCap(t *testing.T) {
	cases := []struct{ cores, spare, want int }{
		{6, 10, 6}, // alone in a group of 16: one loop per vCPU
		{6, 6, 6},
		{6, 4, 4},  // second in the group: what is left
		{6, 1, 1},  // one processor left
		{6, 0, 0},  // none left
		{6, -4, 0}, // over-committed
		{4, 26, 4}, // never more loops than vCPUs
	}
	for _, c := range cases {
		if got := idlepollCap(c.cores, c.spare); got != c.want {
			t.Errorf("idlepollCap(%d, %d) = %d, want %d", c.cores, c.spare, got, c.want)
		}
	}
}
func TestCPURanges(t *testing.T) {
	for mask, want := range map[uint64]string{0xFFFF: "0–15", 0xFFFF0000: "16–31", 0x5: "0,2", 0xFF00F: "0–3,12–19", 1: "0"} {
		if got := cpuRanges(mask); got != want {
			t.Errorf("0x%x: %q, want %q", mask, got, want)
		}
	}
}

// qemuCores without an instance of this root: "-cores N" on the command line, else the default.
func TestQemuCoresCommandLine(t *testing.T) {
	m := &Manager{}
	for _, c := range []struct {
		cmdline []string
		want    int
	}{
		{[]string{"qemu.exe", "-avd", "mas_3", "-cores", "4"}, 4},
		{[]string{"qemu.exe", "-avd", "mas_3"}, 6},
		{[]string{"qemu.exe", "-cores", "x"}, 6},
		{[]string{"qemu.exe", "-cores", "0"}, 6},
		{[]string{"qemu.exe", "-cores"}, 6},
	} {
		if got := m.qemuCores(win.Process{}, c.cmdline, nil, 6); got != c.want {
			t.Errorf("%v: %d, want %d", c.cmdline, got, c.want)
		}
	}
}
