//go:build windows

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
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
