//go:build windows

package ui

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/emu"
)

// TestScreenshotSharedMemory: a frame from shared memory is served without an adb call and says so; when it fails the
// capture falls back to adb, and the next ones skip shared memory for a while.
func TestScreenshotSharedMemory(t *testing.T) {
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)
	shm := append(append([]byte{}, pngSignature...), []byte("from shared memory")...)
	adbPNG := append(append([]byte{}, pngSignature...), []byte("from adb")...)
	h.f.mu.Lock()
	h.f.shot = adb.Result{Code: 0, Stdout: adbPNG}
	h.f.mu.Unlock()

	var calls atomic.Int32
	var serial, dir atomic.Value
	fail := atomic.Bool{}
	h.s.frame = func(rootDir, ser string) ([]byte, error) {
		calls.Add(1)
		dir.Store(rootDir)
		serial.Store(ser)
		if fail.Load() {
			return nil, errors.New("sharing is off")
		}
		return shm, nil
	}
	get := func() (string, string) {
		t.Helper()
		resp := h.do("GET", "/api/instances/3/screenshot", nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Header.Get("X-Shot-Source")
	}

	if body, src := get(); body != string(shm) || src != "shm" {
		t.Fatalf("shared memory capture: %q from %q", body, src)
	}
	if n := h.f.adbCalls.Load(); n != 0 {
		t.Fatalf("%d adb calls for a shared-memory frame", n)
	}
	if serial.Load() != "emulator-21030" || dir.Load() != h.s.root.Dir {
		t.Fatalf("asked for %v in %v", serial.Load(), dir.Load())
	}

	// shared memory fails: adb answers, and the next capture does not even try shared memory
	fail.Store(true)
	time.Sleep(shotReuse + 50*time.Millisecond)
	if body, src := get(); body != string(adbPNG) || src != "adb" {
		t.Fatalf("fallback: %q from %q", body, src)
	}
	tried := calls.Load()
	time.Sleep(shotReuse + 50*time.Millisecond)
	if _, src := get(); src != "adb" || calls.Load() != tried {
		t.Fatalf("shared memory retried inside the hold-back (%d -> %d calls)", tried, calls.Load())
	}

	// not a PNG from shared memory counts as a failure too
	h.s.frame = func(string, string) ([]byte, error) { return []byte("garbage"), nil }
	h.s.shots.forget(3)
	if body, src := get(); body != string(adbPNG) || src != "adb" {
		t.Fatalf("garbage frame: %q from %q", body, src)
	}

	// no library function at all: adb only
	h.s.frame = nil
	h.s.shots.forget(3)
	if _, src := get(); src != "adb" {
		t.Fatalf("source %q without frame function", src)
	}
}

// TestShmFrameWithoutLibrary: a root without extras\modavd_extras.dll just answers an error.
func TestShmFrameWithoutLibrary(t *testing.T) {
	if _, err := shmFrame(t.TempDir(), "emulator-21030"); err == nil {
		t.Fatal("no error without the library")
	}
}
