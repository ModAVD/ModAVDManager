//go:build windows

package update

// The on-disk protocol under <root>\.update\, shared with AUTO-MAS's own ModAVD updater
// (app/utils/common/emulator2/avd/update_stage.py), so that either program can finish or undo a swap the other one
// was cut off in. Paths, state.json keys and values, the staged.json fingerprint and the order of state writes
// relative to each rename are the same:
//
//	.update\state.json          phase and swap plan (written to state.json.tmp, then renamed over it)
//	.update\download\<asset>    the package (.part while downloading)
//	.update\pkg\                the package unpacked; after the stage is built, every package file but sdk\emulator
//	.update\stage\emulator\     the new sdk\emulator; stage\staged.json records its version and fingerprint
//	.update\previous\           during a swap: emulator\ (the old sdk\emulator) and root\ (copies of replaced files)
//	.update\trash\              directories renamed here first, deleted once the state says so
//
// Outside .update the updater writes sdk\emulator (renamed as a whole), the package's other files at the root, and
// mas-avd.json (components.emulator and updateHistory only). Everything else in the root is never touched.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

const (
	updateDirName = ".update"
	manifestName  = "modavd-manifest.json"
	platform      = "win-x64"

	phaseIdle      = "idle"
	phasePreparing = "preparing"
	phaseStaged    = "staged"
	phaseSwapping  = "swapping"

	stepBegin    = "begin"
	stepOldMoved = "old_moved"
	stepNewMoved = "new_moved"
)

// layout is the paths of one root.
type layout struct{ root string }

func (l layout) dir() string              { return filepath.Join(l.root, updateDirName) }
func (l layout) state() string            { return filepath.Join(l.dir(), "state.json") }
func (l layout) download() string         { return filepath.Join(l.dir(), "download") }
func (l layout) pkg() string              { return filepath.Join(l.dir(), "pkg") }
func (l layout) stage() string            { return filepath.Join(l.dir(), "stage") }
func (l layout) stageEmulator() string    { return filepath.Join(l.stage(), "emulator") }
func (l layout) stagedMarker() string     { return filepath.Join(l.stage(), "staged.json") }
func (l layout) previous() string         { return filepath.Join(l.dir(), "previous") }
func (l layout) previousEmulator() string { return filepath.Join(l.previous(), "emulator") }
func (l layout) previousRoot() string     { return filepath.Join(l.previous(), "root") }
func (l layout) trash() string            { return filepath.Join(l.dir(), "trash") }
func (l layout) lock() string             { return filepath.Join(l.dir(), "modavd.lock") }
func (l layout) emulator() string         { return filepath.Join(l.root, "sdk", "emulator") }
func (l layout) at(rel string) string     { return filepath.Join(l.root, filepath.FromSlash(rel)) }
func (l layout) pkgAt(rel string) string  { return filepath.Join(l.pkg(), filepath.FromSlash(rel)) }
func (l layout) prevAt(rel string) string {
	return filepath.Join(l.previousRoot(), filepath.FromSlash(rel))
}
func (l layout) downloadAt(n string) string { return filepath.Join(l.download(), n) }

// ---- state.json

// readState is state.json as an object (empty when missing, unreadable or not an object); phase defaults to idle.
func (l layout) readState() *jsonord.Obj {
	b, err := root.ReadFileShared(l.state())
	if err == nil {
		if v, err := jsonord.Parse(trimBOM(b)); err == nil && v.Kind() == jsonord.Object {
			return v.Obj()
		}
	}
	return jsonord.NewObject().Obj()
}

func phaseOf(st *jsonord.Obj) string {
	if p, ok := st.Get("phase").Str(); ok && p != "" {
		return p
	}
	return phaseIdle
}

// writeState stamps updatedAt (local time, ISO seconds) and writes st atomically, as AUTO-MAS does (indent 2,
// non-ASCII raw, CRLF, no BOM). The temp file is flushed before the rename, which AUTO-MAS does not do.
func (l layout) writeState(st *jsonord.Obj) error {
	st.Set("updatedAt", jsonord.NewString(root.NowISO()))
	return l.writeJSON(l.state(), st)
}

func (l layout) writeJSON(path string, o *jsonord.Obj) error {
	v := jsonord.NewObject()
	for _, k := range o.Keys() {
		v.Obj().Set(k, o.Get(k))
	}
	data := []byte(strings.ReplaceAll(string(v.MarshalPython()), "\n", "\r\n"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	return root.ReplaceWithRetry(tmp, path)
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// newState is a fresh state object: {phase, ...pairs in order}.
func newState(phase string, pairs ...any) *jsonord.Obj {
	o := jsonord.NewObject().Obj()
	o.Set("phase", jsonord.NewString(phase))
	for k := 0; k+1 < len(pairs); k += 2 {
		o.Set(pairs[k].(string), pairs[k+1].(*jsonord.Value))
	}
	return o
}

// objVal wraps an *Obj (nil = JSON null) as a value.
func objVal(o *jsonord.Obj) *jsonord.Value {
	if o == nil {
		return jsonord.NewNull()
	}
	v := jsonord.NewObject()
	for _, k := range o.Keys() {
		v.Obj().Set(k, o.Get(k))
	}
	return v
}

func strs(items []string) *jsonord.Value {
	vals := make([]*jsonord.Value, len(items))
	for k, s := range items {
		vals[k] = jsonord.NewString(s)
	}
	return jsonord.NewArray(vals...)
}

func strList(v *jsonord.Value) []string {
	var out []string
	for _, it := range v.Items() {
		if s, ok := it.Str(); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---- the stage fingerprint (staged.json)

// fingerprint is AUTO-MAS staged_fingerprint: for every regular file under stage\emulator (as "sdk/emulator/<rel>")
// and pkg (as "<rel>"), "<path>\0<size>\0<mtime ns>", sorted, joined by "\n", SHA-256 in lower hex. Any edit,
// deletion or partial file changes it; renames of the trees do not.
func (l layout) fingerprint() (string, error) {
	var lines []string
	for _, t := range []struct{ dir, prefix string }{{l.stageEmulator(), "sdk/emulator/"}, {l.pkg(), ""}} {
		if !isDir(t.dir) {
			continue
		}
		err := walkFiles(t.dir, func(rel string, info fs.FileInfo) {
			lines = append(lines, t.prefix+rel+"\x00"+strconv.FormatInt(info.Size(), 10)+"\x00"+
				strconv.FormatInt(info.ModTime().UnixNano(), 10))
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// walkFiles calls fn for every regular file under dir with its "/"-joined path relative to dir. Directory links are
// not followed (a zip never makes any: links are refused).
func walkFiles(dir string, fn func(rel string, info fs.FileInfo)) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(rel), info)
		return nil
	})
}

// ---- small file helpers

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// rename is os.rename (fails when the target exists).
func rename(from, to string) error {
	if lexists(to) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	return os.Rename(from, to)
}

// replaceFile moves src over dst (AUTO-MAS replace_file): clears dst's read-only bit, creates dst's parent when
// missing, one os.Rename (MoveFileEx with MOVEFILE_REPLACE_EXISTING).
func replaceFile(src, dst string) error {
	if st, err := os.Lstat(dst); err == nil && st.Mode()&0o200 == 0 {
		os.Chmod(dst, 0o666)
	}
	err := os.Rename(src, dst)
	if errors.Is(err, fs.ErrNotExist) && isFile(src) {
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr == nil {
			err = os.Rename(src, dst)
		}
	}
	return err
}

// copyFile copies content, then the modification time (shutil.copy2 as far as it matters here).
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := root.OpenShared(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = out.ReadFrom(in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// toTrash renames p (when it exists) into .update\trash under a unique name.
func (l layout) toTrash(p string) error {
	if !lexists(p) {
		return nil
	}
	if err := os.MkdirAll(l.trash(), 0o755); err != nil {
		return err
	}
	return os.Rename(p, filepath.Join(l.trash(), fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(p))))
}

// purgeTrash deletes .update\trash best effort, never following links or junctions; what cannot go (a file still
// open somewhere) stays for the next time.
func (l layout) purgeTrash() {
	if lexists(l.trash()) {
		removeBestEffort(l.trash())
	}
}

const fileAttributeReparsePoint = 0x400

// removeBestEffort removes p and, for a real directory, its contents first; a link or junction is removed as the link
// itself. Reports whether p is gone.
func removeBestEffort(p string) bool {
	st, err := os.Lstat(p)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok && sys.FileAttributes&fileAttributeReparsePoint != 0 {
		if os.Remove(p) != nil {
			return syscall.RemoveDirectory(mustUTF16(p)) == nil
		}
		return true
	}
	if st.IsDir() {
		entries, _ := os.ReadDir(p)
		all := true
		for _, e := range entries {
			if !removeBestEffort(filepath.Join(p, e.Name())) {
				all = false
			}
		}
		if !all {
			return false
		}
	}
	if err := os.Remove(p); err != nil {
		os.Chmod(p, 0o666)
		return os.Remove(p) == nil
	}
	return true
}

func mustUTF16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// removeTree empties and removes dir (gone already is fine); links inside are not followed.
func removeTree(dir string) error {
	if !lexists(dir) {
		return nil
	}
	if !removeBestEffort(dir) {
		return fmt.Errorf("%s 删不干净（还有文件被占用）", dir)
	}
	return nil
}
