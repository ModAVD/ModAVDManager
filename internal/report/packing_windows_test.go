package report

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/testutil"
)

// Cancel once some ZIP data is observable on disk. No timer or changes to the packing implementation are needed.
type cancelOnZIPData struct {
	context.Context
	cancel   context.CancelFunc
	path     string
	minBytes int64
	atBytes  atomic.Int64
}

func (c *cancelOnZIPData) Err() error {
	if st, err := os.Stat(c.path); err == nil && st.Size() > c.minBytes && c.atBytes.CompareAndSwap(0, st.Size()) {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCollectCanceledDuringPacking(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprint(keep), func(t *testing.T) {
			f := newFixture(t, "full", testTempDir(t), "")
			setLimits(t, f.sc)
			guardDials(t, f.adbPort)
			payload := make([]byte, 256<<10)
			rand.New(rand.NewSource(17)).Read(payload)
			if err := os.WriteFile(filepath.Join(f.root, "logs", "fetch-components.log"), payload, 0600); err != nil {
				t.Fatal(err)
			}
			stage, out := filepath.Join(f.base, "stage"), filepath.Join(f.base, "out")
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &cancelOnZIPData{Context: base, cancel: cancel, minBytes: 4096, path: filepath.Join(out, collectZipStart+f.now.Format("20060102-150405")+".zip")}
			opened := false
			res, err := collect(ctx, f.root, f.host, CollectOptions{OutDir: out, StageDir: stage, KeepStage: keep,
				now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321, explorer: func(string) error { opened = true; return nil }})
			if ctx.atBytes.Load() <= 0 || ctx.atBytes.Load() >= int64(len(payload)/2) {
				t.Fatalf("packing was not interrupted between chunks: %d bytes", ctx.atBytes.Load())
			}
			if res != nil || errs.CodeOf(err) != errs.Canceled || opened {
				t.Fatalf("result=%+v err=%v explorer=%v", res, err, opened)
			}
			if _, err := os.Stat(ctx.path); !os.IsNotExist(err) {
				t.Fatalf("unfinished ZIP remains: %v", err)
			}
			dirs, readErr := os.ReadDir(stage)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if keep {
				if len(dirs) != 1 {
					t.Fatalf("stage %v", dirs)
				}
				kept := filepath.Join(stage, dirs[0].Name())
				if !strings.Contains(errs.As(err).Advice, kept) || !exists(filepath.Join(kept, "summary.txt")) {
					t.Fatalf("missing preserved report: %v", err)
				}
			} else if len(dirs) != 0 {
				t.Fatalf("stage remains: %v", dirs)
			}
		})
	}
}

func TestZipStageAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(testutil.TempDir(t), "existing.zip")
	if err := os.WriteFile(dst, []byte("previous report"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := zipStage(ctx, dst, testutil.TempDir(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled: %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "previous report" {
		t.Fatalf("touched a ZIP we did not create: %q %v", b, err)
	}
}

func TestZipStageCanceledAtCompletion(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnZIPData{Context: base, cancel: cancel, path: filepath.Join(testutil.TempDir(t), "report.zip")}
	// An empty ZIP writes its first bytes only during Close, after all entry reads have finished.
	names, err := zipStage(ctx, ctx.path, testutil.TempDir(t))
	if !errors.Is(err, context.Canceled) || names != nil || ctx.atBytes.Load() == 0 {
		t.Fatalf("close-time cancel ignored: %v %v %d", names, err, ctx.atBytes.Load())
	}
	if _, err := os.Stat(ctx.path); !os.IsNotExist(err) {
		t.Fatalf("canceled ZIP remains: %v", err)
	}
}

func TestCollectPackingFailureKeepsStage(t *testing.T) {
	f := newFixture(t, "bare", testTempDir(t), "")
	guardDials(t, f.adbPort)
	out := filepath.Join(f.base, "not-a-directory")
	if err := os.WriteFile(out, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.base, "stage")
	res, err := collect(context.Background(), f.root, f.host, CollectOptions{OutDir: out, StageDir: stage, NoExplorer: true,
		now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321})
	if res != nil || errs.CodeOf(err) != errs.Internal {
		t.Fatalf("%+v %v", res, err)
	}
	dirs, readErr := os.ReadDir(stage)
	if readErr != nil || len(dirs) != 1 {
		t.Fatalf("lost report on a packing error: %v %v", dirs, readErr)
	}
	if !strings.Contains(errs.As(err).Advice, filepath.Join(stage, dirs[0].Name())) {
		t.Fatal(err)
	}
}
