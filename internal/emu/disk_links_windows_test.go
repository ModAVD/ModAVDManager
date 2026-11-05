//go:build windows

package emu

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
)

// No SDK or process scan is needed: linked overlays must be refused before
// qemu-img, lock creation, or cleanup of a previous compact's scratch file.
func TestDiskCompactLinkedOverlay(t *testing.T) {
	for _, kind := range []string{"hardlink", "hardlink with alias", "symlink", "dangling symlink"} {
		for _, stray := range []bool{false, true} {
			name := kind
			if stray {
				name += "/stray temp"
			}
			t.Run(name, func(t *testing.T) {
				r := managerTestRoot(t, 2)
				avdDir := filepath.Join(r.AvdHome(), root.AvdName(2)+".avd")
				overlay := filepath.Join(avdDir, userdataOverlay)
				target := filepath.Join(r.Dir, "target.qcow2")
				alias := filepath.Join(r.Dir, "other-name.qcow2")
				data := bytes.Repeat([]byte("keep the original data\x00"), 64)
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind != "hardlink" {
					if err := os.Link(target, alias); err != nil {
						t.Fatal(err) // NTFS hardlinks need no symlink privilege.
					}
				}
				linkTarget := target
				want := "硬链接"
				if strings.HasPrefix(kind, "hardlink") {
					if err := os.Link(target, overlay); err != nil {
						t.Fatal(err)
					}
				} else {
					want = "重解析点"
					if kind == "dangling symlink" {
						linkTarget = filepath.Join(r.Dir, "missing.qcow2")
					}
					if err := os.Symlink(linkTarget, overlay); err != nil {
						if errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) {
							t.Skipf("account cannot create file symlinks: %v", err)
						}
						t.Fatal(err)
					}
				}
				tmp := filepath.Join(avdDir, userdataCompactTmp)
				if stray {
					if err := os.WriteFile(tmp, []byte("previous compact scratch"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				paths := []string{overlay, target}
				if kind != "hardlink" {
					paths = append(paths, alias)
				}
				before := make([]os.FileInfo, len(paths))
				for k, path := range paths {
					var err error
					before[k], err = os.Lstat(path)
					if err != nil {
						t.Fatal(err)
					}
				}
				entries := compactLinkTree(t, r.Dir)
				res, err := New(r, nil).DiskCompact(context.Background(), 2)
				if res != nil || errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), want) ||
					!strings.Contains(err.Error(), overlay) || errs.As(err).Advice == "" {
					t.Fatalf("DiskCompact = %+v, %v; want unsupported %s with path and advice", res, err, want)
				}
				t.Logf("refused before compact: %v", err)
				if got := compactLinkTree(t, r.Dir); !reflect.DeepEqual(got, entries) {
					t.Errorf("directory entries changed: before=%v after=%v", entries, got)
				}
				for k, path := range paths {
					after, err := os.Lstat(path)
					if err != nil || !os.SameFile(before[k], after) || before[k].Mode() != after.Mode() {
						t.Fatalf("file/link identity changed at %s: %v", path, err)
					}
					if kind == "dangling symlink" && path == overlay {
						continue
					}
					if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
						t.Errorf("content changed at %s: %v", path, err)
					}
				}
				if !strings.HasPrefix(kind, "hardlink") {
					if got, err := os.Readlink(overlay); err != nil || got != linkTarget {
						t.Errorf("symlink target changed: %q, %v", got, err)
					}
				}
				if stray {
					if got, err := os.ReadFile(tmp); err != nil || string(got) != "previous compact scratch" {
						t.Errorf("stray compact temp changed: %q, %v", got, err)
					}
				} else if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
					t.Errorf("compact copy created: %v", err)
				}
			})
		}
	}
}

func compactLinkTree(t *testing.T, dir string) []string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			entries = append(entries, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

type compactReparseInfo struct{ os.FileInfo }

func (st compactReparseInfo) Sys() any {
	attrs := *st.FileInfo.Sys().(*syscall.Win32FileAttributeData)
	attrs.FileAttributes |= syscall.FILE_ATTRIBUTE_REPARSE_POINT
	return &attrs
}

func TestCompactOverlayLinkMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), userdataOverlay)
	if err := os.WriteFile(path, []byte("ordinary overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("ordinary file", func(t *testing.T) {
		if err := checkCompactOverlayLinks(path, st); err != nil {
			t.Fatalf("ordinary file refused: %v", err)
		}
	})
	t.Run("reparse attribute without symlink mode", func(t *testing.T) {
		// Not all Windows reparse tags are reported as ModeSymlink by Go.
		// Supply the raw attribute while retaining an ordinary file's Mode.
		if err := checkCompactOverlayLinks(path, compactReparseInfo{st}); errs.CodeOf(err) != errs.Unsupported ||
			!strings.Contains(err.Error(), "重解析点") {
			t.Fatalf("non-symlink reparse attribute not refused: %v", err)
		}
	})
	t.Run("metadata unavailable", func(t *testing.T) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		// A stale Lstat must not allow compact to proceed if the handle query fails.
		if err := checkCompactOverlayLinks(path, st); errs.CodeOf(err) != errs.Internal ||
			!strings.Contains(err.Error(), path) {
			t.Fatalf("metadata failure not reported: %v", err)
		}
	})
}

func TestDiskCompactMissingOverlay(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(strconv.FormatBool(directory), func(t *testing.T) {
			r := managerTestRoot(t, 2)
			if directory {
				if err := os.Mkdir(filepath.Join(r.AvdHome(), root.AvdName(2)+".avd", userdataOverlay), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := New(r, nil).DiskCompact(context.Background(), 2); res != nil || errs.CodeOf(err) != errs.NotFound {
				t.Fatalf("missing/non-file overlay: %+v, %v", res, err)
			}
		})
	}
}
