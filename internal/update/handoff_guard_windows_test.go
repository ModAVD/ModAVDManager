// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

func TestHandoffGuardChild(t *testing.T) {
	dir := os.Getenv("MODAVD_HANDOFF_GUARD_ROOT")
	if dir == "" {
		return
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := BeginHandoff(r, testHandoffID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	time.Sleep(time.Minute)
}

func TestHandoffGuardCrashReleasesKernelOwnership(t *testing.T) {
	r := isolatedHandoffRoot(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHandoffGuardChild$")
	host.HideConsoleCmd(cmd)
	cmd.Env = append(os.Environ(), "MODAVD_HANDOFF_GUARD_ROOT="+r.Dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for !HandoffReady(r, testHandoffID) {
		if time.Now().After(deadline) {
			t.Fatal("child did not take ownership")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if HandoffReady(r, testHandoffID) {
		t.Fatal("dead owner's marker stayed live")
	}
	release, updating, err := ManagerStart(r)
	if err != nil || updating {
		t.Fatalf("crash stranded root: %v %v", updating, err)
	}
	release()
	if _, err := os.Stat(ownerPath(r)); !os.IsNotExist(err) {
		t.Fatal("crashed owner was not cleared")
	}
}

func isolatedHandoffRoot(t *testing.T) *root.Root {
	t.Helper()
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.MetadataPath(), []byte(`{"portBase":34000,"adbServerPort":34098,"scriptAdbServerPort":34099,"shareDefaultAdbServer":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHandoffGuardOwnsLaunchUntilCleanup(t *testing.T) {
	r := isolatedHandoffRoot(t)
	g, err := BeginHandoff(r, testHandoffID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !HandoffReady(r, testHandoffID) || HandoffReady(r, HandoffID(strings.Repeat("a", 32))) {
		t.Fatal("readiness was not tied to its owner")
	}
	if release, updating, err := ManagerStart(r); err != nil || !updating || release != nil {
		t.Fatalf("manager admitted during handoff: %v %v", updating, err)
	}
	if _, err := BeginHandoff(r, HandoffID(strings.Repeat("b", 32))); errs.CodeOf(err) != errs.Busy {
		t.Fatalf("second handoff: %v", err)
	}
	ctx, cancel := g.WaitContext(context.Background())
	defer cancel()
	if err := CancelHandoff(r, HandoffID(strings.Repeat("a", 32))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("another ID canceled this handoff")
	case <-time.After(100 * time.Millisecond):
	}
	if err := CancelHandoff(r, testHandoffID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancellation was not observed")
	}
	g.Close()
	g.Close()
	if HandoffReady(r, testHandoffID) {
		t.Fatal("readiness outlived the lease")
	}
	for _, p := range []string{ownerPath(r), cancelPath(r, testHandoffID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("owned state left behind: %s: %v", p, err)
		}
	}
	release, updating, err := ManagerStart(r)
	if err != nil || updating {
		t.Fatalf("manager after cleanup: %v %v", updating, err)
	}
	release()
	// A previous Close cannot clear the next owner's state.
	next, err := BeginHandoff(r, HandoffID(strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	g.Close()
	if !HandoffReady(r, next.id) {
		t.Fatal("previous owner cleared retry")
	}
}

func TestHandoffStaleOwnerNeverBlocksLaunch(t *testing.T) {
	r := isolatedHandoffRoot(t)
	p, _ := host.FindProcess(uint32(os.Getpid()))
	for _, data := range []string{"{", `{"id":"` + string(testHandoffID) + `","pid":1}`, ownerJSON(t, handoffOwner{
		ID: testHandoffID, PID: p.PID, Created: p.Created + 1, Root: realOrSame(r.Dir)}), ownerJSON(t, handoffOwner{
		ID: testHandoffID, PID: p.PID, Created: p.Created, Root: realOrSame(r.Dir)})} {
		if err := os.MkdirAll(filepath.Dir(ownerPath(r)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ownerPath(r), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if HandoffReady(r, testHandoffID) {
			t.Fatal("an unlocked marker was considered live")
		}
		release, updating, err := ManagerStart(r)
		if err != nil || updating {
			t.Fatalf("stale marker vetoed launch: %v %v", updating, err)
		}
		release()
	}
}

func ownerJSON(t *testing.T, o handoffOwner) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHandoffReadinessChecksRootAndProcessCreation(t *testing.T) {
	r := isolatedHandoffRoot(t)
	g, err := BeginHandoff(r, testHandoffID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	o, ok := liveHandoffOwner(r)
	if !ok {
		t.Fatal("no live owner")
	}
	for _, edit := range []func(*handoffOwner){
		func(o *handoffOwner) { o.Created++ },
		func(o *handoffOwner) { o.Root = t.TempDir() },
		func(o *handoffOwner) { o.ID = "bad" },
	} {
		changed := o
		edit(&changed)
		if err := os.WriteFile(ownerPath(r), []byte(ownerJSON(t, changed)), 0o600); err != nil {
			t.Fatal(err)
		}
		if HandoffReady(r, testHandoffID) {
			t.Fatal("unverified owner acknowledged")
		}
	}
}

func TestConcurrentOrdinaryManagerStartWaitsForAdmission(t *testing.T) {
	r := isolatedHandoffRoot(t)
	release, updating, err := ManagerStart(r)
	if err != nil || updating {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		unlock, updating, err := ManagerStart(r)
		if unlock != nil {
			unlock()
		}
		if updating {
			err = errs.New(errs.Internal, "ordinary launch mistaken for update")
		}
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("simultaneous launch passed admission before lease was released")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary launch did not resume")
	}
}

func TestHandoffGuardUsesTheSameRootThroughJunction(t *testing.T) {
	r := isolatedHandoffRoot(t)
	alias := filepath.Join(t.TempDir(), "root-alias")
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, r.Dir)
	host.HideConsoleCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test junction: %v: %s", err, out)
	}
	defer os.Remove(alias)
	other, err := root.New(alias)
	if err != nil {
		t.Fatal(err)
	}
	g, err := BeginHandoff(r, testHandoffID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !HandoffReady(other, testHandoffID) {
		t.Fatal("junction lost the handoff's root identity")
	}
	if release, updating, err := ManagerStart(other); err != nil || !updating || release != nil {
		t.Fatalf("junction admitted a second manager: %v %v", updating, err)
	}
	if _, err := BeginHandoff(other, HandoffID(strings.Repeat("b", 32))); errs.CodeOf(err) != errs.Busy {
		t.Fatalf("junction admitted a competing handoff: %v", err)
	}
	g.Close()
	release, updating, err := ManagerStart(other)
	if err != nil || updating {
		t.Fatalf("junction remained blocked: %v %v", updating, err)
	}
	release()
}
