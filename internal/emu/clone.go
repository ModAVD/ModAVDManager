package emu

// modavd clone <i> (ModAVD/ModAVD#239): a new instance that is a full copy of a stopped one, with its games, logins
// and settings. The new one gets the next free index (so its own ports and adb serial); everything else is the
// source's: its config.ini (memory, cores, frame rate, resolution, sound ...) and its part of mas-avd.json
// (language, time zone, bridge, data disk mode ...), less what belongs to one instance only (creation state,
// diagnostics, boss key: the copy gets the default of its index).
//
// The disks are copied whole, so the two never share a file. Inside an instance the data disk is a qcow2 overlay over
// a base image next to it, named relatively (userdata-qemu.img.qcow2 -> userdata-qemu.img): copying the directory
// keeps that chain inside the copy. A backing named by an absolute path inside the source directory is renamed to the
// relative name in the copy (qemu-img rebase -u: same bytes, only the header changes). The system and vendor
// overlays sit on the root's read-only system image like every instance's (the bridge keeps that path right,
// bridge_backing.go); any other disk whose backing is outside the source directory is flattened into a standalone
// image (qemu-img convert), so the copy never depends on someone else's file.
//
// Not copied: snapshots (every boot is cold), the emulator's per-launch files (hardware-qemu.ini and
// emu-launch-params.txt, written again at each launch with the instance's own paths), lock files and leftovers of an
// interrupted compact. The device identity inside the guest (Android ID, GSF ID) is copied as it is: changing it
// would log the games out, which is what the copy is for.
//
// Before anything is written the volume must have room for the copy plus cloneHeadroom; a failed or canceled clone
// removes the half-made directory, its metadata and its lock files, so nothing of it is listed or left behind.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/avd"
	"modavd/internal/bosskey"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// CloneRequest is `modavd clone <i>`'s options.
type CloneRequest struct {
	Title    string // "" = the new AVD name, like create
	Progress func(Progress)
}

// CloneResult is the new instance and where it came from.
type CloneResult struct {
	Source      int          `json:"source"`
	Instance    InstanceInfo `json:"instance"`
	CopiedBytes int64        `json:"copied_bytes"`
}

// cloneHeadroom is kept free on the volume beyond the copy itself: a volume filled to the last byte would stall the
// instances running from it.
const cloneHeadroom = 2 << 30

// cloneQemuImgTimeout bounds one qemu-img info / rebase; a convert (flattening) gets cloneConvertTimeout.
const (
	cloneQemuImgTimeout = 2 * time.Minute
	cloneConvertTimeout = 3 * time.Hour
)

// Hooks for the tests: qemu-img (a fake answers in tests; nothing real is started) and the free space.
var (
	cloneRunQemuImg = (*Manager).runQemuImg
	cloneDiskFree   = func(path string) (uint64, error) { return hostDiskFree(path) }
)

// cloneSkipDirs and cloneSkipFiles are left out of the copy (names inside the instance directory, any case).
var (
	cloneSkipDirs  = []string{"snapshots", "tmpAdbCmds"}
	cloneSkipFiles = []string{"hardware-qemu.ini", "emu-launch-params.txt", userdataCompactTmp}
)

// cloneMetaDrop are the keys of instances.<i> that belong to the source alone.
var cloneMetaDrop = []string{"creationState", "creationError", "creationLog", "diagnostics", "diagnosticsSince",
	bosskey.MetaKey}

// cloneFile is one file of the copy: rel inside the instance directory, size in bytes (what the copy writes), and how
// a qcow2 disk is carried over.
type cloneFile struct {
	rel  string
	size int64
	// rebase: the backing is the absolute path of a file inside the source directory; the copy gets this relative
	// name instead (qemu-img rebase -u). flatten: the backing is outside; the copy is a standalone image.
	rebase    string
	rebaseFmt string
	flatten   bool
	isDir     bool // a directory (created, not copied)
}

func cloneSkipped(rel string, dir bool) bool {
	base := filepath.Base(rel)
	if dir {
		for _, d := range cloneSkipDirs {
			if strings.EqualFold(rel, d) {
				return true
			}
		}
		return false
	}
	if strings.EqualFold(filepath.Ext(base), ".lock") {
		return true
	}
	if strings.Contains(strings.ToLower(base), ".modavd-") && strings.EqualFold(filepath.Ext(base), ".tmp") {
		return true // a WriteIni temp of a writer that died
	}
	for _, f := range cloneSkipFiles {
		if strings.EqualFold(rel, f) {
			return true
		}
	}
	return false
}

// insideDir: path is dir or below it (both cleaned, any case on Windows).
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// sharedSystemOverlay: the system and vendor overlays, whose backing is the root's read-only system image.
func sharedSystemOverlay(rel string) bool {
	return strings.EqualFold(rel, "system.img.qcow2") || strings.EqualFold(rel, "vendor.img.qcow2")
}

// clonePlan lists what to copy from src and how; need is the bytes the copy writes.
func (m *Manager) clonePlan(ctx context.Context, src string) (files []cloneFile, need int64, err error) {
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return errs.Wrap(errs.Internal, werr, "读不了实例目录: "+werr.Error())
		}
		if path == src {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		if cloneSkipped(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			files = append(files, cloneFile{rel: rel, isDir: true})
			return nil
		}
		// a link to a file is copied as the file it points to (a standalone copy); anything else that is not a plain
		// file (a link to a directory, a device) is not something an instance has
		fi, err := os.Stat(path)
		if err != nil {
			return errs.Wrap(errs.Internal, err, "读不了 "+path+": "+err.Error())
		}
		if !fi.Mode().IsRegular() {
			return errs.Newf(errs.Unsupported, "实例目录里有不是普通文件的 %s，不能复制", path)
		}
		f := cloneFile{rel: rel, size: fi.Size()}
		if strings.EqualFold(filepath.Ext(rel), ".qcow2") && !sharedSystemOverlay(rel) {
			if err := m.cloneBacking(ctx, src, &f); err != nil {
				return err
			}
		}
		files = append(files, f)
		need += f.size
		return nil
	})
	return files, need, err
}

// cloneBacking decides how the disk f (rel set) is carried over, from its backing file name.
func (m *Manager) cloneBacking(ctx context.Context, src string, f *cloneFile) error {
	path := filepath.Join(src, f.rel)
	info, err := m.cloneQemuImgInfo(ctx, path)
	if err != nil {
		return err
	}
	if len(info.Snapshots) > 0 {
		return errs.Newf(errs.Unsupported, "%s 里有 %d 个内部快照，不能复制", path, len(info.Snapshots)).
			WithAdvice("先删掉快照再复制")
	}
	b := info.BackingFilename
	if b == "" {
		return nil
	}
	if !filepath.IsAbs(b) {
		if insideDir(src, filepath.Join(filepath.Dir(path), b)) {
			return nil // relative inside: the copy keeps the chain
		}
	} else if insideDir(src, filepath.Clean(b)) {
		rel, err := filepath.Rel(filepath.Dir(path), filepath.Clean(b))
		if err != nil {
			return errs.Wrap(errs.Internal, err, "算不出 "+b+" 的相对路径")
		}
		f.rebase, f.rebaseFmt = rel, info.BackingFilenameFmt
		return nil
	}
	// outside the instance: the copy is flattened, which may need up to the backing's size more
	f.flatten = true
	if fi, err := os.Stat(b); err == nil {
		f.size += fi.Size()
	} else if !filepath.IsAbs(b) {
		if fi, err := os.Stat(filepath.Join(filepath.Dir(path), b)); err == nil {
			f.size += fi.Size()
		}
	}
	return nil
}

func (m *Manager) cloneQemuImgInfo(ctx context.Context, image string) (*qemuImgInfoResult, error) {
	so, se, code, err := cloneRunQemuImg(m, ctx, cloneQemuImgTimeout, "info", "--output=json", image)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errs.Newf(errs.Internal, "qemu-img info 失败（退出码 %d）: %s", code, qemuImgFail(so, se))
	}
	return decodeQemuImgInfo(so)
}

// lockInstanceForClone takes the source's locks so nobody boots, stops, bridges, creates or compacts it while it is
// copied (the same set as compact). errs.Busy when one is held; the func releases them.
func (m *Manager) lockInstanceForClone(i int) (func(), error) {
	kinds := []struct{ kind, what string }{
		{"disk", "正在整理磁盘"},
		{"start", "正在启动"},
		{"stop", "正在关闭"},
		{"bridge", "正在装卸执行桥"},
		{"create", "正在新建"},
	}
	var locks []*host.FileLock
	unlock := func() {
		for k := len(locks) - 1; k >= 0; k-- {
			locks[k].Unlock()
		}
	}
	for _, kw := range kinds {
		l, ok, err := host.TryLock(m.lockPath(i, kw.kind))
		if err != nil {
			unlock()
			return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+m.lockPath(i, kw.kind))
		}
		if !ok {
			unlock()
			return nil, errs.WithDetails(errs.Newf(errs.Busy, "ModAVD 实例 %d %s，无法复制", i, kw.what),
				map[string]string{errs.Reason: "locked", errs.Lock: kw.kind})
		}
		locks = append(locks, l)
	}
	return unlock, nil
}

// Clone copies stopped instance src into a new instance (see the top of this file). errs.Busy while src runs or is
// being started, stopped, bridged, created or compacted; errs.PrecheckFailed when the volume has no room for the copy;
// errs.Canceled when ctx ends (the half-made copy is removed).
func (m *Manager) Clone(ctx context.Context, src int, req CloneRequest) (*CloneResult, error) {
	if err := requireLifecycle(); err != nil {
		return nil, err
	}
	in, err := m.instance(src)
	if err != nil {
		return nil, err
	}
	if err := m.notCreated(src, "不能复制"); err != nil {
		return nil, err
	}
	if err := avd.ValidateTitle(req.Title); err != nil {
		return nil, err
	}
	if _, err := m.Root.EnsureMetadata(); err != nil {
		return nil, err
	}
	unlockSrc, err := m.lockInstanceForClone(src)
	if err != nil {
		return nil, err
	}
	defer unlockSrc()
	if _, running, err := m.qemuOf(src); err != nil {
		return nil, err
	} else if running {
		return nil, errs.WithDetails(errs.Newf(errs.Busy, "ModAVD 实例 %d 未关闭，无法复制", src).
			WithAdvice(fmt.Sprintf("先 modavd stop %d，关机后再复制", src)), map[string]string{errs.Reason: "running"})
	}

	files, need, err := m.clonePlan(ctx, in.Dir)
	if err != nil {
		return nil, err
	}
	free, err := cloneDiskFree(m.Root.AvdHome())
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不了剩余空间: "+err.Error())
	}
	if want := uint64(need) + cloneHeadroom; free < want {
		return nil, errs.WithDetails(errs.Newf(errs.PrecheckFailed,
			"复制实例 %d 要约 %s 可用空间，%s 所在磁盘只剩 %s", src, gib(int64(want)), m.Root.AvdHome(), gib(int64(free))).
			WithAdvice(fmt.Sprintf("先腾出约 %s 再复制", gib(int64(want-free)))), map[string]string{errs.Reason: "disk_space"})
	}

	insts, err := avd.List(m.Root)
	if err != nil {
		return nil, err
	}
	existing := map[int]bool{}
	for _, x := range insts {
		existing[x.Index] = true
	}
	dst, err := m.freeIndex(existing)
	if err != nil {
		return nil, err
	}
	lock, err := m.lockCreation(ctx, dst)
	if err != nil {
		return nil, err
	}
	dstDir := avd.Dir(m.Root, dst)
	// Mkdir: a create racing for the same index loses here (or we do) instead of sharing the directory.
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		lock.Unlock()
		if errors.Is(err, fs.ErrExist) {
			return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在", root.AvdName(dst))
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了目录 %s：%v", dstDir, err))
	}
	rollback := func() {
		os.RemoveAll(dstDir)
		os.Remove(avd.IniPath(m.Root, dst))
		m.Root.RemoveInstanceMeta(dst)
		lock.Unlock()
		m.removeLockFiles(dst)
	}
	m.Log.Infof("正在把 ModAVD 实例 %d 复制成实例 %d（约 %s）", src, dst, gib(need))
	copied, err := m.cloneCopy(ctx, src, in.Dir, dstDir, files, need, req.Progress)
	if err != nil {
		rollback()
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消复制，没复制完的实例已删掉")
		}
		return nil, err
	}

	meta := m.Root.InstanceMeta(src)
	if meta == nil {
		meta = jsonord.NewObject().Obj()
	}
	for _, k := range cloneMetaDrop {
		meta.Delete(k)
	}
	meta.Set("createdAt", jsonord.NewString(root.NowISO()))
	if _, err := avd.RegisterCopy(m.Root, dst, in.Config, req.Title, meta); err != nil {
		rollback()
		return nil, err
	}
	lock.Unlock()
	m.Log.Infof("已把 ModAVD 实例 %d 复制成实例 %d", src, dst)
	newIn, err := m.instance(dst)
	if err != nil {
		return nil, err
	}
	return &CloneResult{Source: src, Instance: m.instanceInfo(ctx, newIn, nil, nil), CopiedBytes: copied}, nil
}

// cloneCopy copies files from srcDir to dstDir (which exists), reporting progress by bytes.
func (m *Manager) cloneCopy(ctx context.Context, src int, srcDir, dstDir string, files []cloneFile, need int64,
	progress func(Progress)) (int64, error) {
	var done int64
	last := time.Time{}
	report := func(force bool) {
		if progress == nil || (!force && time.Since(last) < 500*time.Millisecond) {
			return
		}
		last = time.Now()
		permille := 1000
		if need > 0 {
			permille = int(done * 1000 / need)
		}
		progress(Progress{Step: "copy", Message: fmt.Sprintf("正在复制实例 %d（%s / %s）", src, gib(done), gib(need)),
			Done: min(permille, 1000), Total: 1000})
	}
	report(true)
	buf := make([]byte, 4<<20)
	for _, f := range files {
		if err := ctxErr(ctx); err != nil {
			return done, err
		}
		from, to := filepath.Join(srcDir, f.rel), filepath.Join(dstDir, f.rel)
		if f.isDir {
			if err := os.Mkdir(to, 0o755); err != nil {
				return done, errs.Wrap(errs.Internal, err, fmt.Sprintf("建不了目录 %s：%v", to, err))
			}
			continue
		}
		if f.flatten {
			so, se, code, err := cloneRunQemuImg(m, ctx, cloneConvertTimeout, "convert", "-O", "qcow2", from, to)
			if err != nil {
				return done, err
			}
			if code != 0 {
				return done, errs.Newf(errs.Internal, "qemu-img convert 失败（退出码 %d）: %s", code, qemuImgFail(so, se))
			}
			if err := syncFile(to); err != nil {
				return done, errs.Wrap(errs.Internal, err, "刷新复制的磁盘失败: "+to)
			}
			done += f.size
			report(false)
			continue
		}
		if err := copyFileSync(ctx, from, to, buf, func(n int64) { done += n; report(false) }); err != nil {
			return done, err
		}
		if f.rebase != "" {
			args := []string{"rebase", "-u", "-b", f.rebase}
			if f.rebaseFmt != "" {
				args = append(args, "-F", f.rebaseFmt)
			}
			so, se, code, err := cloneRunQemuImg(m, ctx, cloneQemuImgTimeout, append(args, to)...)
			if err != nil {
				return done, err
			}
			if code != 0 {
				return done, errs.Newf(errs.Internal, "qemu-img rebase 失败（退出码 %d）: %s", code, qemuImgFail(so, se))
			}
		}
	}
	report(true)
	return done, nil
}

// copyFileSync copies one file (a new file at to) and flushes it to disk; added is called with each chunk's size.
func copyFileSync(ctx context.Context, from, to string, buf []byte, added func(int64)) error {
	in, err := os.Open(from)
	if err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("读不了 %s：%v", from, err))
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("写不了 %s：%v", to, err))
	}
	fail := func(err error) error {
		out.Close()
		return err
	}
	for {
		if err := ctxErr(ctx); err != nil {
			return fail(err)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fail(errs.Wrap(errs.Internal, werr, fmt.Sprintf("写 %s 失败：%v", to, werr)))
			}
			added(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(errs.Wrap(errs.Internal, rerr, fmt.Sprintf("读 %s 失败：%v", from, rerr)))
		}
	}
	if err := out.Sync(); err != nil {
		return fail(errs.Wrap(errs.Internal, err, fmt.Sprintf("刷新 %s 失败：%v", to, err)))
	}
	if err := out.Close(); err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("写 %s 失败：%v", to, err))
	}
	return nil
}
