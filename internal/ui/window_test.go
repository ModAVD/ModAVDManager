//go:build windows

package ui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The manager window must not bring the user's browser in: no Windows-account sign-in, no sync, no extensions.
func TestEdgeArgsKeepUserBrowserOut(t *testing.T) {
	const url = "http://127.0.0.1:1234/?t=abc"
	profile := filepath.Join(t.TempDir(), "p")
	args := edgeArgs(url, profile)
	for _, want := range []string{"--app=" + url, "--user-data-dir=" + profile, "--disable-features=msImplicitSignin",
		"--disable-sync", "--disable-extensions", "--no-first-run"} {
		if !slices.Contains(args, want) {
			t.Errorf("edgeArgs lacks %q: %q", want, args)
		}
	}
	// Edge reads one --disable-features (the last); a second one would drop msImplicitSignin
	n := 0
	for _, a := range args {
		if strings.HasPrefix(a, "--disable-features=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d --disable-features switches: %q", n, args)
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
		return slices.DeleteFunc(args, func(a string) bool { return a == "--disable-features=msImplicitSignin" })
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
