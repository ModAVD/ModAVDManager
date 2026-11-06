//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

func TestStopCancelCreate(t *testing.T) {
	for _, args := range [][]string{
		{"stop", "3"}, {"stop", "3", "--force"}, {"stop", "3", "--cancel-create"},
		{"stop", "--all"}, {"stop", "--all", "--force"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness()
			h.fake.running = func(context.Context) ([]int, error) { return []int{3}, nil }
			cancel := slices.Contains(args, "--all") || slices.Contains(args, "--cancel-create")
			h.fake.stop = func(_ context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
				if opt.CancelCreate != cancel || opt.Force != slices.Contains(args, "--force") {
					t.Errorf("options %+v", opt)
				}
				if !opt.CancelCreate {
					return nil, errs.New(errs.Busy, "正在新建").WithAdvice("用 --cancel-create 打断")
				}
				return &emu.StopResult{Index: i, Method: "graceful", Synced: true, InterruptedCreate: true}, nil
			}
			for _, jsonMode := range []bool{false, true} {
				argv := append([]string{}, args...)
				if jsonMode {
					argv = append(argv, "--json")
				}
				r := h.run(t, nil, argv...)
				if !cancel {
					if r.code != 5 || !strings.Contains(r.stdout+r.stderr, "--cancel-create") {
						t.Fatalf("busy: %+v", r)
					}
				} else {
					want := "已打断新建初始化"
					if jsonMode {
						want = `"interrupted_create":true`
					}
					if r.code != 0 || !strings.Contains(r.stdout, want) {
						t.Fatalf("canceled: %+v", r)
					}
				}
			}
		})
	}
}

// Exercise runStop -> target discovery -> Manager.Stop -> the creator's cancellation
// handling with no emulator or SDK. This is the same creating state as the gap
// between bridge boots: the create lock is held but no qemu / start lock exists.
func TestStopAllCancelsCreateWithoutQemu(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run("force="+strconv.FormatBool(force), func(t *testing.T) {
			dir := testutil.TempDir(t)
			if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(`{"portBase":29300,"adbServerPort":29398,"scriptAdbServerPort":29399}`), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := root.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			m := emu.New(r, nil)
			called := false
			i := 3
			_, err = m.Create(context.Background(), emu.CreateRequest{Index: &i, NoBridge: true,
				Progress: func(ev emu.Progress) {
					if called || ev.Step != "create" {
						return
					}
					called = true
					var out, errb bytes.Buffer
					args := []string{"stop", "--all", "--json"}
					if force {
						args = append(args, "--force")
					}
					code := run(context.Background(), args, strings.NewReader(""), &out, &errb, deps{
						newManager: func(string, *logx.Logger) (manager, error) { return emu.New(r, nil), nil },
					})
					var env struct {
						OK   bool          `json:"ok"`
						Data StopAllResult `json:"data"`
					}
					if e := json.Unmarshal(out.Bytes(), &env); e != nil {
						t.Fatalf("stop output: %s: %v", out.String(), e)
					}
					if code != 0 || !env.OK || len(env.Data.Results) != 1 || len(env.Data.Failed) != 0 || env.Data.Failed == nil {
						t.Errorf("stop --all: exit %d, stdout %s, stderr %s", code, out.String(), errb.String())
						return
					}
					got := env.Data.Results[0]
					if got.Index != i || got.Method != "already_offline" || !got.InterruptedCreate || got.Synced || got.Warnings == nil {
						t.Errorf("stop result: %+v", got)
					}
				},
			})
			if !called || errs.CodeOf(err) != errs.Canceled {
				t.Fatalf("creator: callback %v, error %v", called, err)
			}
			for key, want := range map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: "create_failed", errs.QemuRunning: "false"} {
				if got := errs.DetailsOf(err)[key]; got != want {
					t.Errorf("create error details %s = %q, want %q", key, got, want)
				}
			}
			info, err := emu.New(r, nil).Info(context.Background(), i)
			if err != nil || info.Status != emu.StatusCreateFailed || info.Running {
				t.Fatalf("after cancellation: %+v %v", info, err)
			}
			cancelPath := filepath.Join(r.AvdHome(), ".mas_3.create.cancel")
			if _, err := os.Stat(cancelPath); !os.IsNotExist(err) {
				t.Fatalf("cancellation marker not removed: %v", err)
			}
		})
	}
}
