//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/win"
)

func logcatTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Windows may briefly report ERROR_DIR_NOT_EMPTY after a segment was deleted. Go's TempDir cleanup does not
	// retry that error. Retry only that transient here; a persistent failure still fails the test.
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if !errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY) || time.Now().After(deadline) {
				if err != nil {
					t.Errorf("logcat test cleanup: %v", err)
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return dir
}

func numberedLog(i, count int) []byte {
	var b bytes.Buffer
	for n := 0; n < count; n++ {
		fmt.Fprintf(&b, "instance=%d sequence=%06d payload=abcdefghijk\n", i, n)
	}
	return b.Bytes()
}

func readLogcatSegments(t *testing.T, dir string, i int) ([]byte, []string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("logcat-mas_%d-*.txt", i)))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	var all []byte
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			t.Fatalf("record split across files: %s", path)
		}
		all = append(all, b...)
	}
	return all, paths
}

// Polling may overlap pruneLogDir's Windows delete-access probe or observe the middle of an adb write. Only the
// final comparison below treats a snapshot as a complete stream, once the producer is waiting at its gate.
func logcatTestContains(dir string, i int, marker string, minSegments int) bool {
	paths, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("logcat-mas_%d-*.txt", i)))
	if len(paths) < minSegments {
		return false
	}
	for _, path := range paths {
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(marker)) {
			return true
		}
	}
	return false
}

func TestLogcatRotationPreservesNumberedLines(t *testing.T) {
	for _, chunk := range []int{1, 7, 63, 4096} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			dir := logcatTestDir(t)
			w, err := newLogcatWriter(dir, 2, 127, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { w.Close() })
			input := numberedLog(2, 60)
			for p := input; len(p) > 0; {
				n := min(chunk, len(p))
				if got, err := w.Write(p[:n]); err != nil || got != n {
					t.Fatalf("write %d: %d, %v", n, got, err)
				}
				p = p[n:]
			}
			got, paths := readLogcatSegments(t, dir, 2)
			if len(paths) < 3 || !bytes.Equal(got, input) {
				t.Fatalf("%d segments; numbered stream changed: got %d, want %d bytes", len(paths), len(got), len(input))
			}
			for _, path := range paths[:len(paths)-1] {
				st, _ := os.Stat(path)
				if st.Size() < 127 || st.Size() > 127+int64(len(numberedLog(2, 1))) {
					t.Fatalf("segment size %d", st.Size())
				}
			}
		})
	}
}

func TestLogcatRotationRetentionAndActiveHandle(t *testing.T) {
	for _, total := range []bool{false, true} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			dir := logcatTestDir(t)
			var pruneErr error
			w, err := newLogcatWriter(dir, 0, 90, func() {
				keep, maxBytes := 2, int64(1<<30)
				if total {
					keep, maxBytes = 100, 180
				}
				_, _, pruneErr = pruneLogDir(dir, 0, keep, maxBytes, logx.Nop())
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { w.Close() })
			if _, err := w.Write(numberedLog(0, 80)); err != nil || pruneErr != nil {
				t.Fatalf("write/prune: %v / %v", err, pruneErr)
			}
			_, paths := readLogcatSegments(t, dir, 0)
			if len(paths) > 3 || len(paths) < 2 {
				t.Fatalf("old segments not pruned: %q", paths)
			}
			// Force even the active file to be the oldest and over both limits. Its handle must protect it.
			active := w.file.Name()
			old := time.Unix(1, 0)
			if err := os.Chtimes(active, old, old); err != nil {
				t.Fatal(err)
			}
			if _, _, err := pruneLogDir(dir, 0, 0, 0, logx.Nop()); err != nil {
				t.Fatal(err)
			}
			if busy, _ := fileInUse(active); !busy {
				t.Fatal("current file is not protected")
			}
			files, err := managedLogFiles(dir)
			if err != nil || len(files) != 1 || files[0].path != active {
				t.Fatalf("current file/old-segment cleanup: %v, %v", files, err)
			}
		})
	}
}

func TestLogcatRotationInstancesAndSameSecondRestart(t *testing.T) {
	dir := logcatTestDir(t)
	var wg sync.WaitGroup
	for _, i := range []int{0, 1} {
		wg.Go(func() {
			for range 2 {
				w, err := newLogcatWriter(dir, i, 95, nil)
				if err != nil {
					t.Error(err)
					return
				}
				_, err = w.Write(numberedLog(i, 20))
				if err := errors.Join(err, w.Close()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	for _, i := range []int{0, 1} {
		got, paths := readLogcatSegments(t, dir, i)
		want := bytes.Repeat(numberedLog(i, 20), 2)
		if len(paths) < 6 || !bytes.Equal(got, want) {
			t.Fatalf("instance %d mixed, truncated or repeated its stream", i)
		}
	}
}

func TestLogcatRotationRestartAfterPruningKeepsNameOrder(t *testing.T) {
	dir := logcatTestDir(t)
	w, err := newLogcatWriter(dir, 0, 90, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(numberedLog(0, 20)); err != nil {
		t.Fatal(err)
	}
	last := filepath.Base(w.file.Name())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pruneLogDir(dir, 0, 1, 1<<30, logx.Nop()); err != nil {
		t.Fatal(err)
	}
	w, err = newLogcatWriter(dir, 0, 90, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if next := filepath.Base(w.file.Name()); next <= last {
		t.Fatalf("restart reused an earlier timestamp: %s <= %s", next, last)
	}
}

func TestLogcatRotationFailureKeepsPreviousFile(t *testing.T) {
	dir := logcatTestDir(t)
	w, err := newLogcatWriter(dir, 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	// Make opening the next segment fail, without changing the already open file.
	w.dir = w.file.Name()
	if n, err := w.Write([]byte("second\n")); err == nil || n != 0 {
		t.Fatalf("failed rotation reported %d, %v", n, err)
	}
	got, err := os.ReadFile(w.file.Name())
	if err != nil || string(got) != "first\n" {
		t.Fatalf("previous segment changed: %q, %v", got, err)
	}
}

type logcatFailWriter struct{}

func (logcatFailWriter) Write([]byte) (int, error) { return 0, os.ErrPermission }

func TestLogcatWriteFailureEndsAdb(t *testing.T) {
	r, _ := portPlanRoot(t)
	t.Setenv(fakeLogcatStreamEnv, filepath.Join(r.Dir, "never-emit"))
	m := New(r, nil)
	done := make(chan error, 1)
	go func() {
		done <- captureLogcat(context.Background(), r.AdbExe(), logcatArgs(r.AdbServerPort(), r.Ports().Serial(6)), m.ADB.Env(), logcatFailWriter{})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("write error lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		clients, _ := m.findLogcatProcesses(r.Ports().Serial(6))
		endProcesses(clients, time.Second)
		t.Fatal("writer failure left adb running")
	}
	if clients, err := m.findLogcatProcesses(r.Ports().Serial(6)); err != nil || len(clients) != 0 {
		t.Fatalf("capture still active: %v, %v", clients, err)
	}
}

func TestLogcatCaptureRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"missing-index"}, {t.TempDir(), "5", "1", "1"},
		{t.TempDir(), "-1", "1", "1"}, {t.TempDir(), "not-a-number", "1", "1"},
		{t.TempDir(), "0", "0", "1"}, {t.TempDir(), "0", "1", "0"}} {
		if code := RunLogcatCapture(args, &bytes.Buffer{}); code != 2 {
			t.Errorf("args %q: exit %d", args, code)
		}
	}
	if code := RunLogcatCapture([]string{filepath.Join(t.TempDir(), "missing"), "0", "1", "1"}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("missing root: exit %d", code)
	}
}

const (
	fakeLogcatStreamEnv = "MODAVD_TEST_LOGCAT_STREAM"
	fakeLogcatLaunchArg = "__test_logcat_launch"
)

func fakeLogcatStream(args []string) int {
	if path := os.Getenv("MODAVD_TEST_CAPTURE_ENV"); path != "" {
		exe, _ := os.Executable()
		b, _ := json.Marshal(map[string]any{"exe": exe, "args": args, "sdk": os.Getenv("ANDROID_SDK_ROOT"), "home": os.Getenv("ANDROID_HOME"), "avd": os.Getenv("ANDROID_AVD_HOME"), "server": os.Getenv("ANDROID_ADB_SERVER_PORT")})
		os.WriteFile(path, b, 0o644)
	}
	serial := args[slices.Index(args, "-s")+1]
	for n := 0; n < 15; n++ {
		fmt.Printf("%s sequence=%06d payload=abcdefghijk\n", serial, n)
		time.Sleep(20 * time.Millisecond)
	}
	// A new file from the test asks for more records after the launching CLI has exited and after rotation.
	gate := os.Getenv(fakeLogcatStreamEnv)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(gate); err == nil {
			fmt.Printf("%s after-cli-exit\n", serial)
			for {
				time.Sleep(time.Second)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 1
}

func TestLogcatOrphanSurvivesLauncherAndRestartsOnce(t *testing.T) {
	r, _ := portPlanRoot(t)
	copyTestExe(t, filepath.Join(r.Dir, "tools", "modavd.exe"))
	t.Setenv("LOCALAPPDATA", t.TempDir())
	q := fakeQemu(t, filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64"), 6, 21060, false, filepath.Join(r.AvdHome(), "mas_6.avd"))
	savedScan := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: q} }
	t.Cleanup(func() { qemuScan = savedScan })
	gate := filepath.Join(r.Dir, "emit-after-cli-exit")
	t.Setenv(fakeLogcatStreamEnv, gate)
	m := New(r, nil)
	t.Cleanup(func() {
		clients, _ := m.findLogcatProcesses(r.Ports().Serial(6))
		endProcesses(clients, time.Second)
		waitLogcatTest(t, func() bool { return !m.lockHeld(6, "logcat") })
	})
	self, _ := os.Executable()
	launch := func() {
		t.Helper()
		cmd := exec.Command(self, fakeLogcatLaunchArg, r.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10))
		win.HideConsoleCmd(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("launcher: %v, %s", err, out)
		}
	}
	launch() // the process that called ensureLogcat has now exited
	waitLogcatTest(t, func() bool {
		return logcatTestContains(r.LogsDir(), 6, "sequence=000014 payload=abcdefghijk\n", 3)
	})
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitLogcatTest(t, func() bool {
		return logcatTestContains(r.LogsDir(), 6, "after-cli-exit\n", 3)
	})
	got, _ := readLogcatSegments(t, r.LogsDir(), 6)
	var want strings.Builder
	for n := 0; n < 15; n++ {
		fmt.Fprintf(&want, "emulator-21060 sequence=%06d payload=abcdefghijk\n", n)
	}
	want.WriteString("emulator-21060 after-cli-exit\n")
	if string(got) != want.String() {
		t.Fatalf("orphan stream changed: %q", got)
	}
	clients, err := m.findLogcatProcesses(r.Ports().Serial(6))
	if err != nil || len(clients) != 1 {
		t.Fatalf("active clients: %v, %v", clients, err)
	}
	helper, ok := win.FindProcess(clients[0].PPID)
	if !ok {
		t.Fatal("cannot find logcat helper")
	}
	if path, err := helper.ImagePath(); err != nil || !strings.EqualFold(path, filepath.Join(r.Dir, "tools", "modavd.exe")) {
		t.Fatalf("capture did not use the root's CLI: %s, %v", path, err)
	}
	launch()                             // adopting the running capture must not start another helper/client
	endProcesses(clients, 2*time.Second) // same EOF as a stopped emulator or a stale-server client
	// Start immediately, while the old writer may still be draining and releasing its lock.
	launch()
	waitLogcatTest(t, func() bool {
		live, _ := m.findLogcatProcesses(r.Ports().Serial(6))
		return len(live) == 1 && live[0].PID != clients[0].PID
	})
	if path := m.ensureLogcat(context.Background(), 6); path == "" {
		t.Fatal("restarted capture did not expose its current file")
	}
}

func TestLogcatQemuExitEndsStubbornAdbAndReleasesFiles(t *testing.T) {
	r, _ := portPlanRoot(t)
	copyTestExe(t, filepath.Join(r.Dir, "tools", "modavd.exe"))
	t.Setenv("LOCALAPPDATA", t.TempDir())
	q := fakeQemu(t, filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64"), 6, 21060, false, filepath.Join(r.AvdHome(), "mas_6.avd"))
	gate := filepath.Join(r.Dir, "keep-adb-running")
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeLogcatStreamEnv, gate)
	m := New(r, nil)
	var helper win.Process
	t.Cleanup(func() {
		clients, _ := m.findLogcatProcesses(r.Ports().Serial(6))
		endProcesses(clients, time.Second)
		endProcesses([]win.Process{helper}, time.Second)
	})
	self, _ := os.Executable()
	cmd := exec.Command(self, fakeLogcatLaunchArg, r.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10))
	win.HideConsoleCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v, %s", err, out)
	}
	waitLogcatTest(t, func() bool { return logcatTestContains(r.LogsDir(), 6, "after-cli-exit\n", 3) })
	clients, err := m.findLogcatProcesses(r.Ports().Serial(6))
	if err != nil || len(clients) != 1 {
		t.Fatalf("active clients: %v, %v", clients, err)
	}
	var ok bool
	helper, ok = win.FindProcess(clients[0].PPID)
	if !ok {
		t.Fatal("cannot find logcat helper")
	}
	// The fake adb deliberately never observes qemu's exit: only the writer's qemu watch can end it.
	if err := q.Kill(); err != nil || !q.Wait(time.Second) {
		t.Fatalf("end fake qemu: %v", err)
	}
	if !helper.Wait(3 * time.Second) {
		t.Fatal("qemu exited but logcat helper kept running and blocking its executable")
	}
	if clients[0].Alive() || m.lockHeld(6, "logcat") {
		t.Fatal("qemu exit left adb or the capture lock alive")
	}
	_, paths := readLogcatSegments(t, r.LogsDir(), 6)
	for _, path := range paths {
		if busy, _ := fileInUse(path); busy {
			t.Fatalf("capture still holds %s", path)
		}
	}
	// A reused PID must not adopt a different boot, even when that PID is alive.
	selfProc, _ := win.FindProcess(uint32(os.Getpid()))
	if code := RunLogcatCapture([]string{r.Dir, "6", strconv.Itoa(os.Getpid()), strconv.FormatUint(selfProc.Created+1, 10)}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("stale qemu identity: exit %d", code)
	}
	m.removeLockFiles(6)
	if _, err := os.Stat(m.lockPath(6, "logcat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete left capture lock: %v", err)
	}
}

func waitLogcatTest(t *testing.T, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for logcat")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
