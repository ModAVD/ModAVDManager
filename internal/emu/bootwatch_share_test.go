//go:build windows

package emu

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/host"
	"modavd/internal/jsonord"
)

func stubShareDefaultAdb(t *testing.T, fn func(console int) (bool, error)) {
	t.Helper()
	old := shareDefaultAdbServer
	shareDefaultAdbServer = func(_ context.Context, console int) (bool, error) { return fn(console) }
	t.Cleanup(func() { shareDefaultAdbServer = old })
}

// The boot watch registers the instance with the default adb server only once a start has finished on its qemu, not
// while a stop runs, and not when mas-avd.json turns it off (ModAVD/ModAVD#196). A failure is warned once until a
// look works again.
func TestBootWatchShareDefaultAdbOnce(t *testing.T) {
	m, logs := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	q := host.Process{PID: 4242, Created: 1_000_500}
	console := m.Root.Ports().Console(i)
	var calls atomic.Int32
	var fail atomic.Bool
	var got atomic.Int32
	stubShareDefaultAdb(t, func(c int) (bool, error) {
		calls.Add(1)
		got.Store(int32(c))
		if fail.Load() {
			return false, errors.New("boom")
		}
		return true, nil
	})
	ctx := context.Background()
	w := &bootWatch{m: m, i: i, q: q}

	if w.shareDefaultAdbOnce(ctx) {
		t.Fatal("asked without a boot record")
	}
	m.writeBootRecord(i, bootRecord{QemuPID: 4100, QemuCreated: q.Created, BootID: testBootA})
	if w.shareDefaultAdbOnce(ctx) {
		t.Fatal("asked with the record of another qemu")
	}
	m.writeBootRecord(i, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	if !w.shareDefaultAdbOnce(ctx) || calls.Load() != 1 || int(got.Load()) != console {
		t.Fatalf("asked=%d console=%d, want 1 call for console %d", calls.Load(), got.Load(), console)
	}

	l, ok, err := host.TryLock(m.lockPath(i, "stop"))
	if err != nil || !ok {
		t.Fatalf("stop lock: %v %v", ok, err)
	}
	if w.shareDefaultAdbOnce(ctx) {
		t.Error("asked while a stop runs")
	}
	l.Unlock()

	fail.Store(true)
	before := len(logs.Messages())
	for k := 0; k < 3; k++ {
		if !w.shareDefaultAdbOnce(ctx) {
			t.Fatal("a failing look should still ask")
		}
	}
	if warned := logs.Messages()[before:]; len(warned) != 1 || !strings.Contains(warned[0], "没能登记到默认的 adb server") {
		t.Fatalf("warnings after three failures: %q, want one", warned)
	}
	fail.Store(false)
	w.shareDefaultAdbOnce(ctx)
	fail.Store(true)
	w.shareDefaultAdbOnce(ctx)
	if n := len(logs.Messages()) - before; n != 2 {
		t.Fatalf("a failure after a success should warn again: %d warnings", n)
	}

	if err := m.Root.UpdateMetadata(func(o *jsonord.Obj) error {
		o.Set("shareDefaultAdbServer", jsonord.NewBool(false))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w.shareDefaultAdbOnce(ctx) {
		t.Error("asked although mas-avd.json turned it off")
	}
}

// The loop looks right away, again at every tick, and returns once ctx ends.
func TestBootWatchShareDefaultAdbLoop(t *testing.T) {
	m := markerTestManager(t)
	const i = 2
	q := host.Process{PID: 4242, Created: 1_000_500}
	m.writeBootRecord(i, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	looks := make(chan struct{}, 8)
	stubShareDefaultAdb(t, func(int) (bool, error) { looks <- struct{}{}; return false, nil })
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); (&bootWatch{m: m, i: i, q: q}).shareDefaultAdb(ctx, ticks) }()
	wait := func(what string) {
		select {
		case <-looks:
		case <-time.After(3 * time.Second):
			t.Fatalf("no look %s", what)
		}
	}
	wait("right away")
	ticks <- time.Now()
	wait("after a tick")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop did not return after cancel")
	}
}
