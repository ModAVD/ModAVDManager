//go:build windows

package update

// The package: path rules, unpacking (full or MirrorChyan incremental), the manifest check, the stage.
// AUTO-MAS update_stage.py package_path / package_entries / extract_package / build_patched_package /
// verify_package / build_stage, with the same messages.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

const (
	maxEntries       = 20000
	maxUnpackedBytes = 4 << 30
)

// protectedTop are the root's own top-level names a package must never contain (instances, downloads, logs, the
// updater's own directory, caches); besides those, mas-avd.json and anything under sdk\ but sdk\emulator.
var protectedTop = map[string]bool{"avd": true, "downloads": true, "logs": true, ".update": true,
	"shader-cache": true, "guest-patches": true}

var reservedNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for k := 1; k <= 9; k++ {
		reservedNames[fmt.Sprintf("com%d", k)] = true
		reservedNames[fmt.Sprintf("lpt%d", k)] = true
	}
}

// packageError is a package that must not be installed (bad paths, bad zip, verification failed).
func packageError(format string, args ...any) *errs.Error {
	return errs.Newf(errs.UpdateRejected, format, args...)
}

// packagePath is AUTO-MAS package_path: the "/"-joined relative path of a file entry, "" for a directory entry (and
// the bare sdk/ entry), an error for anything unsafe or protected.
func packagePath(name, topDir string) (string, error) {
	raw := strings.ReplaceAll(name, `\`, "/")
	if topDir != "" {
		prefix := strings.TrimRight(topDir, "/") + "/"
		if raw == prefix {
			return "", nil
		}
		if !strings.HasPrefix(raw, prefix) {
			return "", packageError("包里的 %s 不在顶层目录 %s 下", name, topDir)
		}
		raw = raw[len(prefix):]
	}
	if strings.HasPrefix(raw, "/") || strings.Trim(raw, "/") == "" {
		return "", packageError("包里有不安全的路径: %q", name)
	}
	isDir := strings.HasSuffix(raw, "/")
	parts := strings.Split(strings.TrimRight(raw, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || part != strings.TrimRight(part, ". ") ||
			strings.ContainsAny(part, `<>:"|?*`) || reservedNames[strings.ToLower(strings.Split(part, ".")[0])] {
			return "", packageError("包里有不安全的路径: %q", name)
		}
		for _, r := range part {
			if r < 32 {
				return "", packageError("包里有不安全的路径: %q", name)
			}
		}
	}
	rel := strings.Join(parts, "/")
	first := strings.ToLower(parts[0])
	if protectedTop[first] || strings.EqualFold(rel, "mas-avd.json") ||
		(first == "sdk" && (len(parts) < 2 || !strings.EqualFold(parts[1], "emulator"))) {
		if isDir && first == "sdk" && len(parts) == 1 {
			return "", nil
		}
		return "", packageError("包里有不该由更新覆盖的文件: %s", rel)
	}
	if isDir {
		return "", nil
	}
	return rel, nil
}

// zipEntry is one file of the package.
type zipEntry struct {
	rel string
	f   *zip.File
}

// zipName is the entry's name as text: UTF-8 when flagged, otherwise CP437 like Python's zipfile (the release zips
// carry the flag; this only keeps an unflagged ASCII name the same and refuses other bytes).
func zipName(f *zip.File) (string, error) {
	if f.NonUTF8 {
		for k := 0; k < len(f.Name); k++ {
			if f.Name[k] >= 0x80 {
				return "", packageError("包里有不是 UTF-8 编码的文件名: %q", f.Name)
			}
		}
	}
	if !utf8.ValidString(f.Name) {
		return "", packageError("包里有不是 UTF-8 编码的文件名: %q", f.Name)
	}
	return f.Name, nil
}

// packageEntries is AUTO-MAS package_entries: the file entries keyed by lower-case path. whole: a full package must
// hold sdk/emulator.
func packageEntries(zr *zip.Reader, topDir string, whole bool) (map[string]zipEntry, error) {
	if len(zr.File) > maxEntries {
		return nil, packageError("包里的条目太多（%d 个）", len(zr.File))
	}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
	}
	if total > maxUnpackedBytes {
		return nil, packageError("包解开后太大")
	}
	out := map[string]zipEntry{}
	emulator := false
	for _, f := range zr.File {
		name, err := zipName(f)
		if err != nil {
			return nil, err
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, packageError("包里有符号链接: %s", name)
		}
		rel, err := packagePath(name, topDir)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			continue
		}
		key := strings.ToLower(rel)
		if _, dup := out[key]; dup {
			return nil, packageError("包里有重名的文件: %s", rel)
		}
		out[key] = zipEntry{rel, f}
		if strings.HasPrefix(key, "sdk/emulator/") {
			emulator = true
		}
	}
	if whole && !emulator {
		return nil, packageError(`包里没有 sdk\emulator`)
	}
	return out, nil
}

// extractEntry writes one entry to dst, reading it to the end so the CRC is checked.
func extractEntry(e zipEntry, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := e.f.Open()
	if err != nil {
		return packageError("包里的 %s 打不开: %v", e.rel, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, rc)
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		os.Remove(dst)
		if os.IsPermission(cerr) || isDiskError(cerr) {
			return cerr
		}
		return packageError("包里的 %s 解不开（包可能坏了）: %v", e.rel, cerr)
	}
	return nil
}

func isDiskError(err error) bool {
	_, ok := err.(*os.PathError)
	return ok
}

// extractPackage unpacks a full package into pkg\; the entry count.
func (l layout) extractPackage(archive, topDir string, onEntry func(done, total int)) (int, error) {
	zr, closer, err := openZip(archive)
	if err != nil {
		return 0, err
	}
	defer closer.Close()
	entries, err := packageEntries(zr, topDir, true)
	if err != nil {
		return 0, err
	}
	if err := removeTree(l.pkg()); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(l.pkg(), 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range sortedEntries(entries) {
		if err := extractEntry(e, l.pkgAt(e.rel)); err != nil {
			return 0, err
		}
		n++
		if onEntry != nil {
			onEntry(n, len(entries))
		}
	}
	return n, nil
}

func sortedEntries(m map[string]zipEntry) []zipEntry {
	out := make([]zipEntry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].rel < out[b].rel })
	return out
}

type zipCloser struct{ f *os.File }

func (c zipCloser) Close() error { return c.f.Close() }

func openZip(archive string) (*zip.Reader, io.Closer, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, nil, packageError("下载的包不是有效的 zip: %v", err)
	}
	return zr, zipCloser{f}, nil
}

// ---- MirrorChyan incremental packages

type patchChanges struct {
	added, modified, deleted []string
}

// readPatchChanges is AUTO-MAS read_patch_changes: changes.json of an incremental package.
func readPatchChanges(e zipEntry) (patchChanges, error) {
	var pc patchChanges
	rc, err := e.f.Open()
	if err != nil {
		return pc, packageError("增量包的 changes.json 读不懂")
	}
	b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	rc.Close()
	if err != nil {
		return pc, packageError("增量包的 changes.json 读不懂")
	}
	var raw any
	if json.Unmarshal(trimBOM(b), &raw) != nil {
		return pc, packageError("增量包的 changes.json 读不懂")
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return pc, packageError("增量包的 changes.json 不是对象")
	}
	list := func(key string, files bool) ([]string, error) {
		v, present := obj[key]
		if !present || v == nil {
			return nil, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, packageError("增量包的 changes.json 里 %s 写错了", key)
		}
		var out []string
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, packageError("增量包的 changes.json 里 %s 写错了", key)
			}
			if !files {
				if _, err := packagePath(strings.TrimRight(strings.ReplaceAll(s, `\`, "/"), "/")+"/", ""); err != nil {
					return nil, err
				}
				continue
			}
			rel, err := packagePath(s, "")
			if err != nil {
				return nil, err
			}
			if rel == "" {
				return nil, packageError("增量包的 changes.json 里 %s 有目录: %q", key, s)
			}
			out = append(out, rel)
		}
		return out, nil
	}
	if pc.added, err = list("added", true); err != nil {
		return pc, err
	}
	if pc.modified, err = list("modified", true); err != nil {
		return pc, err
	}
	if pc.deleted, err = list("deleted", true); err != nil {
		return pc, err
	}
	for _, k := range []string{"added_dir", "deleted_dir"} {
		if _, err := list(k, false); err != nil {
			return pc, err
		}
	}
	return pc, nil
}

// patchBaseOK is AUTO-MAS patch_base_ok: the root's manifest says base and is a valid manifest, so an incremental
// package from base can be applied here.
func patchBaseOK(r *root.Root, base Version) bool {
	m, ok := readManifest(filepath.Join(r.Dir, manifestName))
	if !ok {
		return false
	}
	v, ok := ParseVersion(manifestString(m, "version"))
	if !ok || v != base {
		return false
	}
	_, err := manifestFiles(m)
	return err == nil
}

// buildPatchedPackage is AUTO-MAS build_patched_package: pkg\ = the base files from the root (by the root manifest)
// that the patch does not touch, plus the patch's added and modified files. Deleted files are just not copied.
func (l layout) buildPatchedPackage(r *root.Root, archive string, base Version) (int, error) {
	if !patchBaseOK(r, base) {
		return 0, packageError("本地装着的不是增量包的起点 %s（或没有清单）", base.Display())
	}
	m, _ := readManifest(filepath.Join(r.Dir, manifestName))
	files, _ := manifestFiles(m)
	baseFiles := []string{manifestName}
	for rel := range files {
		baseFiles = append(baseFiles, rel)
	}
	zr, closer, err := openZip(archive)
	if err != nil {
		return 0, err
	}
	defer closer.Close()
	if err := removeTree(l.pkg()); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(l.pkg(), 0o755); err != nil {
		return 0, err
	}
	entries, err := packageEntries(zr, "", false)
	if err != nil {
		return 0, err
	}
	ce, ok := entries["changes.json"]
	if !ok {
		return 0, packageError("增量包里没有 changes.json")
	}
	delete(entries, "changes.json")
	pc, err := readPatchChanges(ce)
	if err != nil {
		return 0, err
	}
	wanted := append(append([]string{}, pc.added...), pc.modified...)
	var missing []string
	for _, rel := range wanted {
		if _, ok := entries[strings.ToLower(rel)]; !ok {
			missing = append(missing, rel)
		}
	}
	if len(missing) > 0 {
		more := ""
		if len(missing) > 5 {
			more = fmt.Sprintf(" 等 %d 个", len(missing))
			missing = missing[:5]
		}
		return 0, packageError("增量包里缺 %s%s", strings.Join(missing, "、"), more)
	}
	touched := map[string]bool{}
	for _, rel := range append(append([]string{}, wanted...), pc.deleted...) {
		touched[strings.ToLower(rel)] = true
	}
	n := 0
	for _, rel := range baseFiles {
		if touched[strings.ToLower(rel)] || !isFile(l.at(rel)) {
			continue
		}
		if err := copyFile(l.at(rel), l.pkgAt(rel)); err != nil {
			return 0, err
		}
		n++
	}
	for _, rel := range wanted {
		e := entries[strings.ToLower(rel)]
		if err := extractEntry(e, l.pkgAt(e.rel)); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// ---- the manifest

func readManifest(path string) (*jsonord.Obj, bool) {
	b, err := root.ReadFileShared(path)
	if err != nil {
		return nil, false
	}
	v, err := jsonord.Parse(trimBOM(b))
	if err != nil || v.Kind() != jsonord.Object || v.Obj().Len() == 0 {
		return nil, false
	}
	return v.Obj(), true
}

func manifestString(m *jsonord.Obj, key string) string {
	s, _ := m.Get(key).Str()
	return s
}

type fileSpec struct {
	size   int64
	sha256 string
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// manifestFiles is AUTO-MAS manifest_files: the "files" table, every key already a clean package path.
func manifestFiles(m *jsonord.Obj) (map[string]fileSpec, error) {
	files := m.Get("files")
	if files.Kind() != jsonord.Object || files.Obj().Len() == 0 {
		return nil, packageError("清单里没有文件列表")
	}
	out := map[string]fileSpec{}
	for _, rel := range files.Obj().Keys() {
		e := files.Obj().Get(rel)
		size, okSize := e.Obj().Get("size").Int()
		sum, okSum := e.Obj().Get("sha256").Str()
		sum = strings.ToLower(sum)
		if e.Kind() != jsonord.Object || !okSize || size < 0 || !okSum || !sha256Hex.MatchString(sum) {
			return nil, packageError("清单里有写错的条目: %q", rel)
		}
		if clean, err := packagePath(rel, ""); err != nil || clean != rel {
			return nil, packageError("清单里有写错的条目: %q", rel)
		}
		out[rel] = fileSpec{size, sum}
	}
	return out, nil
}

// verifyTree is AUTO-MAS verify_tree: every file under tree (but the root manifest) against the manifest, both ways.
func verifyTree(tree string, files map[string]fileSpec) ([]string, error) {
	want := map[string]string{}
	for rel := range files {
		want[strings.ToLower(rel)] = rel
	}
	var problems []string
	seen := map[string]bool{}
	err := walkFiles(tree, func(rel string, info os.FileInfo) {
		if strings.EqualFold(rel, manifestName) {
			return
		}
		key := strings.ToLower(rel)
		seen[key] = true
		mrel, ok := want[key]
		if !ok {
			problems = append(problems, "清单里没有 "+rel)
			return
		}
		spec := files[mrel]
		if info.Size() != spec.size {
			problems = append(problems, rel+" 大小不对")
			return
		}
		if sum, err := hashFile(filepath.Join(tree, filepath.FromSlash(rel))); err != nil || sum != spec.sha256 {
			problems = append(problems, rel+" 内容与清单不符")
		}
	})
	if err != nil {
		return nil, err
	}
	for key, rel := range want {
		if !seen[key] {
			problems = append(problems, "缺 "+rel)
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func hashFile(p string) (string, error) {
	f, err := root.OpenShared(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkPackageManifest is AUTO-MAS check_package_manifest: version, build and platform match what is being installed.
func checkPackageManifest(m *jsonord.Obj, target Version) error {
	v, ok := ParseVersion(manifestString(m, "version"))
	if !ok || v != target {
		return packageError("包里清单的版本（%s）和要装的版本（%s）不一致", displayVersion(manifestString(m, "version")), target.Display())
	}
	if b := m.Get("build"); b.Kind() != jsonord.Null {
		n, ok := b.Int()
		if !ok || int(n) != target.Build {
			lit, _ := b.NumberLiteral()
			return packageError("包里清单的构建号（%s）和版本 %s 的构建号（%d）不一致", lit, target.Display(), target.Build)
		}
	}
	p, ok := m.Get("platform").Str()
	if !ok || strings.TrimSpace(p) == "" {
		return packageError("包里清单没写 platform，确认不了是 %s 的包，不装", platform)
	}
	if strings.ToLower(strings.TrimSpace(p)) != platform {
		return packageError("这是 %s 平台的包，本机要的是 %s，不装", p, platform)
	}
	return nil
}

var buildIDRe = regexp.MustCompile(`^mas-([0-9]+)$`)

// checkEmulator is AUTO-MAS check_emulator: the package's sdk\emulator is complete, its Pkg.BuildId matches, and its
// qemu is our own build (it knows the free-page-reporting balloon option).
func checkEmulator(emu string, target Version) error {
	if !isFile(filepath.Join(emu, "emulator.exe")) ||
		!isFile(filepath.Join(emu, "qemu", "windows-x86_64", "qemu-system-x86_64.exe")) {
		return packageError(`包里的 sdk\emulator 不完整（缺 emulator.exe 或 qemu）`)
	}
	build := "读不到"
	if b, err := os.ReadFile(filepath.Join(emu, "source.properties")); err == nil {
		id := ""
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "Pkg.BuildId" {
				id = strings.TrimSpace(v)
			}
		}
		if m := buildIDRe.FindStringSubmatch(id); m != nil {
			build = m[1]
		}
	}
	if build != fmt.Sprint(target.Build) {
		return packageError("包里模拟器的构建号（%s）和要装的 %s（构建号 %d）不一致", build, target.Display(), target.Build)
	}
	if !fileContains(filepath.Join(emu, "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe"), []byte("free-page-reporting")) {
		return packageError("包里的模拟器不是魔改 AVD 的自编版")
	}
	return nil
}

// fileContains scans p in 4 MiB chunks (with overlap) for needle; false when unreadable.
func fileContains(p string, needle []byte) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4<<20)
	carry := []byte{}
	for {
		n, err := f.Read(buf)
		chunk := append(carry, buf[:n]...)
		if bytes.Contains(chunk, needle) {
			return true
		}
		if len(chunk) >= len(needle) {
			carry = append([]byte{}, chunk[len(chunk)-len(needle)+1:]...)
		} else {
			carry = chunk
		}
		if err != nil {
			return false
		}
	}
}

// verifyPackage is AUTO-MAS verify_package: "manifest" when pkg\ matches its manifest, "zip" for a legacy package
// without one (only its zip CRC and download hash vouch for it).
func (l layout) verifyPackage(target Version, tag string) (string, error) {
	m, ok := readManifest(l.pkgAt(manifestName))
	verified := "zip"
	switch {
	case !ok && requiresManifest(tag):
		return "", packageError("%s 的包里没有 %s，不装", tag, manifestName)
	case ok:
		if err := checkPackageManifest(m, target); err != nil {
			return "", err
		}
		files, err := manifestFiles(m)
		if err != nil {
			return "", err
		}
		problems, err := verifyTree(l.pkg(), files)
		if err != nil {
			return "", err
		}
		if len(problems) > 0 {
			more := ""
			if len(problems) > 5 {
				more = fmt.Sprintf(" 等 %d 项", len(problems))
				problems = problems[:5]
			}
			return "", packageError("包里的文件和清单对不上：%s%s", strings.Join(problems, "；"), more)
		}
		verified = "manifest"
	}
	if err := checkEmulator(l.pkgAt("sdk/emulator"), target); err != nil {
		return "", err
	}
	return verified, nil
}

// buildStage moves pkg\sdk\emulator to .update\stage\emulator.
func (l layout) buildStage(target Version) error {
	if err := checkEmulator(l.pkgAt("sdk/emulator"), target); err != nil {
		return err
	}
	if err := removeTree(l.stage()); err != nil {
		return err
	}
	if err := os.MkdirAll(l.stage(), 0o755); err != nil {
		return err
	}
	return os.Rename(l.pkgAt("sdk/emulator"), l.stageEmulator())
}

// countFiles is the number of regular files under dir.
func countFiles(dir string) int {
	n := 0
	walkFiles(dir, func(string, os.FileInfo) { n++ })
	return n
}
