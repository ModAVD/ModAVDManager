//go:build windows

package emu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// opsTestManager is a Manager on a fresh root (opsTestTempDir). The process scan is replaced by one that finds
// nothing: a real instance mas_<i> of another root on this machine has the same name and ports and would otherwise
// count as this root's instance (and get adb commands). Nothing here runs adb; the private port is set to an unused
// one all the same.
func opsTestManager(t *testing.T) (*Manager, *logx.Collector) {
	t.Helper()
	dir := opsTestTempDir(t)
	meta := `{"adbServerPort": 20391, "scriptAdbServerPort": 20392}`
	if err := os.WriteFile(filepath.Join(dir, "mas-avd.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{} }
	t.Cleanup(func() { qemuScan = saved })
	c := &logx.Collector{}
	return New(r, logx.New(c.Sink())), c
}

// opsSkipIfNotImplemented: the packages below emu are written at the same time; skip until they are.
func opsSkipIfNotImplemented(t *testing.T, err error) {
	t.Helper()
	if errs.CodeOf(err) == errs.NotImplemented {
		t.Skipf("dependency not implemented yet: %v", err)
	}
}

func TestOpsInstanceLifecycle(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()

	created, err := m.Create(ctx, CreateRequest{Title: "测试实例"})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	if !root.ValidIndex(i) || created.Title != "测试实例" || created.Status != StatusOffline || created.Running || created.PID != 0 {
		t.Fatalf("created = %+v", created)
	}
	want := InstanceConfig{MemoryMB: 6144, CPU: 6, FPS: 60, DataPartitionGB: 64, Width: 1280, Height: 720, DPI: 240}
	if created.Config != want {
		t.Fatalf("config = %+v, want %+v", created.Config, want)
	}
	if created.Serial != root.Serial(i) || created.AdbAddress != "127.0.0.1:"+strconv.Itoa(root.AdbPort(i)) ||
		created.Ports.Grpc != root.GrpcPort(i) || created.Screencap.Shm != shmName(i) || created.Screencap.ShmReady {
		t.Fatalf("addresses = %+v", created)
	}
	if created.Initialized || !fileOrDirExists(created.Path) {
		t.Fatalf("initialized / path: %+v", created)
	}

	if _, err := m.Create(ctx, CreateRequest{Index: &i}); errs.CodeOf(err) != errs.AlreadyExists {
		t.Fatalf("same index again: %v", err)
	}
	five := 5
	if _, err := m.Create(ctx, CreateRequest{Index: &five}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("index 5: %v", err)
	}
	bad := 1000
	if _, err := m.Create(ctx, CreateRequest{MemoryMB: &bad}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("memory 1000: %v", err)
	}

	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Instances) != 1 || list.Instances[0].Index != i || list.Root != m.Root.Dir || list.AdbServerPort != 20391 {
		t.Fatalf("list = %+v", list)
	}

	mem, cpu, fps := 4096, 4, 30
	up, err := m.Update(ctx, i, UpdateRequest{MemoryMB: &mem, CPU: &cpu, FPS: &fps})
	if err != nil {
		t.Fatal(err)
	}
	if up.AppliesNextBoot || up.Instance.Config.MemoryMB != 4096 || up.Instance.Config.CPU != 4 || up.Instance.Config.FPS != 30 {
		t.Fatalf("update = %+v", up)
	}
	if len(up.Applied) == 0 {
		t.Fatalf("applied empty: %+v", up)
	}

	// A start in progress (lock held, here by this process) shows as busy and blocks the delete.
	l, ok, err := win.TryLock(m.lockPath(i, "start"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	info, err := m.Info(ctx, i)
	if err != nil || info.Busy != "starting" {
		l.Unlock()
		t.Fatalf("info while starting: %+v %v", info, err)
	}
	if _, err := m.Delete(ctx, i); errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在启动") {
		l.Unlock()
		t.Fatalf("delete while starting: %v", err)
	}
	l.Unlock()
	if info, _ := m.Info(ctx, i); info.Busy != "" {
		t.Fatalf("busy after unlock: %q", info.Busy)
	}

	st, err := m.Stop(ctx, i, StopOptions{})
	if err != nil || st.Method != "already_offline" || st.Warnings == nil {
		t.Fatalf("stop offline: %+v %v", st, err)
	}

	del, err := m.Delete(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	if del.Index != i || del.Name != root.AvdName(i) || fileOrDirExists(del.Path) {
		t.Fatalf("delete = %+v", del)
	}
	for _, kind := range []string{"start", "boot", "stop"} {
		if fileOrDirExists(m.lockPath(i, kind)) {
			t.Errorf("lock file %s left behind", kind)
		}
	}
	if _, err := m.Info(ctx, i); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("info after delete: %v", err)
	}
	if _, err := m.Stop(ctx, i, StopOptions{}); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("stop after delete: %v", err)
	}

	list, err = m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(list)
	if !strings.Contains(string(b), `"instances":[]`) {
		t.Fatalf("empty list must marshal instances as []: %s", b)
	}
}

func TestOpsCheckOnEmptyRoot(t *testing.T) {
	m, logs := opsTestManager(t)
	ctx := context.Background()
	if m.Root.Components() == nil {
		t.Skip("root.Components not implemented yet")
	}
	saved := vulkanProbeArgv
	defer func() { vulkanProbeArgv = saved }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vulkanProbeArgv = func() ([]string, error) { return []string{exe, "-test.run=^TestVulkanProbeHelperProcess$"}, nil }
	t.Setenv("MODAVD_TEST_VKPROBE_CHILD", "1")

	res, err := m.Check(ctx, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]CheckItem{}
	for _, it := range res.Items {
		ids = append(ids, it.ID)
		byID[it.ID] = it
	}
	if got := strings.Join(ids, ","); got != "path,components,emulator,acceleration,vulkan,disk,memory" {
		t.Fatalf("items = %s", got)
	}
	if res.Ready {
		t.Fatal("an empty root is not ready")
	}
	if it := byID["components"]; it.OK == nil || *it.OK || it.Advice == "" {
		t.Fatalf("components = %+v", it)
	}
	if it := byID["emulator"]; it.OK == nil || *it.OK {
		t.Fatalf("emulator = %+v", it)
	}
	if it := byID["acceleration"]; it.OK != nil {
		t.Fatalf("acceleration without an emulator must be unknown: %+v", it)
	}
	// without a Vulkan driver (CI runners) the item is a non-blocking failure with advice
	if it := byID["vulkan"]; hostHasVulkan() && (it.OK == nil || !*it.OK) ||
		!hostHasVulkan() && (it.OK == nil || *it.OK || it.Blocking || it.Advice == "") {
		t.Fatalf("vulkan = %+v", it)
	}
	if it := byID["disk"]; it.OK == nil {
		t.Fatalf("disk = %+v", it)
	}
	if len(res.Components) == 0 {
		t.Fatal("components missing from the result")
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"id":"acceleration","title":"硬件虚拟化","ok":null`) {
		t.Fatalf("unknown must marshal as null: %s", b)
	}
	t.Logf("check: %s", b)

	err = m.precheckBeforeLaunch(ctx, 6144)
	if errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("precheck on an empty root: %v (%s)", err, errs.CodeOf(err))
	}
	t.Logf("precheck: %v; log warnings: %q", err, logs.Messages())
}
