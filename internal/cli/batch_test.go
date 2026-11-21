//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// TestCreateCount: create --count N takes N free indexes up front and creates them one after another with the same
// options (the title numbered); a failure does not stop the next; the exit code is the first failure's (ModAVD/ModAVD#240).
func TestCreateCount(t *testing.T) {
	h := newHarness()
	h.fake.freeIndexes = func(n int) ([]int, error) { return []int{0, 3, 4}, nil }
	var got []emu.CreateRequest
	h.fake.create = func(req emu.CreateRequest) (*emu.InstanceInfo, error) {
		got = append(got, req)
		if req.Progress != nil {
			req.Progress(emu.Progress{Step: "create", Message: "后台无头开机", Done: 20, Total: 1000})
		}
		if *req.Index == 3 {
			return nil, errs.New(errs.LaunchFailed, "模拟器退出了").WithAdvice("看日志")
		}
		in := sampleInstance(*req.Index)
		in.Title = req.Title
		return &in, nil
	}
	r := h.run(t, nil, "create", "--count", "3", "--title", "挂机", "--memory", "4G", "--json")
	if r.code != 7 {
		t.Errorf("exit %d, want 7 (launch_failed): %s", r.code, r.stderr)
	}
	var env struct {
		OK   bool              `json:"ok"`
		Data CreateBatchResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || len(env.Data.Results) != 2 || env.Data.Results[0].Index != 0 || env.Data.Results[1].Index != 4 ||
		len(env.Data.Failed) != 1 || env.Data.Failed[0].Index != 3 || env.Data.Failed[0].Error.Code != errs.LaunchFailed ||
		env.Data.Failed[0].Error.Advice != "看日志" {
		t.Fatalf("data = %+v", env.Data)
	}
	if len(got) != 3 {
		t.Fatalf("creates = %d", len(got))
	}
	for k, req := range got {
		want := []int{0, 3, 4}[k]
		if req.Index == nil || *req.Index != want || req.Title != "挂机 "+itoa(k+1) || req.MemoryMB == nil || *req.MemoryMB != 4096 {
			t.Errorf("create %d: %+v", k, req)
		}
	}
	if !strings.Contains(r.stderr, "[2/3] 新建中 2%：后台无头开机") {
		t.Errorf("progress: %s", r.stderr)
	}
	if calls := h.fake.callList(); !slices.Equal(calls, []string{"free-indexes 3", "space", "create", "space", "create", "space", "create"}) {
		t.Errorf("calls = %q", calls)
	}

	// human output: the made ones, then the failure with its advice
	h.fake.calls = nil
	r = h.run(t, nil, "create", "--count", "3")
	if r.code != 7 || !strings.Contains(r.stdout, "新建了 2 台，1 台没建成") || !strings.Contains(r.stdout, "实例 3 没建成：模拟器退出了\n  建议：看日志") {
		t.Errorf("human: exit %d stdout %q", r.code, r.stdout)
	}
}

// TestCreateCountStopsOnLowDisk: when the disk is low before an instance, that one and the rest are not made.
func TestCreateCountStopsOnLowDisk(t *testing.T) {
	h := newHarness()
	spaceCalls := 0
	h.fake.space = func() error {
		spaceCalls++
		if spaceCalls == 2 {
			return errs.New(errs.PrecheckFailed, "实例所在盘 E:\\ 剩余 3.0 GB，低于开机所需的 8 GB").WithAdvice("清理空间")
		}
		return nil
	}
	r := h.run(t, nil, "create", "--count", "3", "--json")
	var env struct {
		Data CreateBatchResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if r.code != 4 || len(env.Data.Results) != 1 || len(env.Data.Failed) != 2 || env.Data.Failed[0].Index != 2 ||
		env.Data.Failed[1].Index != 3 || env.Data.Failed[1].Error.Code != errs.PrecheckFailed {
		t.Fatalf("exit %d data %+v", r.code, env.Data)
	}
	if n := strings.Count(strings.Join(h.fake.callList(), ","), "create"); n != 1 {
		t.Errorf("%d creates after the disk ran low", n)
	}
}

// TestCreateCountRefused: too few free indexes creates none; --count with --index / --resume or out of range is a
// usage error before the root.
func TestCreateCountRefused(t *testing.T) {
	h := newHarness()
	h.fake.freeIndexes = func(n int) ([]int, error) {
		return nil, errs.Newf(errs.Busy, "只有 %d 个空闲编号（端口也要空着），建不了 %d 台", 2, n)
	}
	if r := h.run(t, nil, "create", "--count", "4", "--json"); r.code != 5 || !strings.Contains(r.stdout, "建不了 4 台") {
		t.Errorf("few indexes: exit %d %s", r.code, r.stdout)
	}
	if calls := h.fake.callList(); !slices.Equal(calls, []string{"free-indexes 4"}) {
		t.Errorf("calls = %q", calls)
	}
	for _, bad := range [][]string{{"create", "--count", "2", "--index", "3"}, {"create", "--count", "2", "--resume", "3"},
		{"create", "--count", "0"}, {"create", "--count", "10"}} {
		h := newHarness()
		h.rootErr = errs.New(errs.NotFound, "没有根目录")
		if r := h.run(t, nil, bad...); r.code != 2 || h.newCalls != 0 {
			t.Errorf("%v: exit %d, managers %d (%s)", bad, r.code, h.newCalls, r.stderr)
		}
	}
	// --count 1 is a plain create (its result is the instance)
	h = newHarness()
	if r := h.run(t, nil, "create", "--count", "1", "--index", "6", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"data":{"index":1`) {
		t.Errorf("count 1: exit %d %s", r.code, r.stdout)
	}
}

// TestDeleteSeveral: delete takes several indexes (sorted, each once), deletes them one after another; one that is
// running is reported and the others go on. Without --yes the message names them and nothing is deleted.
func TestDeleteSeveral(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "delete", "4", "1", "4")
	if r.code != 2 || !strings.Contains(r.stderr, "要删除实例 1、4，会连同游戏数据一起删掉，确认请加 --yes") || h.newCalls != 0 {
		t.Errorf("no --yes: exit %d stderr %q", r.code, r.stderr)
	}
	h.fake.del = func(i int) (*emu.DeleteResult, error) {
		if i == 3 {
			return nil, errs.WithDetails(errs.Newf(errs.Busy, "ModAVD 实例 %d 未关闭，无法删除", i), map[string]string{errs.Reason: "running"})
		}
		return &emu.DeleteResult{Index: i, Name: root.AvdName(i), Path: `D:\r\avd\` + root.AvdName(i) + ".avd"}, nil
	}
	r = h.run(t, nil, "delete", "4", "3", "1", "4", "--yes", "--json")
	if r.code != 5 {
		t.Errorf("exit %d, want 5 (busy)", r.code)
	}
	var env struct {
		OK   bool              `json:"ok"`
		Data DeleteBatchResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || len(env.Data.Results) != 2 || env.Data.Results[0].Index != 1 || env.Data.Results[1].Index != 4 ||
		len(env.Data.Failed) != 1 || env.Data.Failed[0].Index != 3 || env.Data.Failed[0].Error.Code != errs.Busy {
		t.Fatalf("data = %+v", env.Data)
	}
	if calls := h.fake.callList(); !slices.Equal(calls, []string{"delete 1", "delete 3", "delete 4"}) {
		t.Errorf("calls = %q", calls)
	}
	h.fake.calls = nil
	r = h.run(t, nil, "delete", "1", "3", "--yes")
	if r.code != 5 || !strings.Contains(r.stdout, "删除了 1 台，1 台没删") || !strings.Contains(r.stdout, "已删除实例 1（mas_1）") ||
		!strings.Contains(r.stdout, "实例 3 没删：ModAVD 实例 3 未关闭，无法删除") {
		t.Errorf("human: exit %d stdout %q", r.code, r.stdout)
	}
	// a bad index among them deletes nothing
	h.fake.calls = nil
	if r := h.run(t, nil, "delete", "1", "abc", "--yes"); r.code != 2 || len(h.fake.callList()) != 0 {
		t.Errorf("bad index: exit %d calls %q", r.code, h.fake.callList())
	}
	// Ctrl+C before the run: nothing deleted, each reported canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.fake.calls = nil
	r = h.run(t, ctx, "delete", "1", "2", "--yes", "--json")
	if r.code != 8 || len(h.fake.callList()) != 0 || strings.Count(r.stdout, `"code":"canceled"`) != 2 {
		t.Errorf("canceled: exit %d calls %q stdout %s", r.code, h.fake.callList(), r.stdout)
	}
}
