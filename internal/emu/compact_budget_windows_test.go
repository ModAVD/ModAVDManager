//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/win"
)

const compactFakeEnv = "MODAVD_TEST_COMPACT_BUDGET"

type compactFakeRequest struct {
	Allocated                             int64
	Missing, Corrupt, Snapshot, NoBacking bool
	CorruptOutput, Mismatch               bool
	Hold, Fail                            string
	DelayMillis                           int
	OutputSize                            int64
}

// Keep the controlled helper separate from TestMain and the #60 lifetime helper.
func init() {
	if req := os.Getenv(compactFakeEnv); req != "" && strings.EqualFold(filepath.Base(os.Args[0]), "qemu-img.exe") {
		os.Exit(compactFakeChild(req))
	}
}

func compactFakeChild(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 90
	}
	var req compactFakeRequest
	if json.Unmarshal(b, &req) != nil || len(os.Args) < 2 {
		return 91
	}
	stage := os.Args[1]
	f, err := os.OpenFile(path+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 92
	}
	fmt.Fprintln(f, stage)
	f.Close()
	if stage == req.Hold {
		os.WriteFile(path+".ready", []byte(stage), 0600)
		time.Sleep(30 * time.Second) // parent must cancel/timeout, never wait this out
	}
	if req.DelayMillis > 0 {
		time.Sleep(time.Duration(req.DelayMillis) * time.Millisecond)
	}
	if stage == req.Fail {
		return 7
	}
	switch stage {
	case "info":
		r := map[string]any{"backing-filename": "userdata-qemu.img", "backing-filename-format": "qcow2"}
		if req.Snapshot {
			r["snapshots"] = []any{map[string]any{"id": "1"}}
		}
		if req.NoBacking {
			delete(r, "backing-filename")
		}
		json.NewEncoder(os.Stdout).Encode(r)
	case "check":
		r := map[string]any{"check-errors": 0}
		if !req.Missing {
			r["allocated-clusters"] = req.Allocated
		}
		if req.Corrupt || (req.CorruptOutput && filepath.Base(os.Args[len(os.Args)-1]) == userdataCompactTmp) {
			r["corruptions"] = 1
		}
		json.NewEncoder(os.Stdout).Encode(r)
	case "convert":
		if err := compactTestImage(os.Args[len(os.Args)-1], req.OutputSize); err != nil {
			return 93
		}
	case "compare":
		if req.Mismatch {
			return 1
		}
	}
	return 0
}

func compactTestImage(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	b := bytes.Repeat([]byte{0x6d}, 1<<20)
	// A 64 MiB virtual disk with 512-byte clusters. Only the controlled helper
	// reads these files; real-image tests use real qemu-img fixtures instead.
	binary.BigEndian.PutUint32(b[0:], 0x514649fb)
	binary.BigEndian.PutUint32(b[20:], 9)
	binary.BigEndian.PutUint64(b[24:], 64<<20)
	for remaining := size; remaining > 0; {
		n := min(remaining, int64(len(b)))
		if _, err = f.Write(b[:n]); err != nil {
			f.Close()
			return err
		}
		remaining -= n
	}
	return f.Close()
}

func compactFakeManager(t *testing.T, req compactFakeRequest) (*Manager, string, string) {
	t.Helper()
	m := New(managerTestRoot(t, 2), nil)
	copyTestExe(t, filepath.Join(m.Root.RuntimeSDK(), "emulator", "qemu-img.exe"))
	in, err := m.instance(2)
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(in.Dir, userdataOverlay)
	if err := compactTestImage(overlay, 32<<20); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.Root.Dir, "compact-request.json")
	b, _ := json.Marshal(req)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(compactFakeEnv, path)
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{} }
	t.Cleanup(func() { qemuScan = saved })
	return m, path, overlay
}

func compactCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCompactBudgetThresholdPipeline(t *testing.T) {
	for _, delta := range []int64{-512, 0, 512} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			m, path, overlay := compactFakeManager(t, compactFakeRequest{Allocated: ((32 << 20) - (16 << 20) - delta) / 512, OutputSize: 1 << 20})
			before, _ := os.ReadFile(overlay)
			res, err := m.DiskCompact(context.Background(), 2)
			if err != nil {
				t.Fatal(err)
			}
			wantSkip := delta < 0
			if res.Skipped != wantSkip || *res.EstimatedReclaimableBytes != (16<<20)+delta {
				t.Fatalf("%+v", res)
			}
			calls := compactCalls(t, path)
			if strings.Contains(calls, "convert") == wantSkip || !strings.Contains(calls, "check") {
				t.Fatalf("stages: %s", calls)
			}
			if wantSkip {
				after, _ := os.ReadFile(overlay)
				if !bytes.Equal(before, after) || res.OnDiskDeltaBytes != 0 || res.BeforeOnDiskBytes != res.AfterOnDiskBytes {
					t.Fatal("skip changed original")
				}
			}
		})
	}
}

func TestCompactBudgetMissingAndGrowth(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			m, path, _ := compactFakeManager(t, compactFakeRequest{Missing: missing, OutputSize: 33 << 20})
			res, err := m.DiskCompact(context.Background(), 2)
			if err != nil {
				t.Fatal(err)
			}
			if res.Skipped || res.OnDiskDeltaBytes != 1<<20 || res.FreedBytes != 0 || res.AfterOnDiskBytes != 33<<20 {
				t.Fatalf("%+v", res)
			}
			if (res.EstimatedReclaimableBytes == nil) != missing || !strings.Contains(compactCalls(t, path), "compare") {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestCompactBudgetValidationBeforeSkip(t *testing.T) {
	for _, req := range []compactFakeRequest{
		{Allocated: 64 << 20 / 512, Corrupt: true}, {Allocated: 64 << 20 / 512, Snapshot: true},
		{Allocated: 64 << 20 / 512, NoBacking: true}, {Allocated: 1 << 62}, {Fail: "check"},
	} {
		t.Run(fmt.Sprintf("%+v", req), func(t *testing.T) {
			m, path, overlay := compactFakeManager(t, req)
			before, _ := os.ReadFile(overlay)
			if res, err := m.DiskCompact(context.Background(), 2); err == nil || res != nil {
				t.Fatalf("accepted %+v %v", res, err)
			}
			after, _ := os.ReadFile(overlay)
			if !bytes.Equal(before, after) || strings.Contains(compactCalls(t, path), "convert") {
				t.Fatal("invalid input rewrote original")
			}
		})
	}
}

func TestCompactBudgetVerificationProtectsOriginal(t *testing.T) {
	for _, req := range []compactFakeRequest{
		{Fail: "convert"}, {Fail: "compare"}, {CorruptOutput: true}, {Mismatch: true},
	} {
		t.Run(fmt.Sprintf("%+v", req), func(t *testing.T) {
			req.OutputSize = 1 << 20
			m, _, overlay := compactFakeManager(t, req)
			before, err := os.ReadFile(overlay)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := m.DiskCompact(context.Background(), 2); err == nil || res != nil {
				t.Fatalf("accepted %+v %v", res, err)
			}
			after, err := os.ReadFile(overlay)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed compact changed original: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(overlay), userdataCompactTmp)); !os.IsNotExist(err) {
				t.Fatal("failed compact left scratch image")
			}
		})
	}
}

func compactAwaitStage(t *testing.T, path, stage string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path + ".ready")
		if string(b) == stage {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper never entered %s", stage)
}

func TestCompactBudgetCancelProtectsOriginal(t *testing.T) {
	for _, stage := range []string{"convert", "compare"} {
		t.Run(stage, func(t *testing.T) {
			m, path, overlay := compactFakeManager(t, compactFakeRequest{Hold: stage, OutputSize: 1 << 20})
			before, _ := os.ReadFile(overlay)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := m.DiskCompact(ctx, 2); done <- err }()
			compactAwaitStage(t, path, stage)
			cancel()
			select {
			case err := <-done:
				if errs.CodeOf(err) != errs.Canceled {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not stop helper")
			}
			after, _ := os.ReadFile(overlay)
			if !bytes.Equal(before, after) {
				t.Fatal("canceled compact changed original")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(overlay), userdataCompactTmp)); !os.IsNotExist(err) {
				t.Fatal("canceled compact left scratch image")
			}
			// A retry must be able to take all the locks again.
			unlock, err := m.lockInstanceForCompact(2)
			if err != nil {
				t.Fatal(err)
			}
			unlock()
		})
	}
}

func TestCompactBudgetControlledTimeout(t *testing.T) {
	for _, stage := range []string{"convert", "compare"} {
		t.Run(stage, func(t *testing.T) {
			m, path, _ := compactFakeManager(t, compactFakeRequest{Hold: stage})
			start := time.Now()
			_, _, _, err := m.runQemuImg(context.Background(), 3*time.Second, stage, "unused")
			if errs.CodeOf(err) != errs.Timeout || time.Since(start) > 5*time.Second {
				t.Fatalf("timeout: %v", err)
			}
			if !strings.Contains(err.Error(), "3 秒没跑完") {
				t.Fatalf("timeout budget is unreadable: %v", err)
			}
			if b, _ := os.ReadFile(path + ".ready"); string(b) != stage {
				t.Fatal("timed out before helper started")
			}
		})
	}
	for _, budget := range []time.Duration{0, 15 * time.Second} {
		t.Run(budget.String(), func(t *testing.T) {
			m, _, _ := compactFakeManager(t, compactFakeRequest{DelayMillis: 250})
			if _, _, code, err := m.runQemuImg(context.Background(), budget, "compare", "unused"); err != nil || code != 0 {
				t.Fatalf("slow operation: code=%d %v", code, err)
			}
		})
	}
	t.Run("unbounded still cancels", func(t *testing.T) {
		m, path, _ := compactFakeManager(t, compactFakeRequest{Hold: "compare"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, _, _, err := m.runQemuImg(ctx, 0, "compare", "unused"); done <- err }()
		compactAwaitStage(t, path, "compare")
		cancel()
		select {
		case err := <-done:
			if errs.CodeOf(err) != errs.Canceled {
				t.Fatalf("%v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("unknown-size operation ignored cancel")
		}
	})
}
