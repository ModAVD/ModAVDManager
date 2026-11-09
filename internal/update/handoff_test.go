package update

import (
	"os"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

func TestHandoffRoundTrip(t *testing.T) {
	r := &root.Root{Dir: t.TempDir()}
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.Local)
	if TakeHandoff(r, now) != nil {
		t.Fatal("a result without a file")
	}
	st := &Status{Updated: true, CurrentVersion: "v0.40.0", Message: "已更新到 0.40.0"}
	if err := SaveHandoff(r, NewHandoffResult(st, nil, now)); err != nil {
		t.Fatal(err)
	}
	got := TakeHandoff(r, now.Add(time.Minute))
	if got == nil || !got.OK || !got.Updated || got.Version != "v0.40.0" || got.Message != "已更新到 0.40.0" || got.Error != nil {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(handoffPath(r)); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}

	err := errs.New(errs.Busy, "现在不能换上").WithAdvice("先关实例")
	if err := SaveHandoff(r, NewHandoffResult(nil, err, now)); err != nil {
		t.Fatal(err)
	}
	got = TakeHandoff(r, now)
	if got == nil || got.OK || got.Updated || got.Message != "现在不能换上" || got.Error == nil ||
		got.Error.Code != errs.Busy || got.Error.Advice != "先关实例" {
		t.Fatalf("%+v", got)
	}
}

// Only the root's programs count (not the emulators the manager is about to stop), and never a shutdown helper.
func TestProgramsInUse(t *testing.T) {
	u := New(&root.Root{Dir: t.TempDir()}, nil, Options{})
	u.scan = func(programs bool) ([]processUse, error) {
		if !programs {
			t.Error("scanned without the programs")
		}
		return []processUse{
			{PID: 10, Name: "qemu-system-x86_64.exe"},
			{PID: 11, Name: "emulator.exe", ShutdownHelper: true},
			{PID: 12, Name: "modavd.exe", RootProgram: true, ShutdownHelper: true},
			{PID: 13, Name: "modavd.exe", RootProgram: true},
			{PID: 14, Name: "netsimd.exe", Unknown: true},
		}, nil
	}
	got, err := u.ProgramsInUse()
	if err != nil || len(got) != 1 || got[0] != "modavd.exe（pid 13）" {
		t.Fatalf("%q %v", got, err)
	}
}

// Too old (a manager started long after), from the future or unreadable: not shown, and removed all the same.
func TestHandoffStaleOrBroken(t *testing.T) {
	r := &root.Root{Dir: t.TempDir()}
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.Local)
	for _, at := range []time.Time{now.Add(-HandoffFresh - time.Second), now.Add(2 * time.Minute)} {
		if err := SaveHandoff(r, NewHandoffResult(&Status{}, nil, at)); err != nil {
			t.Fatal(err)
		}
		if got := TakeHandoff(r, now); got != nil {
			t.Errorf("at %v: %+v", at, got)
		}
		if _, err := os.Stat(handoffPath(r)); !os.IsNotExist(err) {
			t.Errorf("at %v: not removed", at)
		}
	}
	if err := SaveHandoff(r, NewHandoffResult(&Status{}, nil, now.Add(-HandoffFresh+time.Second))); err != nil {
		t.Fatal(err)
	}
	if TakeHandoff(r, now) == nil {
		t.Error("a result just inside HandoffFresh was dropped")
	}
	if err := os.WriteFile(handoffPath(r), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if TakeHandoff(r, now) != nil {
		t.Error("a broken file gave a result")
	}
	if _, err := os.Stat(handoffPath(r)); !os.IsNotExist(err) {
		t.Error("a broken file was not removed")
	}
}
