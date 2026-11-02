package emu

import (
	"testing"

	"modavd/internal/win"
)

var (
	x3dBig   = win.L3Group{Size: 96 << 20, Mask: 0x0000FFFF}
	x3dSmall = win.L3Group{Size: 32 << 20, Mask: 0xFFFF0000}
	x3d      = []win.L3Group{x3dBig, x3dSmall}
)

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

func TestChooseAffinityX3D(t *testing.T) {
	// 6-vCPU instances starting one after another on a 7950X3D (two groups of 16).
	var pinned []uint64
	want := []uint64{x3dBig.Mask, x3dSmall.Mask, x3dBig.Mask, x3dSmall.Mask}
	for k, w := range want {
		c := chooseAffinity(x3d, pinned, 6)
		if c.Group < 0 || c.Mask != w {
			t.Fatalf("instance %d: got group %d mask 0x%x free %v, want mask 0x%x", k+1, c.Group, c.Mask, c.Free, w)
		}
		pinned = append(pinned, c.Mask)
	}
	// 24 of 32 vCPUs placed (12 per group): 4 free in each group, 6 do not fit anywhere.
	if c := chooseAffinity(x3d, pinned, 6); c.Group != -1 || c.Free[0] != 4 || c.Free[1] != 4 {
		t.Errorf("both groups full: %+v", c)
	}
}

func TestChooseAffinityProduction(t *testing.T) {
	// Production instances pinned to 0-15 by AUTO-MAS: the new one goes to the other group.
	prod := []uint64{0xFFFF, 0xFFFF}
	if c := chooseAffinity(x3d, prod, 6); c.Mask != x3dSmall.Mask {
		t.Errorf("mask 0x%x free %v", c.Mask, c.Free)
	}
	// One that could not be read (0) or is not pinned (all cores) is spread over both groups: 6 vCPUs -> 3 each.
	for _, m := range []uint64{0, 0xFFFFFFFF} {
		c := chooseAffinity(x3d, []uint64{m}, 6)
		if c.Free[0] != 13 || c.Free[1] != 13 || c.Mask != x3dBig.Mask {
			t.Errorf("mask 0x%x: %+v", m, c)
		}
	}
	// Tie goes to the big L3 whichever order the list has.
	rev := []win.L3Group{x3dSmall, x3dBig}
	if c := chooseAffinity(rev, nil, 6); c.Mask != x3dBig.Mask {
		t.Errorf("reversed list: 0x%x", c.Mask)
	}
}

func TestChooseAffinityEdges(t *testing.T) {
	// Exactly fitting counts as room; one less does not.
	if c := chooseAffinity(x3d, []uint64{0xFFFF, 0xFFFF}, 4); c.Mask != x3dSmall.Mask {
		t.Errorf("fits exactly: %+v", c)
	}
	full := []uint64{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF0000, 0xFFFF0000, 0xFFFF0000} // 4 free in each group at 4 vCPUs
	if c := chooseAffinity(x3d, full, 4); c.Mask != x3dBig.Mask {
		t.Errorf("4 free, 4 needed: %+v", c)
	}
	if c := chooseAffinity(x3d, full, 5); c.Group != -1 {
		t.Errorf("4 free, 5 needed: %+v", c)
	}
	if c := chooseAffinity(x3d, []uint64{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, 17); c.Group != -1 {
		t.Errorf("17 vCPUs fit nowhere: %+v", c)
	}
	// A mask over both groups (not all of them) is spread by overlap: 8 cpus in the big group, 4 in the small.
	c := chooseAffinity(x3d, []uint64{0x0000_0FF0 | 0x000F_0000<<0}, 12)
	if c.Free[0] != 16-8 || c.Free[1] != 16-4 {
		t.Errorf("partial overlap: %+v", c)
	}
}

func TestCPURanges(t *testing.T) {
	for mask, want := range map[uint64]string{0xFFFF: "0–15", 0xFFFF0000: "16–31", 0x5: "0,2", 0xFF00F: "0–3,12–19", 1: "0"} {
		if got := cpuRanges(mask); got != want {
			t.Errorf("0x%x: %q, want %q", mask, got, want)
		}
	}
}
