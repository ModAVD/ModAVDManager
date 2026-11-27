//go:build windows

package emu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

func wantErrorDetails(t *testing.T, err error, want map[string]string) {
	t.Helper()
	got := errs.DetailsOf(err)
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q; error %v, details %v", key, got[key], value, err, got)
		}
	}
}

func TestBusyLockDetails(t *testing.T) {
	m, _ := opsTestManager(t)
	i := 3
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"start", "stop", "boot", "bridge", "create", "disk"} {
		t.Run(kind, func(t *testing.T) {
			held, ok, err := win.TryLock(m.lockPath(i, kind))
			if err != nil || !ok {
				t.Fatalf("hold %s: %v %v", kind, ok, err)
			}
			defer held.Unlock()
			_, err = m.lock(context.Background(), i, kind, 0, "test lock")
			wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: kind})
			_, err = m.tryInstanceLock(i, kind, "test lock")
			wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: kind})
			if kind != "boot" {
				_, err = m.lockInstanceForCompact(i)
				wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: kind})
			}
			if kind == "bridge" {
				_, err = m.Bridge(context.Background(), i, "install")
				wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: "bridge"})
			}
			if kind == "create" {
				if err := m.setCreation(i, creationCreating, "", ""); err != nil {
					t.Fatal(err)
				}
				wantErrorDetails(t, m.notCreated(i, "test"), map[string]string{errs.Reason: "locked", errs.Lock: "create"})
				_, err = m.Stop(context.Background(), i, StopOptions{})
				wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: "create"})
			}
		})
	}
	// A shared disk-info reader owns the same disk lock category as compact.
	reader, err := os.OpenFile(m.lockPath(i, "disk"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var overlap syscall.Overlapped
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	if ok, _, err := lock.Call(reader.Fd(), 1, 0, 0xffffffff, 0xffffffff, uintptr(unsafe.Pointer(&overlap))); ok == 0 {
		t.Fatal(err)
	}

	_, err = m.lockInstanceForCompact(i)
	wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: "disk"})
}

func TestBusyPortDetails(t *testing.T) {
	m, _ := opsTestManager(t)
	i := 3
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	for base := 35000; base < 36000; base += 100 {
		var err error
		listener, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", base+30))
		if err == nil {
			if err := m.Root.UpdateMetadata(func(meta *jsonord.Obj) error { meta.Set("portBase", jsonord.NewInt(int64(base))); return nil }); err != nil {
				listener.Close()
				t.Fatal(err)
			}
			break
		}
	}
	if listener == nil {
		t.Fatal("no test port available")
	}
	defer listener.Close()
	in, err := m.instance(i)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.launchPlanFor(context.Background(), in, 6144, false)
	wantErrorDetails(t, err, map[string]string{errs.Reason: "port_in_use"})
}

func TestCreateFailureDetails(t *testing.T) {
	for _, stopState := range []string{"false", "true", unknownState} {
		t.Run(stopState, func(t *testing.T) {
			m, _ := opsTestManager(t)
			inner := errs.WithDetails(errs.New(errs.Canceled, "test interrupted boot"), map[string]string{
				errs.BootDiagDir: "saved-guest-state", errs.InstanceStatus: "starting", errs.QemuRunning: "true",
			})
			h := &createHooks{running: true}
			h.start = func(*Manager, context.Context, int, StartOptions) (*StartResult, error) { return nil, inner }
			if stopState != "false" {
				h.stopErr = errs.New(errs.Timeout, "test shutdown failed")
			}
			h.install(t)
			saved := errorRunning
			t.Cleanup(func() { errorRunning = saved })
			observations := 0
			errorRunning = func(m *Manager, i int) string {
				observations++
				if !m.lockHeld(i, "create") {
					t.Error("creation observation happened after releasing the create lock")
				}
				if stopState == unknownState {
					return unknownState
				}
				return strconv.FormatBool(h.running)
			}
			i := 6
			_, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true})
			if errs.CodeOf(err) != errs.Canceled || !errors.Is(err, inner) || observations != 1 || h.stopped != 1 {
				t.Fatalf("create: %v, observations %d, stops %d", err, observations, h.stopped)
			}
			wantErrorDetails(t, err, map[string]string{errs.InstanceStatus: "create_failed", errs.InstanceExists: "true",
				errs.QemuRunning: stopState, errs.BootDiagDir: "saved-guest-state"})
			if inner.Details[errs.QemuRunning] != "true" || inner.Details[errs.InstanceStatus] != "starting" {
				t.Fatal("changed the wrapped boot error")
			}
		})
	}
}

func TestCreateFailureExistingAndMissing(t *testing.T) {
	m, _ := opsTestManager(t)
	saved := errorRunning
	t.Cleanup(func() { errorRunning = saved })
	errorRunning = func(*Manager, int) string { return "false" }
	i := 6
	bad := 1
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, MemoryMB: &bad})
	wantErrorDetails(t, err, map[string]string{errs.InstanceExists: "false", errs.InstanceStatus: "not_found", errs.QemuRunning: "false"})
	_, err = m.Create(context.Background(), CreateRequest{MemoryMB: &bad})
	wantErrorDetails(t, err, map[string]string{errs.InstanceExists: unknownState, errs.InstanceStatus: unknownState, errs.QemuRunning: unknownState})
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []CreateRequest{{Index: &i, filesOnly: true}, {Index: &i, MemoryMB: &bad}} {
		_, err := m.Create(context.Background(), req)
		wantErrorDetails(t, err, map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: "offline", errs.QemuRunning: "false"})
		if _, err := avd.Get(m.Root, i); err != nil {
			t.Fatalf("existing instance changed: %v", err)
		}
	}
	if err := avd.Delete(m.Root, i); err != nil {
		t.Fatal(err)
	}
	// Incomplete files still occupy the index; a lossy NotFound cannot prove whether the rest are missing or unreadable.
	writeTestFile(t, avd.IniPath(m.Root, i), "leftover")
	_, err = m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true})
	wantErrorDetails(t, err, map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: unknownState})
	if err := os.Remove(avd.IniPath(m.Root, i)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Root.MetadataPath(), []byte("unreadable json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true})
	wantErrorDetails(t, err, map[string]string{errs.InstanceExists: unknownState, errs.InstanceStatus: unknownState})
}

func TestCreateConcurrentFailureDetails(t *testing.T) {
	m, _ := opsTestManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h := &createHooks{start: func(*Manager, context.Context, int, StartOptions) (*StartResult, error) {
		close(entered)
		<-release
		return nil, errs.New(errs.Timeout, "test boot timed out")
	}}
	h.install(t)
	saved := errorRunning
	t.Cleanup(func() { errorRunning = saved })
	errorRunning = func(*Manager, int) string { return "false" }
	i := 6
	first := make(chan error, 1)
	go func() { _, err := m.Create(context.Background(), CreateRequest{Index: &i}); first <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("creator did not enter setup")
	}
	_, second := m.Create(context.Background(), CreateRequest{Index: &i})
	wantErrorDetails(t, second, map[string]string{errs.Reason: "locked", errs.Lock: "create", errs.InstanceExists: "true", errs.InstanceStatus: unknownState})
	if !m.lockHeld(i, "create") || !fileOrDirExists(avd.ConfigPath(m.Root, i)) {
		t.Error("failed second creator disturbed the first")
	}
	close(release)
	wantErrorDetails(t, <-first, map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: "create_failed", errs.QemuRunning: "false"})
}

func TestErrorObservationUnknown(t *testing.T) {
	m, _ := opsTestManager(t)
	snapshot := listQemuProcesses
	t.Cleanup(func() { listQemuProcesses = snapshot })
	for _, failure := range []error{errors.New("snapshot unavailable"), errs.New(errs.Unsupported, "unsupported snapshot")} {
		calls := injectQemuSnapshotError(t, failure)
		if got := errorRunning(m, 3); got != unknownState || *calls != 1 {
			t.Errorf("errorRunning = %s, calls %d", got, *calls)
		}
	}
	// Unreadable metadata cannot turn into a definite absence.
	if err := os.WriteFile(m.Root.MetadataPath(), []byte("invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	listQemuProcesses = func() ([]win.Process, error) { return nil, nil }
	wantErrorDetails(t, m.withCreateErrorState(3, errs.New(errs.Internal, "original"), false), map[string]string{errs.InstanceExists: unknownState, errs.QemuRunning: unknownState})
	if err := os.WriteFile(m.Root.MetadataPath(), []byte(`{"portBase":"invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := errorRunning(m, 3); got != unknownState {
		t.Fatalf("invalid port plan must not prove qemu absent: %s", got)
	}
}

func TestBusyRunningDetails(t *testing.T) {
	m, _ := opsTestManager(t)
	i := 3
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	diskBusyHeader(t, m.Root, i)
	saved := qemuScan
	t.Cleanup(func() { qemuScan = saved })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{3: {PID: 4242}} }
	_, err := m.Delete(context.Background(), i)
	wantErrorDetails(t, err, map[string]string{errs.Reason: "running"})
	_, err = m.DiskCompact(context.Background(), i)
	wantErrorDetails(t, err, map[string]string{errs.Reason: "running"})
}

func TestCreateRollbackDetails(t *testing.T) {
	m, _ := opsTestManager(t)
	saved := errorRunning
	t.Cleanup(func() { errorRunning = saved })
	errorRunning = func(*Manager, int) string { return "false" }
	// avd.Create has made config.ini when the metadata write hits this directory and rolls it back.
	tmp := fmt.Sprintf("%s.modavd-%d.tmp", m.Root.MetadataPath(), os.Getpid())
	if err := os.MkdirAll(tmp+"/x", 0o755); err != nil {
		t.Fatal(err)
	}
	i := 3
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true})
	if err == nil {
		t.Fatal("expected the metadata write to fail")
	}
	wantErrorDetails(t, err, map[string]string{errs.InstanceExists: "false", errs.InstanceStatus: "not_found", errs.QemuRunning: "false"})
	if len(instanceFilesOnDisk(m.Root, i)) != 0 {
		t.Fatal("rollback left files")
	}
}

func TestCreateRecordedStateAfterUnlock(t *testing.T) {
	m, _ := opsTestManager(t)
	saved := errorRunning
	t.Cleanup(func() { errorRunning = saved })
	errorRunning = func(*Manager, int) string { return "false" }
	i := 3
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.setCreation(i, creationCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	held, ok, err := win.TryLock(m.lockPath(i, "create"))
	if err != nil || !ok {
		t.Fatalf("hold create: %v %v", ok, err)
	}
	defer held.Unlock()
	base := errs.New(errs.Internal, "failed to persist final metadata")
	wantErrorDetails(t, m.withCreateErrorState(i, base, true), map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: "create_failed"})
	wantErrorDetails(t, m.withCreateErrorState(i, base, false), map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: unknownState})
	if !m.lockHeld(i, "create") {
		t.Fatal("observation disturbed the held lock")
	}
	held.Unlock()
	if state, _, _ := m.creation(i); state != creationFailed {
		t.Fatalf("after releasing own lock: %s", state)
	}
}

// avd.Get drops a failed config.ini Stat and returns NotFound. Existing files or metadata mean that result cannot
// prove absence. Inject exactly that lossy boundary result without depending on the host's ACL behavior.
func TestCreateFileObservationUnknown(t *testing.T) {
	m, _ := opsTestManager(t)
	i := 3
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	savedInstance, savedRunning := errorInstanceForState, errorRunning
	t.Cleanup(func() { errorInstanceForState, errorRunning = savedInstance, savedRunning })
	errorRunning = func(*Manager, int) string { return "false" }
	queries := 0
	errorInstanceForState = func(*root.Root, int) (*avd.Instance, error) {
		queries++
		return nil, errs.New(errs.NotFound, "instance lookup lost the config.ini attribute error")
	}
	original := errs.New(errs.InvalidArgument, "original create error").WithAdvice("original advice")
	observed := m.withCreateErrorState(i, original, false)
	wantErrorDetails(t, observed, map[string]string{errs.InstanceExists: "true", errs.InstanceStatus: unknownState, errs.QemuRunning: "false"})
	if queries != 1 || errs.CodeOf(observed) != original.Code || errs.As(observed).Message != original.Message || errs.As(observed).Advice != original.Advice || original.Details != nil {
		t.Fatalf("observation changed the original error: %v, queries %d", observed, queries)
	}
	if err := avd.Delete(m.Root, i); err != nil {
		t.Fatal(err)
	}
	wantErrorDetails(t, m.withCreateErrorState(i, original, false), map[string]string{errs.InstanceExists: "false", errs.InstanceStatus: "not_found", errs.QemuRunning: "false"})
	if queries != 1 {
		t.Fatalf("absence was already established without an instance lookup, queries %d", queries)
	}
}

// A warning about a legacy plan prevents new boots, but does not make its port observation unreliable.
// A malformed value that fell back to the default plan must never scan that default segment as if it were ours.
func TestErrorObservationPortPlanTrust(t *testing.T) {
	for _, c := range []struct {
		raw, want string
		scans     int
	}{
		{`{}`, "false", 1},
		{`{"portBase":20000}`, "false", 1},
		{`{"portBase":21000}`, "false", 1},
		{`{"portBase":5500}`, "false", 1},
		{`{"portBase":"5500"}`, "false", 1},
		{`{"portBase":49100}`, "false", 1},
		{`{"portBase":49100.0}`, "false", 1},
		{`{"portBase":65400}`, "false", 1},
		{`{"portBase":"invalid"}`, unknownState, 0},
		{`{"portBase":21050}`, unknownState, 0},
		{`{"portBase":1000}`, unknownState, 0},
		{`{"portBase":65500}`, unknownState, 0},
		{`{"portBase":null}`, unknownState, 0},
		{`broken json`, unknownState, 0},
	} {
		t.Run(c.raw, func(t *testing.T) {
			m, _ := opsTestManager(t)
			if err := os.WriteFile(m.Root.MetadataPath(), []byte(c.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			saved := listQemuProcesses
			t.Cleanup(func() { listQemuProcesses = saved })
			scans := 0
			listQemuProcesses = func() ([]win.Process, error) { scans++; return nil, nil }
			if got := errorRunning(m, 6); got != c.want || scans != c.scans {
				t.Fatalf("running=%s scans=%d, want %s/%d", got, scans, c.want, c.scans)
			}
		})
	}
}

func TestErrorObservationLockedMetadataDoesNotScan(t *testing.T) {
	m, _ := opsTestManager(t)
	if err := os.WriteFile(m.Root.MetadataPath(), []byte(`{"portBase":5500}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := syscall.UTF16PtrFromString(m.Root.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	file, err := syscall.CreateFile(path, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(file)
	saved := listQemuProcesses
	t.Cleanup(func() { listQemuProcesses = saved })
	scans := 0
	listQemuProcesses = func() ([]win.Process, error) { scans++; return nil, nil }
	if got := errorRunning(m, 6); got != unknownState || scans != 0 {
		t.Fatalf("running=%s scans=%d with unreadable metadata", got, scans)
	}
}
