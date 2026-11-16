//go:build windows

package emu

import (
	"context"
	"testing"
	"time"
)

// TestWaitBootEarly: the boot wait hands each poll where adb answers before the boot has completed to early, until
// early says done; a poll where adb does not answer does not call it; the boot completing ends the wait.
func TestWaitBootEarly(t *testing.T) {
	m, _ := opsTestManager(t)
	q, _ := startSleeper(t)
	bootAdbFakes(t, q, false)
	saved := waitBootProbe
	t.Cleanup(func() { waitBootProbe = saved })
	// polls: adb not up, up (not done), up (done), up, booted
	probes := []struct{ booted, answered bool }{{false, false}, {false, true}, {false, true}, {false, true}, {true, true}}
	n := 0
	waitBootProbe = func(_ *Manager, _ context.Context, _ int, _ time.Duration, _ bool) (bool, bool) {
		p := probes[min(n, len(probes)-1)]
		n++
		return p.booted, p.answered
	}
	var calls []int // the probe number each early call followed
	early := func() bool {
		calls = append(calls, n)
		return len(calls) == 2
	}
	got, err := m.waitBoot(context.Background(), 3, time.Minute, &q, nil, "", &progressor{}, nil, nil, early)
	if err != nil || got.PID != q.PID {
		t.Fatalf("waitBoot: %v %+v", err, got)
	}
	if len(calls) != 2 || calls[0] != 2 || calls[1] != 3 {
		t.Errorf("early after probes %v, want [2 3]", calls)
	}
	if n != 5 {
		t.Errorf("%d probes, want 5", n)
	}

	// booted at once: early never runs
	n, calls = len(probes)-1, nil
	if _, err := m.waitBoot(context.Background(), 3, time.Minute, &q, nil, "", &progressor{}, nil, nil, early); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("early ran on a booted guest: %v", calls)
	}
}
