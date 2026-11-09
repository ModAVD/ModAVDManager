package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

const bridgeBackingFile = "modavd-bridge-backings.json"

// One record per system/vendor overlay, kept with the instance. Previous is the
// header path before the last repair: either header can survive an interrupted move.
type backingProof struct {
	Path     string `json:"path"`
	Previous string `json:"previous,omitempty"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Stamp    string `json:"stamp,omitempty"`
}

type backingDisk struct {
	overlay, old, target string
	virtualSize          int64 // zero before the emulator creates the overlay
	stale                bool
}

// prepareBridgeBackings runs under the start lock, only for a cold writable boot.
func (m *Manager) prepareBridgeBackings(ctx context.Context, in *avd.Instance) error {
	marker, err := os.Lstat(filepath.Join(in.Dir, bridgeMarkerName))
	if errors.Is(err, os.ErrNotExist) && nativePlayStore() {
		marker, err = os.Lstat(filepath.Join(in.Dir, playStoreMarkerName))
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return errs.Newf(errs.PrecheckFailed, "实例 %s 的可写系统开机标记不是普通文件", in.Dir)
	}
	if st, err := os.Lstat(in.Dir); err != nil || !st.IsDir() {
		return errs.Newf(errs.PrecheckFailed, "无法确认执行桥覆盖层的实例目录 %s", in.Dir)
	}
	proofPath := filepath.Join(in.Dir, bridgeBackingFile)
	proofs := map[string]backingProof{}
	record, err := os.Lstat(proofPath)
	if err == nil {
		if !record.Mode().IsRegular() || record.Size() > 1<<20 {
			return errs.Newf(errs.PrecheckFailed, "执行桥后备镜像记录 %s 无效，请从该实例的备份恢复", proofPath)
		}
		data, err := os.ReadFile(proofPath)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &proofs); err != nil || proofs == nil {
			return errs.Newf(errs.PrecheckFailed, "执行桥后备镜像记录 %s 损坏，请从该实例的备份恢复", proofPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	sysdir := filepath.Clean(in.Config.Value("image.sysdir.1"))
	standard := sysdir == filepath.Clean(root.CurrentPlatform().SystemImageDir)
	if root.CurrentPlatform().Windows() {
		standard = strings.EqualFold(sysdir, filepath.Clean(root.SystemImageDir))
	}
	imageDir := filepath.Join(m.Root.RuntimeSDK(), sysdir)
	var repairs []backingDisk
	changed := false
	for _, name := range []string{"system.img", "vendor.img"} {
		disk, err := m.inspectBridgeDisk(ctx, filepath.Join(in.Dir, name+".qcow2"), filepath.Join(imageDir, name), standard)
		if err != nil {
			return err
		}
		if disk == nil {
			continue
		}
		proof := proofs[name]
		// Enroll the file named in the existing header, even when a still-live
		// shared SDK uses a different spelling or the root has already been copied.
		path := disk.old
		if disk.stale {
			path = disk.target
		}
		base, err := openBackingRead(path)
		if err != nil {
			if disk.virtualSize == 0 && errors.Is(err, os.ErrNotExist) {
				return errs.Newf(errs.NotReady, "首次可写开机缺少系统镜像 %s", path).WithAdvice("请先补全系统镜像组件")
			}
			return backingError(disk.overlay, disk.old, disk.target, err.Error())
		}
		defer base.Close() // pin both verified files through every replacement
		if disk.stale && !proof.matchesSource(disk.old) {
			return backingError(disk.overlay, disk.old, disk.target, "原路径已失效，无法用该实例保存的 SHA-256 确认原后备镜像")
		}
		next, err := disk.verify(ctx, base, proof)
		if err != nil {
			return err
		}
		if next != proof {
			proofs[name] = next
			changed = true
		}
		if disk.stale {
			repairs = append(repairs, *disk)
		}
	}

	// Check both bases before touching either overlay. Persist both allowed paths
	// first: a retry after either atomic replacement can finish the remaining disk.
	if changed {
		data, err := json.MarshalIndent(proofs, "", "  ")
		if err != nil {
			return err
		}
		if err := writeBackingProofs(proofPath, data); err != nil {
			return fmt.Errorf("保存执行桥后备镜像记录: %w", err)
		}
	}
	for _, disk := range repairs {
		if err := m.rebaseBridgeCopy(ctx, disk); err != nil {
			return err
		}
		m.Log.Infof("实例 %d 已核对 SHA-256，%s 的后备镜像从 %s 改为 %s", in.Index, disk.overlay, disk.old, disk.target)
	}
	return nil
}

func (p backingProof) matchesSource(path string) bool {
	return isSHA256Hex(p.SHA256) && p.Size > 0 && (path == p.Path || path == p.Previous)
}

func (m *Manager) inspectBridgeDisk(ctx context.Context, overlay, target string, standard bool) (*backingDisk, error) {
	d := &backingDisk{overlay: overlay, old: target, target: target}
	st, err := os.Lstat(overlay)
	if errors.Is(err, os.ErrNotExist) {
		if !standard {
			return nil, nil
		}
		return d, nil // record the raw base before the first writable boot
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errs.Newf(errs.PrecheckFailed, "执行桥覆盖层 %s 不是普通文件，请检查实例目录", overlay)
	}
	info, err := m.qemuImgInfo(ctx, overlay)
	if err != nil {
		return nil, errs.Wrap(errs.CodeOf(err), err, fmt.Sprintf("读取覆盖层 %s 失败：%v", overlay, err))
	}
	if info.BackingFilename == "" { // self-contained
		return nil, nil
	}
	d.old = filepath.Clean(info.BackingFilename)
	if !filepath.IsAbs(d.old) {
		d.old = filepath.Join(filepath.Dir(overlay), d.old)
	}
	d.virtualSize = info.VirtualSize
	_, err = os.Stat(d.old)
	d.stale = errors.Is(err, os.ErrNotExist)
	if err != nil && !d.stale {
		return nil, backingError(overlay, d.old, target, err.Error())
	}
	managed := standard && info.Format == "qcow2" && info.BackingFilenameFmt == "raw" && info.VirtualSize > 0 && filepath.IsAbs(info.BackingFilename)
	role := string(filepath.Separator) + filepath.Join(root.CurrentPlatform().SystemImageDir, filepath.Base(target))
	pathRole := strings.HasSuffix(d.old, role)
	if root.CurrentPlatform().Windows() {
		pathRole = strings.HasSuffix(strings.ToLower(d.old), strings.ToLower(role))
	}
	if !managed || !pathRole {
		if d.stale {
			return nil, backingError(overlay, d.old, target, "无法确认覆盖层格式或原后备路径")
		}
		return nil, nil // preserve working relative/custom chains
	}
	return d, nil
}

func (d backingDisk) verify(ctx context.Context, base *os.File, proof backingProof) (backingProof, error) {
	fi, err := base.Stat()
	if err != nil {
		return proof, err
	}
	if !fi.Mode().IsRegular() || fi.Size() <= 0 || (d.virtualSize != 0 && fi.Size() != d.virtualSize) {
		return proof, backingError(d.overlay, d.old, d.target, "候选镜像类型或大小不符")
	}
	stamp := host.FileChangeStamp(base)
	// The stamp only saves a full read on an ordinary boot. Every rebase hashes
	// the candidate, including a same-volume rename that preserves the file ID.
	if !d.stale && stamp != "" && proof.Stamp == stamp && proof.Path == base.Name() && proof.Size == fi.Size() && isSHA256Hex(proof.SHA256) {
		return proof, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, cancelReader{ctx, base}); err != nil {
		return proof, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if d.virtualSize != 0 && proof.SHA256 != "" && (proof.Size != fi.Size() || proof.SHA256 != sum) {
		return proof, backingError(d.overlay, d.old, d.target, "候选镜像与实例记录的大小或 SHA-256 不符")
	}
	next := backingProof{Path: base.Name(), Size: fi.Size(), SHA256: sum, Stamp: stamp}
	if d.stale {
		next.Previous = d.old
	}
	return next, nil
}

func backingError(overlay, old, target, why string) error {
	return errs.Newf(errs.PrecheckFailed, "无法接回 %s 的后备镜像（原路径 %s，当前 SDK %s）：%s", overlay, old, target, why).
		WithAdvice("保留实例和覆盖层。将根目录和原镜像恢复到搬迁前的位置，用新版管理器开机、关机一次再整体搬迁；已有校验记录时可直接将原镜像恢复到当前 SDK")
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := ctxErr(r.ctx); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func writeBackingProofs(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-proof-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return replaceBackingFile(tmp.Name(), path)
}

// Rebase a private copy: a failed or killed qemu-img must never damage the only
// copy of the installed bridge. Rename also leaves other hard links intact.
func (m *Manager) rebaseBridgeCopy(ctx context.Context, repair backingDisk) error {
	source, err := openBackingRead(repair.overlay)
	if err != nil {
		return err
	}
	defer source.Close()
	fi, err := source.Stat()
	if err != nil {
		return err
	}
	// Only this instance's start lock owner uses this scratch name. A killed
	// copy/rebase can leave it behind; the original is intact until rename.
	path := repair.overlay + ".modavd-rebase.tmp"
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("覆盖层临时路径不是普通文件: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	free, err := host.DiskFree(filepath.Dir(repair.overlay))
	if err != nil {
		return err
	}
	if free < uint64(fi.Size())+compactSpaceSlack {
		return errs.Newf(errs.PrecheckFailed, "修正 %s 需要 %s 空闲空间（含临时副本），请腾出空间再开机", repair.overlay, gib(fi.Size()+compactSpaceSlack))
	}
	tmp, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	_, err = io.Copy(tmp, cancelReader{ctx, source})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	info, err := m.qemuImgInfo(ctx, path)
	if err != nil {
		return err
	}
	if filepath.Clean(info.BackingFilename) != repair.old || info.Format != "qcow2" || info.BackingFilenameFmt != "raw" || info.VirtualSize != repair.virtualSize {
		return errs.Newf(errs.PrecheckFailed, "%s 的覆盖层已变化，请重新开机", repair.overlay)
	}
	so, se, code, err := m.runQemuImg(ctx, diskInfoTimeout, "rebase", "-u", "-f", "qcow2", "-F", "raw", "-b", repair.target, path)
	if err != nil {
		return err
	}
	if code != 0 {
		return errs.Newf(errs.PrecheckFailed, "无法修正 %s 的后备路径：%s", repair.overlay, qemuImgFail(so, se))
	}
	if err := syncFile(path); err != nil {
		return err
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	source.Close() // Windows cannot replace an open file
	return replaceBackingFile(path, repair.overlay)
}

// POSIX has no Windows mandatory share modes. Native operations hold the
// instance lifecycle lock, pin the opened inode and reject links/replacements at
// open; content hashes remain required for every changed backing identity.
func openBackingRead(path string) (*os.File, error) {
	if !nativePlayStore() {
		return host.OpenStableRead(path)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("后备镜像不是普通文件: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		f.Close()
		return nil, fmt.Errorf("后备镜像在打开时变化: %s", path)
	}
	return f, nil
}

func replaceBackingFile(from, to string) error {
	if !nativePlayStore() {
		return host.MoveFileReplaceThrough(from, to)
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
