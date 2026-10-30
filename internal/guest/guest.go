// Package guest holds the scripts modavd pushes into the guest after boot. They are copies of AUTO-MAS
// res/avd/guest/*.sh (the product versions). Keep these shared scripts in sync with the companion launcher.
package guest

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed scripts/*.sh
var scripts embed.FS

const (
	CPUInfoBind  = "cpuinfo-bind.sh"
	IdlePoll     = "mas-idlepoll.sh"
	WifiFastpath = "mas-wifi-fastpath.sh"
	VulkanFix    = "vulkanfix.sh"
)

// RemoteDir is where the scripts go in the guest.
const RemoteDir = "/data/local/tmp"

// Script returns the script with any CR removed (Android's sh chokes on CRLF; .gitattributes keeps them LF, this is
// a second guard against a checkout that converted them).
func Script(name string) ([]byte, error) {
	b, err := scripts.ReadFile("scripts/" + name)
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(b, []byte{'\r'}, nil), nil
}

// tmpSeq tells apart the temp files of goroutines of one process (an MCP server or the GUI starting two instances).
var tmpSeq atomic.Uint64

// LocalFile returns a host copy of the script for adb push: dir\<first 12 hex digits of its sha256>\<name>, written
// when missing (dir is created).
//
// Content-addressed because several modavd processes (CLI, MCP server, GUI, possibly of different versions) push
// these at the same time: a fixed name rewritten in place made one writer's rename fail while another process wrote
// the same temp file or an adb push held the target open, and that boot step was skipped. Here a path, once there,
// always holds the same bytes and is never replaced: a missing one is written to a temp file of this writer and moved
// into place without replacing; losing that race to another writer of the same bytes is success.
func LocalFile(dir, name string) (string, error) {
	b, err := Script(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	sub := filepath.Join(dir, hex.EncodeToString(sum[:])[:12])
	path := filepath.Join(sub, name)
	switch old, err := readStable(path); {
	case err == nil && bytes.Equal(old, b):
		return path, nil
	case err == nil:
		// other bytes under a content address: damaged (nobody can be pushing a right copy from it), so it goes
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return "", err
	}
	tmp := fmt.Sprintf("%s.%d-%d.tmp", path, os.Getpid(), tmpSeq.Add(1))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.Remove(tmp)
		return "", err
	}
	err = moveNoReplace(tmp, path)
	if err != nil {
		os.Remove(tmp)
		// another writer got there first: its file is just as good when it holds these bytes
		if old, rerr := readStable(path); rerr == nil && bytes.Equal(old, b) {
			return path, nil
		}
		return "", err
	}
	return path, nil
}

// readStable tolerates the short window in which another writer has renamed a complete file but still has its
// handle open. Windows reports that as ERROR_SHARING_VIOLATION (or ERROR_LOCK_VIOLATION); retrying here makes the
// content-addressed first write safe across processes without weakening the no-replace invariant.
func readStable(path string) ([]byte, error) {
	const attempts = 40
	for n := 0; ; n++ {
		b, err := os.ReadFile(path)
		if err == nil || n >= attempts || !sharingViolation(err) {
			return b, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func sharingViolation(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == syscall.Errno(32) || errno == syscall.Errno(33))
}

// Remote is the guest path of a script.
func Remote(name string) string { return RemoteDir + "/" + name }
