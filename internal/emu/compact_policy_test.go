package emu

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestCompactBudgetContext(t *testing.T) {
	for _, budget := range []time.Duration{0, -1, time.Hour} {
		parent, stop := context.WithCancel(context.Background())
		ctx, cancel := qemuImgContext(parent, budget)
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline != (budget > 0) || (hasDeadline && time.Until(deadline) < 59*time.Minute) {
			t.Fatalf("budget=%s deadline=%s present=%v", budget, deadline, hasDeadline)
		}
		stop()
		if ctx.Err() != context.Canceled {
			t.Fatal("context lost parent cancellation")
		}
		cancel()
	}
}

func TestCompactTimeoutText(t *testing.T) {
	for _, c := range []struct {
		budget time.Duration
		want   string
	}{
		{200 * time.Millisecond, "200ms"},
		{3 * time.Second, "3 秒"},
		{30 * time.Minute, "30 分钟"},
		{time.Hour - time.Second, "59 分钟"},
		{time.Hour, "1 小时"},
		{compactTimeout(64<<30, 0, 0, 3, 30*time.Minute), "54 小时 41 分钟"},
		{compactTimeout(64<<30, 0, 0, 2, 20*time.Minute), "36 小时 29 分钟"},
		{time.Duration(math.MaxInt64), "2562047 小时 47 分钟"},
	} {
		if got := qemuImgTimeoutText(c.budget); got != c.want {
			t.Errorf("budget=%s got=%q want=%q", c.budget, got, c.want)
		}
	}
}

func TestCompactTimeout(t *testing.T) {
	for _, c := range []struct {
		name                             string
		virtual, allocated, host, passes int64
		floor, want                      time.Duration
	}{
		{"small convert", 64 << 20, 1 << 20, 1 << 20, 3, 30 * time.Minute, 30 * time.Minute},
		{"slow 128GiB", 128 << 30, 80 << 30, 90 << 30, 3, 30 * time.Minute, (3*128*1024 + 300) * time.Second},
		{"backing compare", 1 << 40, 64 << 10, 1 << 20, 2, 20 * time.Minute, (2*1024*1024 + 300) * time.Second},
		{"host larger", 1 << 20, 1 << 20, 1 << 30, 3, 30 * time.Minute, (3*1024 + 300) * time.Second},
		{"round up", 1<<30 + 1, 0, 0, 2, 20 * time.Minute, (2*1025 + 300) * time.Second},
		{"unknown", 0, 1 << 30, 2 << 30, 3, 30 * time.Minute, 0},
		{"all missing", 0, 0, 0, 2, 20 * time.Minute, 0},
		{"saturate", math.MaxInt64, math.MaxInt64, math.MaxInt64, 3, 30 * time.Minute, time.Duration(math.MaxInt64)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := compactTimeout(c.virtual, c.allocated, c.host, c.passes, c.floor); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}

func TestCompactBenefit(t *testing.T) {
	for _, before := range []int64{0, 64 << 20, 64 << 30, math.MaxInt64} {
		threshold := max(16<<20, before/100)
		if estimate, got := compactBenefit(before, nil); estimate != nil || got != threshold {
			t.Fatalf("unknown allocation: %v %d", estimate, got)
		}
		if before < threshold {
			continue
		}
		for _, gain := range []int64{0, threshold - 1, threshold, threshold + 1} {
			alloc := before - gain
			estimate, got := compactBenefit(before, &alloc)
			if estimate == nil || *estimate != gain || got != threshold {
				t.Fatalf("before=%d gain=%d estimate=%v threshold=%d", before, gain, estimate, got)
			}
		}
	}
	compressed := int64(128 << 20)
	estimate, _ := compactBenefit(16<<20, &compressed)
	if *estimate != 0 {
		t.Fatal("compressed live clusters must not predict a negative saving")
	}
}

func TestCompactSizeBoundaries(t *testing.T) {
	for _, c := range []struct {
		clusters, size int64
		ok             bool
	}{
		{0, 65536, true}, {math.MaxInt64 / 65536, 65536, true},
		{math.MaxInt64/65536 + 1, 65536, false}, {-1, 65536, false}, {1, 0, false},
	} {
		_, err := checkedCompactAllocation(c.clusters, c.size)
		if (err == nil) != c.ok {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	for _, n := range []int64{0, 128 << 30, math.MaxInt64} {
		got, err := compactRequiredSpace(n)
		if n == math.MaxInt64 {
			if err == nil {
				t.Fatal("space overflow accepted")
			}
		} else if err != nil || got != n+n/20+compactSpaceSlack {
			t.Fatalf("space=%d %v", got, err)
		}
	}
	limit := int64(math.MaxInt64 - compactSpaceSlack)
	last := limit / 21 * 20
	for last+last/20 <= limit {
		last++
	}
	last--
	if _, err := compactRequiredSpace(last); err != nil {
		t.Fatal("last representable reservation rejected", err)
	}
	if _, err := compactRequiredSpace(last + 1); err == nil {
		t.Fatal("first overflowing reservation accepted")
	}
	for _, in := range []string{`{}`, `{"check-errors":0}`, `{"total-clusters":1024}`} {
		c, err := parseQemuImgCheck([]byte(in))
		if err != nil || c.allocationKnown {
			t.Fatalf("missing information counted as empty: %+v %v", c, err)
		}
	}
	c, err := parseQemuImgCheck([]byte(`{"total-clusters":1024,"check-errors":0,"format":"qcow2"}`))
	if err != nil || !c.allocationKnown || *c.AllocatedClusters != 0 {
		t.Fatalf("qemu 2.12 empty: %+v %v", c, err)
	}
}

func TestCompactResultGrowth(t *testing.T) {
	for _, delta := range []int64{-1 << 20, 0, 1 << 20} {
		r := compactResult(2, "overlay", 2<<20, (2<<20)+delta, nil, 16<<20, false)
		if r.OnDiskDeltaBytes != delta || r.FreedBytes != max(0, -delta) {
			t.Fatalf("%+v", r)
		}
	}
}
