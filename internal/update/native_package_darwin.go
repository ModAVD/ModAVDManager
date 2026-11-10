//go:build darwin && cgo

package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modavd/internal/root"
)

const nativeFormat = "modavd-macos-update-v1"
const nativeManifestName = "modavd-update-manifest.json"

type nativeFile struct {
	Type   string            `json:"type"`
	Mode   uint32            `json:"mode,omitempty"`
	Size   int64             `json:"size,omitempty"`
	SHA256 string            `json:"sha256"`
	Target string            `json:"target,omitempty"`
	Xattrs map[string]string `json:"xattrs,omitempty"`
}
type nativeManifest struct {
	Schema   int                   `json:"schema"`
	Format   string                `json:"format"`
	Version  string                `json:"version"`
	Build    int                   `json:"build"`
	Platform string                `json:"platform"`
	Units    []string              `json:"units"`
	Files    map[string]nativeFile `json:"files"`
}

var nativeAllowedUnits = map[string]bool{
	"sdk/emulator": true, "ModAVD.app": true, "tools": true,
	"components": true, "licenses": true, "provenance": true,
	"modavd-manifest.json": true, "PACKAGE-MANIFEST.json": true,
	"BUILD-RECEIPT.json": true, "README.md": true, "README.zh-CN.txt": true,
	"CHANGELOG.md": true, "LICENSE": true, "NOTICE": true, "VERSION": true,
	"SHA256SUMS": true, ".modavd-macos-package.json": true,
}

func nativeDirectoryUnit(p string) bool {
	return p == "sdk/emulator" || p == "ModAVD.app" || p == "tools" || p == "components" || p == "licenses" || p == "provenance"
}

func nativeCleanPath(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "../") {
		return false
	}
	for _, c := range p {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}
func (m *nativeManifest) unit(p string) string {
	for _, u := range m.Units {
		if p == u || strings.HasPrefix(p, u+"/") {
			return u
		}
	}
	return ""
}
func digestBytes(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// validateNativeManifest validates the complete graph before any archive path is written.
// Symlinks may contain '..', but every resolution step stays inside its move unit.
func validateNativeManifest(m *nativeManifest) (Version, error) {
	v, ok := ParseVersion(m.Version)
	if !ok || m.Schema != 1 || m.Format != nativeFormat || m.Platform != root.CurrentPlatform().PackageSuffix || m.Build != v.Build || len(m.Files) == 0 || len(m.Files) > maxEntries || len(m.Units) > len(nativeAllowedUnits) {
		return Version{}, packageError("macOS 更新清单格式、平台或版本不正确")
	}
	units := map[string]bool{}
	for _, u := range m.Units {
		if !nativeAllowedUnits[u] || units[u] {
			return Version{}, packageError("更新包含不允许的单元: %s", u)
		}
		units[u] = true
	}
	for _, u := range []string{"sdk/emulator", "ModAVD.app", "tools", manifestName} {
		if !units[u] {
			return Version{}, packageError("更新缺少单元: %s", u)
		}
	}
	graph := map[string]string{}
	fold := map[string]string{}
	add := func(p, kind string) error {
		key := strings.ToLower(p)
		if old, ok := fold[key]; ok && old != p {
			return packageError("更新存在大小写路径冲突: %s / %s", old, p)
		}
		fold[key] = p
		if old, ok := graph[p]; ok && old != kind {
			return packageError("更新路径类型冲突: %s", p)
		}
		graph[p] = kind
		return nil
	}
	var signatureBytes int
	var unpacked int64
	for p, s := range m.Files {
		if err := validateNativeXattrs(s.Xattrs); err != nil {
			return Version{}, err
		}
		for _, value := range s.Xattrs {
			signatureBytes += len(value)
		}
		if signatureBytes > 12<<20 {
			return Version{}, packageError("签名扩展属性总量太大")
		}
		if !nativeCleanPath(p) || m.unit(p) == "" || !sha256Hex.MatchString(s.SHA256) || s.Mode & ^uint32(0777) != 0 {
			return Version{}, packageError("清单路径或属性不安全: %s", p)
		}
		if s.Type != "file" && s.Type != "symlink" {
			return Version{}, packageError("不支持的文件类型: %s", p)
		}
		if s.Type == "file" && (s.Size < 0 || s.Target != "" || s.Size > maxUnpackedBytes || unpacked > maxUnpackedBytes-s.Size) {
			return Version{}, packageError("文件记录不正确: %s", p)
		}
		if s.Type == "file" {
			unpacked += s.Size
		}
		if s.Type == "symlink" && (s.Size != 0 || s.Mode != 0 || len(s.Target) > 4096 || len(s.Xattrs) != 0 || s.Target == "" || path.IsAbs(s.Target) || strings.ContainsAny(s.Target, "\\\x00\r\n") || digestBytes([]byte(s.Target)) != s.SHA256) {
			return Version{}, packageError("符号链接不安全: %s", p)
		}
		if err := add(p, s.Type); err != nil {
			return Version{}, err
		}
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if err := add(d, "dir"); err != nil {
				return Version{}, err
			}
		}
	}
	for _, u := range m.Units {
		want := "file"
		if nativeDirectoryUnit(u) {
			want = "dir"
		}
		if graph[u] != want {
			return Version{}, packageError("更新单元为空或类型不正确: %s", u)
		}
	}
	for p, spec := range m.Files {
		if spec.Type != "symlink" {
			continue
		}
		unit := m.unit(p)
		stack := strings.Split(unit, "/")
		floor := len(stack)
		todo := strings.Split(strings.TrimPrefix(p, unit+"/"), "/")
		expansions := 0
		for len(todo) > 0 {
			part := todo[0]
			todo = todo[1:]
			if part == "" || part == "." {
				continue
			}
			if part == ".." {
				if len(stack) <= floor {
					return Version{}, packageError("链接越过更新单元: %s", p)
				}
				stack = stack[:len(stack)-1]
				continue
			}
			prefix := strings.Join(append(append([]string{}, stack...), part), "/")
			kind, ok := graph[prefix]
			if !ok {
				return Version{}, packageError("悬空符号链接: %s", p)
			}
			if kind == "symlink" {
				expansions++
				if expansions > 64 {
					return Version{}, packageError("符号链接循环: %s", p)
				}
				todo = append(strings.Split(m.Files[prefix].Target, "/"), todo...)
				continue
			}
			if len(todo) > 0 && kind != "dir" {
				return Version{}, packageError("链接穿过普通文件: %s", p)
			}
			stack = append(stack, part)
		}
	}
	return v, nil
}

func readNativeManifest(p string) (*nativeManifest, error) {
	b, err := nativeReadFile(p, 16<<20)
	if err != nil {
		return nil, err
	}
	if len(b) > 16<<20 {
		return nil, packageError("更新清单太大")
	}
	var m nativeManifest
	if err := decodeNativeJSON(b, &m); err != nil {
		return nil, packageError("更新清单读不懂: %v", err)
	}
	if _, err := validateNativeManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func extractNativePackage(archive, dst, topDir string, check func() error) (*nativeManifest, error) {
	zr, closer, err := openZip(archive)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	if len(zr.File) > maxEntries {
		return nil, packageError("更新包条目太多")
	}
	entries := map[string]*zip.File{}
	fold := map[string]bool{}
	var total uint64
	for _, f := range zr.File {
		name, err := zipName(f)
		if err != nil {
			return nil, err
		}
		if topDir != "" {
			prefix := strings.TrimRight(topDir, "/") + "/"
			if name == prefix {
				continue
			}
			if !strings.HasPrefix(name, prefix) {
				return nil, packageError("更新包目录不正确")
			}
			name = strings.TrimPrefix(name, prefix)
		}
		name = strings.TrimSuffix(name, "/")
		if !nativeCleanPath(name) || fold[strings.ToLower(name)] {
			return nil, packageError("更新包路径不安全或重复: %s", name)
		}
		fold[strings.ToLower(name)] = true
		if f.Mode().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() && f.Mode()&os.ModeSymlink == 0 {
			return nil, packageError("更新包含特殊文件: %s", name)
		}
		if f.UncompressedSize64 > maxUnpackedBytes || total > maxUnpackedBytes-f.UncompressedSize64 {
			return nil, packageError("更新包解开后太大")
		}
		total += f.UncompressedSize64
		entries[name] = f
	}
	mf := entries[nativeManifestName]
	if mf == nil || !mf.Mode().IsRegular() || mf.UncompressedSize64 > 16<<20 {
		return nil, packageError("缺少 macOS 更新清单")
	}
	rc, err := mf.Open()
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 16<<20+1))
	rc.Close()
	if err != nil {
		return nil, err
	}
	var m nativeManifest
	if err := decodeNativeJSON(b, &m); err != nil {
		return nil, packageError("更新清单读不懂: %v", err)
	}
	if _, err := validateNativeManifest(&m); err != nil {
		return nil, err
	}
	if len(entries) != len(m.Files)+1 {
		return nil, packageError("更新包与清单条目不一致")
	}
	for p, s := range m.Files {
		f := entries[p]
		if f == nil {
			return nil, packageError("更新包缺少 %s", p)
		}
		if (f.Mode()&os.ModeSymlink != 0) != (s.Type == "symlink") {
			return nil, packageError("文件类型不符: %s", p)
		}
		if s.Type == "file" && (uint64(s.Size) != f.UncompressedSize64 || uint32(f.Mode().Perm()) != s.Mode) {
			return nil, packageError("文件属性不符: %s", p)
		}
	}
	if err := removeTree(dst); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m.Files))
	for p := range m.Files {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if m.Files[p].Type == "file" {
			if err := nativeExtractEntry(zipEntry{p, entries[p]}, filepath.Join(dst, filepath.FromSlash(p)), check); err != nil {
				return nil, err
			}
			if err := os.Chmod(filepath.Join(dst, filepath.FromSlash(p)), os.FileMode(m.Files[p].Mode)); err != nil {
				return nil, err
			}
			if err := restoreNativeXattrs(filepath.Join(dst, filepath.FromSlash(p)), m.Files[p].Xattrs); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range keys {
		s := m.Files[p]
		if s.Type != "symlink" {
			continue
		}
		f := entries[p]
		if f.UncompressedSize64 > 4096 {
			return nil, packageError("链接目标太长: %s", p)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 4097))
		rc.Close()
		if err != nil || string(b) != s.Target {
			return nil, packageError("链接内容不符: %s", p)
		}
		dstPath := filepath.Join(dst, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return nil, err
		}
		if err := os.Symlink(s.Target, dstPath); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dst, nativeManifestName), b, 0644); err != nil {
		return nil, err
	}
	return &m, nil
}

// verifyNativeTree uses Lstat/WalkDir: it never follows archive links while hashing.
func verifyNativeTree(base string, m *nativeManifest) error {
	return verifyNativeTreeAt(m, func(p string) string { return filepath.Join(base, filepath.FromSlash(p)) })
}

func verifyNativeTreeAt(m *nativeManifest, at func(string) string) error {
	if _, err := validateNativeManifest(m); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, unit := range m.Units {
		unitPath := at(unit)
		err := filepath.WalkDir(unitPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(unitPath, p)
			if err != nil {
				return err
			}
			if rel == "." {
				rel = unit
			} else {
				rel = unit + "/" + filepath.ToSlash(rel)
			}
			s, ok := m.Files[rel]
			if !ok {
				return packageError("清单未列出的文件: %s", rel)
			}
			seen[rel] = true
			info, err := d.Info()
			if err != nil {
				return err
			}
			if s.Type == "symlink" {
				if info.Mode()&os.ModeSymlink == 0 {
					return packageError("链接被替换: %s", rel)
				}
				target, err := os.Readlink(p)
				if err != nil || target != s.Target {
					return packageError("链接内容不符: %s", rel)
				}
				return nil
			}
			if !info.Mode().IsRegular() || info.Size() != s.Size || uint32(info.Mode().Perm()) != s.Mode {
				return packageError("文件属性不符: %s", rel)
			}
			sum, err := nativeHashFile(p)
			if err != nil || sum != s.SHA256 {
				return packageError("文件内容不符: %s", rel)
			}
			attrs, err := nativeXattrs(p)
			if err != nil || len(attrs) != len(s.Xattrs) {
				return packageError("签名扩展属性不符: %s", rel)
			}
			for key, value := range attrs {
				if s.Xattrs[key] != value {
					return packageError("签名扩展属性不符: %s", rel)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(seen) != len(m.Files) {
		return packageError("更新文件缺失")
	}
	return nil
}

func arm64MachO(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	var magic [4]byte
	n, err := f.Read(magic[:])
	f.Close()
	if n < 4 {
		return false, nil
	}
	m := binary.BigEndian.Uint32(magic[:])
	if m != macho.Magic32 && m != macho.Magic64 && m != 0xcefaedfe && m != 0xcffaedfe && m != macho.MagicFat && m != 0xbebafeca {
		return false, nil
	}
	if fat, err := macho.OpenFat(p); err == nil {
		defer fat.Close()
		for _, a := range fat.Arches {
			if a.Cpu == macho.CpuArm64 {
				return true, nil
			}
		}
		return true, packageError("Mach-O 缺少 ARM64: %s", p)
	}
	thin, err := macho.Open(p)
	if err != nil {
		return true, packageError("Mach-O 损坏: %s", p)
	}
	defer thin.Close()
	if thin.Cpu != macho.CpuArm64 {
		return true, packageError("Mach-O 不是 ARM64: %s", p)
	}
	return true, nil
}

func verifyDarwinPackage(base string, m *nativeManifest, target Version) error {
	return verifyDarwinPackageContext(context.Background(), base, m, target)
}

func verifyDarwinPackageContext(ctx context.Context, base string, m *nativeManifest, target Version) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return verifyDarwinPackageAtContext(ctx, m, target, func(p string) string { return filepath.Join(base, filepath.FromSlash(p)) })
}

func verifyDarwinPackageAt(m *nativeManifest, target Version, at func(string) string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return verifyDarwinPackageAtContext(ctx, m, target, at)
}

func verifyDarwinPackageAtContext(ctx context.Context, m *nativeManifest, target Version, at func(string) string) error {
	if err := verifyNativeTreeAt(m, at); err != nil {
		return err
	}
	if err := checkEmulator(at("sdk/emulator"), target); err != nil {
		return err
	}
	required := append(managerPaths(), "sdk/emulator/"+root.CurrentPlatform().Emulator, "sdk/emulator/qemu/"+root.CurrentPlatform().QemuDir+"/"+root.CurrentPlatform().QEMU, "sdk/emulator/qemu/"+root.CurrentPlatform().QemuDir+"/"+root.CurrentPlatform().QEMUHeadless)
	machos := map[string]bool{}
	for p, s := range m.Files {
		if ctx.Err() != nil {
			return packageError("签名校验已取消或超时: %v", ctx.Err())
		}
		if s.Type != "file" {
			continue
		}
		full := at(p)
		native, err := arm64MachO(full)
		if err != nil {
			return err
		}
		if native {
			machos[p] = true
			if out, err := nativeCommandOutput(ctx, "/usr/bin/codesign", "--verify", "--strict", full); err != nil {
				return packageError("原生文件签名核对失败 %s: %s", p, strings.TrimSpace(string(out)))
			}
		}
	}
	for _, p := range required {
		if !machos[p] || m.Files[p].Mode&0o111 == 0 {
			return packageError("必需文件不是真实 ARM64 Mach-O: %s", p)
		}
	}
	info := at("ModAVD.app/Contents/Info.plist")
	out, err := nativeCommandOutput(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :CFBundleExecutable", info)
	if err != nil || strings.TrimSpace(string(out)) != "ModAVD" {
		return packageError("应用包 CFBundleExecutable 不正确")
	}
	for _, p := range []string{"ModAVD.app", "sdk/emulator"} {
		out, err := nativeCommandOutput(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", at(p))
		if err != nil {
			return packageError("签名完整性核对失败 %s: %s", p, strings.TrimSpace(string(out)))
		}
	}
	installed, ok := manifestVersion(filepath.Dir(at(manifestName)))
	if !ok || installed != target {
		return packageError("载荷版本清单与更新版本不一致")
	}
	return nil
}

type nativeCommandBuffer struct{ data []byte }

func (b *nativeCommandBuffer) Write(p []byte) (int, error) {
	if left := (32 << 10) - len(b.data); left > 0 {
		b.data = append(b.data, p[:min(left, len(p))]...)
	}
	return len(p), nil
}

func nativeCommandOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.WaitDelay = time.Second
	var output nativeCommandBuffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.data, err
}
