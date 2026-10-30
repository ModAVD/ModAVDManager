//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/root"
	"modavd/internal/ui"
	"modavd/internal/win"
)

// One manager per root: a lock file plus a state file under %LOCALAPPDATA%\ModAVD, both named after a hash of the
// root path. The lock is held for the manager's whole life and dies with its process, so a crash never blocks the
// next start; the state file tells a second launch where the running manager listens.

// instanceKey is "ui-<16 hex>" for the root (case-insensitive, like Windows paths).
func instanceKey(r *root.Root) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(r.Dir))))
	return "ui-" + hex.EncodeToString(sum[:8])
}

func lockPath(r *root.Root) string  { return filepath.Join(ui.AppDataDir(), instanceKey(r)+".lock") }
func statePath(r *root.Root) string { return filepath.Join(ui.AppDataDir(), instanceKey(r)+".json") }

// state is the content of the state file.
type state struct {
	PID       int    `json:"pid"`
	URL       string `json:"url"` // "http://127.0.0.1:<port>/"
	Port      int    `json:"port"`
	Token     string `json:"token"`
	Root      string `json:"root"`
	StartedAt string `json:"started_at"`
}

type instance struct {
	r    *root.Root
	lock *win.FileLock
}

// lockInstance takes the root's manager lock; nil (and no error) when another manager holds it.
func lockInstance(r *root.Root) (*instance, error) {
	l, ok, err := win.TryLock(lockPath(r))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	// a state file left by a manager that was killed must not send a second launch to a dead port meanwhile
	os.Remove(statePath(r))
	return &instance{r: r, lock: l}, nil
}

// writeState writes the state file atomically (a second launch may read it at any moment).
func (in *instance) writeState(baseURL, token string) error {
	var port int
	fmt.Sscanf(strings.TrimPrefix(baseURL, "http://127.0.0.1:"), "%d", &port)
	data, err := json.MarshalIndent(state{PID: os.Getpid(), URL: baseURL, Port: port, Token: token, Root: in.r.Dir,
		StartedAt: time.Now().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return err
	}
	path := statePath(in.r)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// release removes the state file, then lets go of the lock (in that order: a new manager may write its own state
// as soon as it has the lock). Safe to call twice.
func (in *instance) release() {
	if in.lock == nil {
		return
	}
	os.Remove(statePath(in.r))
	in.lock.Unlock()
	in.lock = nil
}

// readStateWait reads the running manager's state, waiting up to d for it to appear (that manager may still be
// starting).
func readStateWait(r *root.Root, d time.Duration) (state, error) {
	deadline := time.Now().Add(d)
	for {
		st, err := readState(r)
		if err == nil {
			return st, nil
		}
		if time.Now().After(deadline) {
			return state{}, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func readState(r *root.Root) (state, error) {
	data, err := os.ReadFile(statePath(r))
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, err
	}
	if st.URL == "" || st.Token == "" || !strings.HasPrefix(st.URL, "http://127.0.0.1:") {
		return state{}, errors.New("状态文件内容不对: " + statePath(r))
	}
	return st, nil
}
