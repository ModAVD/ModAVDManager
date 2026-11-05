//go:build windows

package ui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// The manager window must not bring the user's browser in: no Windows-account sign-in, no sync, no extensions, and
// no Edge on the user's own profile once the window closes (startup boost).
func TestEdgeArgsKeepUserBrowserOut(t *testing.T) {
	const url = "http://127.0.0.1:1234/?t=abc"
	profile := filepath.Join(testutil.TempDir(t), "p")
	args := edgeArgs(url, profile)
	for _, want := range []string{"--app=" + url, "--user-data-dir=" + profile, "--disable-sync", "--disable-extensions",
		"--no-first-run"} {
		if !slices.Contains(args, want) {
			t.Errorf("edgeArgs lacks %q: %q", want, args)
		}
	}
	// Edge reads one --disable-features (the last): a second one would drop the features of the first
	var features []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--disable-features="); ok {
			features = append(features, v)
		}
	}
	if len(features) != 1 {
		t.Fatalf("%d --disable-features switches: %q", len(features), args)
	}
	for _, want := range []string{"msImplicitSignin", "msEdgeStartupBoost"} {
		if !slices.Contains(strings.Split(features[0], ","), want) {
			t.Errorf("--disable-features=%s lacks %s", features[0], want)
		}
	}
}

// ui-edge belongs to managers up to 0.3.0 and may be signed in with the user's account: new managers stay off it.
func TestEdgeProfileDirNotOldProfile(t *testing.T) {
	dir := EdgeProfileDir()
	if filepath.Dir(dir) != AppDataDir() {
		t.Errorf("EdgeProfileDir %q is not under %q", dir, AppDataDir())
	}
	if strings.EqualFold(filepath.Base(dir), "ui-edge") {
		t.Errorf("EdgeProfileDir is the old profile %q", dir)
	}
}

// TestEdgeNoImplicitSignIn runs headless Edge on new profiles of its own: without msImplicitSignin disabled it signs
// in with the Windows account, with edgeArgs it must not. Opt-in (MODAVD_EDGE_PROBE=1), since it needs Edge and a
// Windows account Edge can sign in with, and the first run does sign that account into its temporary profile (sync
// stays off). It guards the feature name, which Edge does not document.
func TestEdgeNoImplicitSignIn(t *testing.T) {
	if os.Getenv("MODAVD_EDGE_PROBE") != "1" {
		t.Skip("MODAVD_EDGE_PROBE=1 runs Edge")
	}
	exe := FindEdge()
	if exe == "" {
		t.Skip("no Edge")
	}
	took, signed := edgeSignsIn(t, exe, func(args []string) []string {
		for i, a := range args {
			if a == "--disable-features="+edgeDisabledFeatures {
				args[i] = "--disable-features=msEdgeStartupBoost"
			}
		}
		return args
	}, time.Minute)
	if !signed {
		t.Skip("Edge does not sign in even without the switch (no Windows account to sign in with?)")
	}
	t.Logf("without the switch Edge signed in after %v", took.Round(time.Millisecond))
	if _, signed := edgeSignsIn(t, exe, func(args []string) []string { return args }, max(2*took, 20*time.Second)); signed {
		t.Fatal("Edge signed the manager profile in despite edgeArgs")
	}
}

// edgeSignsIn starts headless Edge on a new profile with edit(edgeArgs) and waits up to limit for the profile to be
// signed in, reporting how long that took.
func edgeSignsIn(t *testing.T, exe string, edit func([]string) []string, limit time.Duration) (time.Duration, bool) {
	t.Helper()
	dir, err := os.MkdirTemp("", "modavd-edge-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeProfile(t, dir)
	cmd := exec.Command(exe, append(edit(edgeArgs("about:blank", dir)), "--headless")...)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	for time.Since(start) < limit {
		if profileSignedIn(dir) {
			return time.Since(start), true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return limit, false
}

// profileSignedIn: Edge recorded an implicit sign-in (Local State) or an account (Preferences). Edge replaces both
// files whole, so a read never sees half a file.
func profileSignedIn(dir string) bool {
	var ls struct {
		Profiles struct {
			Implicit json.RawMessage `json:"edge_implicitly_signed_in"`
		} `json:"profiles"`
	}
	var pref struct {
		AccountInfo []json.RawMessage `json:"account_info"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Local State")); err == nil && json.Unmarshal(b, &ls) == nil {
		if s := string(ls.Profiles.Implicit); s != "" && s != "null" {
			return true
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Default", "Preferences")); err == nil && json.Unmarshal(b, &pref) == nil {
		return len(pref.AccountInfo) > 0
	}
	return false
}

// removeProfile removes a test profile once the Edge child processes have let go of it.
func removeProfile(t *testing.T, dir string) {
	var err error
	for range 40 {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("test profile %s left behind: %v", dir, err)
}

var oldProfileFiles = []string{"Local State", `Default\Preferences`, `Default\History`, `Default\Login Data`,
	`Default\Extensions\iikmkjmpaadaobahmlepeloendndfphd\5.5.1_0\manifest.json`}

// fakeOldProfile makes dir (appData\ui-edge or a copy) with the files of an Edge profile.
func fakeOldProfile(t *testing.T, dir string) {
	t.Helper()
	for _, f := range oldProfileFiles {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// logLines keeps every message of a logger, whatever its level.
type logLines struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logLines) logger() *logx.Logger {
	return logx.New(func(_ logx.Level, msg string) {
		l.mu.Lock()
		l.msgs = append(l.msgs, msg)
		l.mu.Unlock()
	})
}

func (l *logLines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.msgs, "\n")
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// requireOldProfile: dir is still there, every file intact.
func requireOldProfile(t *testing.T, dir string) {
	t.Helper()
	for _, f := range oldProfileFiles {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || string(b) != f {
			t.Errorf("%s: %q, %v", f, b, err)
		}
	}
}

// holdOpen opens path as Edge holds its files, until the returned func: the profile's lockfile is shared for reading
// only and deleted on close (Chromium's process singleton), the databases are shared for reading and writing.
func holdOpen(t *testing.T, path string, lockfile bool) func() {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	share, flags, create := uint32(syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE), uint32(0), uint32(syscall.OPEN_EXISTING)
	if lockfile {
		const fileFlagDeleteOnClose = 0x04000000
		share, flags, create = syscall.FILE_SHARE_READ, fileFlagDeleteOnClose, syscall.CREATE_ALWAYS
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, share, nil, create,
		syscall.FILE_ATTRIBUTE_NORMAL|flags, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	var once atomic.Bool
	release := func() {
		if once.CompareAndSwap(false, true) {
			syscall.CloseHandle(h)
		}
	}
	t.Cleanup(release)
	return release
}

// Only appData\ui-edge goes: not the new profile, not the state files, not a folder with a similar name.
func TestRemoveOldEdgeProfileOnlyThatFolder(t *testing.T) {
	appData := testutil.TempDir(t)
	old := filepath.Join(appData, oldEdgeProfile)
	fakeOldProfile(t, old)
	keep := []string{`ui-edge2\Default\Preferences`, `ui-edge2\Local State`, "ui-46fbca8bcf6316d5.json",
		"ui-46fbca8bcf6316d5.lock", "update-settings.json", `ui-edge-backup\Local State`, `ui-edge.old\Local State`}
	for _, f := range keep {
		p := filepath.Join(appData, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logs logLines
	removeOldEdgeProfile(appData, logs.logger())
	if exists(old) || exists(old+".deleting") {
		t.Errorf("old profile left: %v, being deleted: %v", exists(old), exists(old+".deleting"))
	}
	for _, f := range keep {
		if b, err := os.ReadFile(filepath.Join(appData, f)); err != nil || string(b) != f {
			t.Errorf("%s touched: %q, %v", f, b, err)
		}
	}
	if m := logs.text(); !strings.Contains(m, "删掉了旧版管理器的 Edge 配置") {
		t.Errorf("log: %s", m)
	}
	// nothing there any more: a no-op, nothing logged
	before := len(logs.msgs)
	removeOldEdgeProfile(appData, logs.logger())
	if n := len(logs.msgs); n != before {
		t.Errorf("second run logged %q", logs.msgs[before:])
	}
}

// While an Edge runs on the old profile (an older manager's window), it stays whole; the next start deletes it.
func TestRemoveOldEdgeProfileInUse(t *testing.T) {
	for _, c := range []struct {
		name, file string
		lockfile   bool
	}{
		{"lockfile", "lockfile", true},
		{"open database", `Default\History`, false}, // no lockfile check: the rename refuses
	} {
		t.Run(c.name, func(t *testing.T) {
			appData := testutil.TempDir(t)
			old := filepath.Join(appData, oldEdgeProfile)
			fakeOldProfile(t, old)
			release := holdOpen(t, filepath.Join(old, c.file), c.lockfile)
			var logs logLines
			removeOldEdgeProfile(appData, logs.logger())
			requireOldProfile(t, old)
			if exists(old + ".deleting") {
				t.Error("renamed while in use")
			}
			if m := logs.text(); !strings.Contains(m, "下次打开时再删") {
				t.Errorf("log: %s", m)
			}
			release()
			removeOldEdgeProfile(appData, logs.logger())
			if exists(old) || exists(old+".deleting") {
				t.Errorf("after Edge exited: old %v, being deleted %v", exists(old), exists(old+".deleting"))
			}
		})
	}
}

// A deletion cut short (power cut, a file the virus scanner held) is finished on the next start.
func TestRemoveOldEdgeProfileLeftover(t *testing.T) {
	appData := testutil.TempDir(t)
	trash := filepath.Join(appData, oldEdgeProfile+".deleting")
	fakeOldProfile(t, trash)
	release := holdOpen(t, filepath.Join(trash, `Default\History`), false)
	var logs logLines
	removeOldEdgeProfile(appData, logs.logger())
	if !exists(trash) {
		t.Fatal("a file held open, yet the folder is gone")
	}
	if m := logs.text(); !strings.Contains(m, "下次打开时再删") {
		t.Errorf("log: %s", m)
	}
	release()
	removeOldEdgeProfile(appData, logs.logger())
	if exists(trash) {
		t.Error("leftover not deleted on the next start")
	}
}

// ui-edge that is not a plain folder (a junction someone made, a file) is left alone, and so is what it points to.
func TestRemoveOldEdgeProfileNotPlainFolder(t *testing.T) {
	appData := testutil.TempDir(t)
	target := filepath.Join(testutil.TempDir(t), "elsewhere")
	fakeOldProfile(t, target)
	old := filepath.Join(appData, oldEdgeProfile)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", old, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	removeOldEdgeProfile(appData, logx.Nop())
	requireOldProfile(t, target)
	if !exists(old) {
		t.Error("junction removed")
	}
	if err := os.Remove(old); err != nil { // the junction only
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeOldEdgeProfile(appData, logx.Nop())
	if b, err := os.ReadFile(old); err != nil || string(b) != "file" {
		t.Errorf("file ui-edge touched: %q, %v", b, err)
	}
	// nor a junction in place of the folder being deleted
	trash := old + ".deleting"
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", trash, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	removeOldEdgeProfile(appData, logx.Nop())
	requireOldProfile(t, target)
	if !exists(trash) {
		t.Error("junction ui-edge.deleting removed")
	}
	os.Remove(trash)
}

// The deletion runs after the window opened and never holds the manager up: Run opens the window and exits as usual
// while it is still stuck.
func TestRunOpensWindowDespiteOldProfile(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	var launched atomic.Bool
	removing := make(chan bool, 2) // whether the window was open when the deletion started
	stuck := make(chan struct{})
	defer close(stuck)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{Root: r, OpenWindow: true, IdleExit: 100 * time.Millisecond,
			token:      testToken,
			newBackend: func(l *logx.Logger) backend { return fakeBackend{f, l} },
			launch: func(string) (*Window, error) {
				time.Sleep(200 * time.Millisecond) // a deletion started before this would run meanwhile
				launched.Store(true)
				return &Window{Started: time.Now()}, nil
			},
			removeOld: func(*logx.Logger) {
				removing <- launched.Load()
				<-stuck
			},
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not exit while the deletion was stuck")
	}
	if !launched.Load() {
		t.Error("no window opened")
	}
	select {
	case open := <-removing:
		if !open {
			t.Error("deletion started before the window opened")
		}
	case <-time.After(5 * time.Second):
		t.Error("old profile never deleted")
	}
	if len(removing) > 0 {
		t.Error("deletion started more than once")
	}
}
