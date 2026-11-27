package report

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

type lateCancelHost struct {
	*fakeHost
	cancel context.CancelFunc
}

func (h lateCancelHost) AppEvents(ctx context.Context) ([]AppEvent, error) {
	h.cancel()
	return h.fakeHost.AppEvents(ctx)
}

func TestCollectCanceledKeepsStage(t *testing.T) {
	f := newFixture(t, "full", testTempDir(t), "")
	setLimits(t, f.sc)
	guardDials(t, f.adbPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage, out := filepath.Join(f.base, "stage"), filepath.Join(f.base, "out")
	res, err := collect(ctx, f.root, lateCancelHost{f.host, cancel}, CollectOptions{OutDir: out, StageDir: stage, KeepStage: true, NoExplorer: true, now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321})
	if res != nil || errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("%+v %v", res, err)
	}
	dirs, _ := os.ReadDir(stage)
	if len(dirs) != 1 {
		t.Fatalf("stage %v", dirs)
	}
	kept := filepath.Join(stage, dirs[0].Name())
	if !strings.Contains(errs.As(err).Advice, kept) || !exists(filepath.Join(kept, "mas-avd.json")) {
		t.Fatalf("missing collected files: %v", err)
	}
	if z, _ := filepath.Glob(filepath.Join(out, "*.zip")); len(z) != 0 {
		t.Fatalf("partial zip %v", z)
	}
}

func TestZipLocalWallClock(t *testing.T) {
	for _, offset := range []int{8 * 3600, -7 * 3600} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			saved := time.Local
			time.Local = time.FixedZone("test", offset)
			defer func() { time.Local = saved }()
			when := time.Date(2026, 1, 2, 3, 4, 6, 0, time.Local)
			stage := testutil.TempDir(t)
			p := filepath.Join(stage, "summary.txt")
			os.WriteFile(p, []byte("clock"), 0600)
			if err := os.Chtimes(p, when, when); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(testutil.TempDir(t), "zip")
			if _, err := zipStage(context.Background(), dst, stage); err != nil {
				t.Fatal(err)
			}
			z, err := zip.OpenReader(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			h := z.File[0]
			wantDate := uint16((2026-1980)<<9 | 1<<5 | 2)
			wantTime := uint16(3<<11 | 4<<5 | 3)
			if h.ModifiedDate != wantDate || h.ModifiedTime != wantTime {
				t.Fatalf("DOS time %04x %04x want %04x %04x", h.ModifiedDate, h.ModifiedTime, wantDate, wantTime)
			}
		})
	}
}

func TestCollectExpandsRootAndStage(t *testing.T) {
	f := newFixture(t, "shortname", testTempDir(t), "Long Root Directory")
	guardDials(t, f.adbPort)
	if err := os.MkdirAll(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.base, "Long Stage Directory")
	os.MkdirAll(stage, 0700)
	rootShort, stageShort := shortName(t, f.root), shortName(t, stage)
	if rootShort == "" || stageShort == "" {
		t.Skip("volume does not create 8.3 names")
	}
	res, err := collect(context.Background(), filepath.Join(f.base, rootShort), f.host, CollectOptions{OutDir: filepath.Join(f.base, "out"), StageDir: filepath.Join(f.base, stageShort), KeepStage: true, NoExplorer: true, now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stage, stage+string(filepath.Separator)) || !strings.Contains(res.Summary, "路径："+f.root+"\r\n") {
		t.Fatalf("stage %q, summary %s", res.Stage, res.Summary)
	}
}

func TestLogcatService(t *testing.T) {
	for _, features := range []string{"shell_v2", ""} {
		t.Run(features, func(t *testing.T) {
			f := newFixture(t, "full", testTempDir(t), "")
			guardDials(t, f.adbPort)
			s := f.adb.data.Serials["emulator-20010"]
			s.Features = features
			// Recorded AOSP adb commandline.cpp logcat() request, independent of the implementation's quoting helper.
			command := "export ANDROID_LOG_TAGS=''; exec logcat '-d' '-b' 'crash'"
			s.Shell = map[string]scenarioShell{command: {Out: "current adb\n"}}
			f.adb.data.Serials["emulator-20010"] = s
			res := adbLive(context.Background(), f.adbPort, "emulator-20010", time.Second, nil, "logcat", "-d", "-b", "crash")
			if string(res.Stdout) != "current adb\n" || res.ExitCode != 0 {
				t.Fatalf("%+v", res)
			}
			prefix := "shell:"
			if features != "" {
				prefix = "shell,v2:"
			}
			f.adb.mu.Lock()
			found := false
			for _, r := range f.adb.requests {
				if r == "emulator-20010 "+prefix+command {
					found = true
				}
			}
			f.adb.mu.Unlock()
			if !found {
				t.Fatal("wrong service")
			}
		})
	}
}

type diskProcessHost struct {
	*fakeHost
	ctx     context.Context
	started chan struct{}
	exited  chan error
}

func (h diskProcessHost) Drive(root string) (Drive, error) {
	// Exercise cancellation of an already-running disk query, regardless of scheduling.
	select {
	case <-h.started:
		return h.fakeHost.Drive(root)
	case <-h.ctx.Done():
		return Drive{}, h.ctx.Err()
	}
}

func (h diskProcessHost) DiskKind(ctx context.Context, letter string) (string, string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestDiskQueryChild$")
	cmd.Env = append(os.Environ(), "MODAVD_DISK_TEST_CHILD=1")
	win.HideConsoleCmd(cmd)
	if err = cmd.Start(); err != nil {
		return "", "", err
	}
	close(h.started)
	err = cmd.Wait()
	h.exited <- err
	return "", "", err
}
func TestDiskQueryChild(t *testing.T) {
	if os.Getenv("MODAVD_DISK_TEST_CHILD") != "1" {
		t.Skip("child")
	}
	time.Sleep(time.Minute)
}
func TestDriveFailureJoinsDiskProcess(t *testing.T) {
	f := newFixture(t, "bare", testTempDir(t), "")
	guardDials(t, f.adbPort)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := diskProcessHost{fakeHost: f.host, ctx: ctx, started: make(chan struct{}), exited: make(chan error, 1)}
	done := make(chan struct{})
	go func() { buildConfig(ctx, f.root, h); close(done) }()
	select {
	case <-h.started:
	case <-ctx.Done():
		t.Fatal("no disk query")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("buildConfig did not finish")
	}
	select {
	case err := <-h.exited:
		if err == nil {
			t.Fatal("disk child finished without cancellation")
		}
	default:
		t.Fatal("disk child outlived failed drive query")
	}
}
