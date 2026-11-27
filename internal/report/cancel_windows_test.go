package report

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modavd/internal/errs"
)

// cancelingHost is the scenario's host, whose antivirus query presses Ctrl+C (cancel) in the middle of the run.
type cancelingHost struct {
	*fakeHost
	cancel context.CancelFunc
}

func (h cancelingHost) Antivirus(ctx context.Context) ([]Antivirus, error) {
	h.cancel()
	return h.fakeHost.Antivirus(ctx)
}

// TestCollectCanceled: Ctrl+C in the middle of a collection is canceled, with the stage directory removed and no zip
// written.
func TestCollectCanceled(t *testing.T) {
	f := newFixture(t, "full", testTempDir(t), "")
	setLimits(t, f.sc)
	guardDials(t, f.adbPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, stage := filepath.Join(f.base, "out"), filepath.Join(f.base, "stage")
	res, err := collect(ctx, f.root, cancelingHost{f.host, cancel}, CollectOptions{OutDir: out, StageDir: stage,
		NoExplorer: true, now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321})
	if errs.CodeOf(err) != errs.Canceled || res != nil {
		t.Fatalf("result %+v, error %v; want canceled", res, err)
	}
	if ents, err := os.ReadDir(stage); err != nil || len(ents) != 0 {
		t.Fatalf("stage directory: %v, %v; want it empty", ents, err)
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Fatalf("written to the output folder: %v", ents)
	}
}
