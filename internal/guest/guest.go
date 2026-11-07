// Package guest holds the scripts modavd pushes into the guest after boot. They are copies of AUTO-MAS
// res/avd/guest/*.sh (the product versions). Until AUTO-MAS calls modavd for booting, changes go into both places.
// It also carries the guest daemon of the per-app proxy (ProxyDaemon), which build.ps1 builds and puts in proxyd/.
package guest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed scripts/*.sh
var scripts embed.FS

//go:embed proxyd
var proxyd embed.FS

// ProxyDaemonName is the daemon's file name, on the host and in the guest.
const ProxyDaemonName = "modavd-proxyd"

// ErrNoProxyDaemon: this modavd was built without build.ps1, so it carries no guest daemon.
var ErrNoProxyDaemon = errors.New("这个 modavd 编的时候没带按应用走代理的客体程序（要用 build.ps1 编）")

// ProxyDaemon is cmd/modavd-proxyd for linux/amd64, as build.ps1 embedded it.
func ProxyDaemon() ([]byte, error) {
	z, err := proxyd.ReadFile("proxyd/" + ProxyDaemonName + ".gz")
	if err != nil {
		return nil, ErrNoProxyDaemon
	}
	r, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// LocalProxyDaemon is LocalFile for the daemon.
func LocalProxyDaemon(dir string) (string, error) {
	b, err := ProxyDaemon()
	if err != nil {
		return "", err
	}
	return localCopy(dir, ProxyDaemonName, b)
}

const (
	CPUInfoBind  = "cpuinfo-bind.sh"
	IdlePoll     = "mas-idlepoll.sh"
	WifiFastpath = "mas-wifi-fastpath.sh"
	// LibFix mounts the patched guest libraries, the Vulkan driver and the ARM translation layer (modavd only;
	// AUTO-MAS keeps its own vulkanfix.sh for the Vulkan driver alone, with the same guest cache and upload names).
	LibFix = "libfix.sh"
	// PlayStore installs, checks and removes the Google Play Store (modavd only, not in AUTO-MAS).
	PlayStore = "playstore.sh"
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
	return localCopy(dir, name, b)
}

// localCopy is LocalFile for any bytes.
func localCopy(dir, name string, b []byte) (string, error) {
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
	if err := moveNoReplace(tmp, path); err != nil {
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
