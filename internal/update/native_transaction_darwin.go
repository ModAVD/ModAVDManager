//go:build darwin && cgo

package update

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

type nativeUnit struct {
	Path   string `json:"path"`
	HadOld bool   `json:"had_old"`
	Step   string `json:"step"` // pending, old_moved, new_moved
}
type nativeState struct {
	Format    string `json:"format"`
	Phase     string `json:"phase"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Target    struct {
		Version string `json:"version"`
		Source  string `json:"source"`
		Asset   string `json:"asset_name"`
	} `json:"target"`
	ManifestSHA256 string              `json:"manifest_sha256"`
	From           string              `json:"from"`
	Units          []nativeUnit        `json:"units"`
	Metadata       *nativeMetadataUndo `json:"metadata,omitempty"`
}

func (u *Updater) nativeUpdates() bool { return root.CurrentPlatform().Supported() }
func (u *Updater) hasNativeTransaction() bool {
	s, _ := u.l.readState().Get("format").Str()
	return s == nativeFormat
}
func (l layout) nativeStagedVersion(st *jsonord.Obj) (Version, bool) {
	f, _ := st.Get("format").Str()
	if f != nativeFormat || phaseOf(st) != phaseStaged || !isDir(filepath.Join(l.stage(), "payload")) {
		return Version{}, false
	}
	v, _ := st.Get("target").Obj().Get("version").Str()
	return ParseVersion(v)
}
func (u *Updater) nativePayload() string { return filepath.Join(u.l.stage(), "payload") }
func (u *Updater) nativeBackup(p string) string {
	return filepath.Join(u.l.previous(), "native", filepath.FromSlash(p))
}
func (u *Updater) writeNativeState(st *nativeState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return packageError("更新事务元数据太大")
	}
	v, err := jsonord.Parse(b)
	if err != nil {
		return err
	}
	v.Obj().Set("updatedAt", jsonord.NewString(root.NowISO()))
	if len(v.MarshalPython()) > 1<<20 {
		return packageError("更新事务元数据太大")
	}
	return u.l.writeState(v.Obj())
}
func (u *Updater) readNativeState() (*nativeState, error) {
	b, err := nativeReadFile(u.l.state(), 1<<20)
	if err != nil {
		return nil, err
	}
	var st nativeState
	if err := decodeNativeJSON(b, &st); err != nil {
		return nil, err
	}
	if st.Format != nativeFormat || len(st.Units) > len(nativeAllowedUnits) {
		return nil, packageError("更新事务格式错误")
	}
	if st.Metadata != nil {
		id, err := hex.DecodeString(st.Metadata.ID)
		if err != nil || len(id) != 16 || len(st.Metadata.Components) == 0 || len(st.Metadata.History) == 0 {
			return nil, packageError("更新事务元数据恢复记录错误")
		}
	}
	seen := map[string]bool{}
	for _, unit := range st.Units {
		if !nativeAllowedUnits[unit.Path] || seen[unit.Path] {
			return nil, packageError("更新事务单元不安全: %s", unit.Path)
		}
		seen[unit.Path] = true
		switch unit.Step {
		case "pending", "old_moved", "new_moved", "restoring", "restored":
		default:
			return nil, packageError("更新事务步骤不正确")
		}
		if st.Phase != "rolling_back" && (unit.Step == "restored" || unit.Step == "restoring") {
			return nil, packageError("交换状态含回退步骤")
		}
	}
	if _, ok := ParseVersion(st.Target.Version); !ok {
		return nil, packageError("更新事务版本错误")
	}
	if st.Phase != phasePreparing && st.Phase != phaseStaged && st.Phase != phaseSwapping && st.Phase != "rolling_back" {
		return nil, packageError("更新事务状态错误")
	}
	if st.Phase != phasePreparing && !sha256Hex.MatchString(st.ManifestSHA256) {
		return nil, packageError("更新事务清单摘要错误")
	}
	if (st.Phase == phaseSwapping || st.Phase == "rolling_back") && len(st.Units) == 0 {
		return nil, packageError("更新事务缺少交换单元")
	}
	if st.Phase == phaseSwapping || st.Phase == "rolling_back" {
		for _, unit := range []string{"sdk/emulator", "ModAVD.app", "tools", manifestName} {
			if !seen[unit] {
				return nil, packageError("更新事务缺少必需单元: %s", unit)
			}
		}
	}
	return &st, nil
}

func (u *Updater) nativeUpdate(ctx context.Context, install bool) (*Status, error) {
	if !u.nativeUpdates() {
		return u.status(), errs.New(errs.Unsupported, "本地签名包更新只支持原生 macOS ARM64 构建")
	}
	u.localOperation = u.Opt.PackagePath != ""
	if u.hasNativeTransaction() {
		if pending, err := u.readNativeState(); err == nil && pending.Target.Source == "local" {
			u.localOperation = true
		}
	}
	st := u.status()
	if !st.Updatable && !u.hasNativeTransaction() {
		return st, errs.New(errs.NotReady, st.NotUpdatableReason)
	}
	if (u.Opt.PackagePath == "") != (u.Opt.PackageSHA256 == "") {
		return st, errs.New(errs.Usage, "本地更新必须同时指定 --package ZIP 和 --sha256 HEX")
	}
	lock, err := u.begin(st)
	if err != nil {
		return st, err
	}
	defer lock.Unlock()
	if reason := u.notUpdatable(); reason != "" {
		return u.finish(st), errs.New(errs.NotReady, reason)
	}
	if u.Opt.PackagePath != "" || phaseOf(u.l.readState()) != phaseStaged || !u.hasNativeTransaction() {
		if err := u.prepareNative(ctx, st); err != nil {
			return u.finish(st), err
		}
	}
	if !install {
		out := u.finish(st)
		if out.PreparedVersion != "" {
			out.Message = displayVersion(out.PreparedVersion) + " 已核对文件与签名，可以离线安装"
		}
		return out, nil
	}
	if !u.hasNativeTransaction() || phaseOf(u.l.readState()) != phaseStaged {
		return u.finish(st), nil
	}
	ns, err := u.readNativeState()
	if err != nil {
		return u.finish(st), err
	}
	v, _ := ParseVersion(ns.Target.Version)
	if cur, ok := LocalVersion(u.Root); ok && !cur.Less(v) {
		return u.finish(st), packageError("更新包 %s 不比本地 %s 新", v.Display(), cur.Display())
	}
	m, err := u.checkedNativeStage(ctx, ns)
	if err != nil {
		return u.finish(st), err
	}
	unlock, err := u.lockNativeSwap("安装")
	if err != nil {
		return u.finish(st), err
	}
	defer unlock()
	if err := u.checkSpace(); err != nil {
		return u.finish(st), err
	}
	if err := u.validateNativeDestinations(m); err != nil {
		return u.finish(st), err
	}
	if ctx.Err() != nil {
		return u.finish(st), errs.Wrap(errs.Canceled, ctx.Err(), "更新已取消")
	}
	if err := removeTree(u.l.previous()); err != nil {
		return u.finish(st), err
	}
	if cur, ok := LocalVersion(u.Root); ok {
		ns.From = cur.String()
	}
	ns.Phase = phaseSwapping
	ns.Units = nil
	for _, p := range m.Units {
		ns.Units = append(ns.Units, nativeUnit{Path: p, HadOld: lexists(u.l.at(p)), Step: "pending"})
	}
	if err := u.writeNativeState(ns); err != nil {
		return u.finish(st), err
	}
	u.step("installing", "正在换上 macOS 新版")
	if err := u.completeNativeSwap(ns, m); err != nil {
		unlock() // recovery acquires the same start locks itself
		msg, rerr := u.recoverNative()
		if rerr != nil {
			return u.finish(st), fmt.Errorf("%w; recovery: %v", err, rerr)
		}
		st.Recovered = msg
		return u.finish(st), err
	}
	out := u.finish(st)
	out.Updated = true
	out.Message = "已更新到 " + v.Display()
	return out, nil
}

func (u *Updater) prepareNative(ctx context.Context, st *Status) (err error) {
	check := func() error {
		if ctx.Err() != nil {
			return errs.Wrap(errs.Canceled, ctx.Err(), "更新已取消")
		}
		return u.checkSpace()
	}
	var archive, source, asset string
	var expected Version
	if u.Opt.PackagePath != "" {
		archive, err = filepath.Abs(u.Opt.PackagePath)
		if err != nil {
			return err
		}
		for _, dir := range []string{u.l.stage(), u.l.previous(), u.l.pkg(), u.l.trash()} {
			rel, e := filepath.Rel(dir, archive)
			if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return packageError("本地更新归档不能放在更新暂存或备份目录内")
			}
		}
		want := strings.ToLower(u.Opt.PackageSHA256)
		if !sha256Hex.MatchString(want) {
			return errs.New(errs.Usage, "--sha256 必须是 64 位十六进制摘要")
		}
		fi, e := os.Lstat(archive)
		if e != nil || !fi.Mode().IsRegular() {
			return packageError("本地更新包必须是普通文件")
		}
		if err := u.checkSpace(); err != nil {
			return err
		}
		if err := os.MkdirAll(u.l.download(), 0755); err != nil {
			return err
		}
		tmp, e := os.CreateTemp(u.l.download(), "local-*.zip")
		if e != nil {
			return e
		}
		selected := tmp.Name()
		if e := tmp.Close(); e != nil {
			return e
		}
		defer os.Remove(selected)
		if e := nativeCopyFile(archive, selected, check); e != nil {
			return e
		}
		sum, e := nativeHashFile(selected)
		if e != nil || sum != want {
			return packageError("本地更新包 SHA256 不符")
		}
		source, asset = "local", filepath.Base(archive)
		archive = selected // Extract only the exact bytes whose supplied digest was checked.
	} else {
		r, e := u.pick(ctx, st)
		if e != nil {
			return e
		}
		if r == nil {
			return nil
		}
		if r.incremental {
			return packageError("macOS 更新需要完整签名包；增量更新尚未提供")
		}
		if !sha256Hex.MatchString(r.sha256) {
			return sourceError("macOS 更新包缺少可信传输摘要，暂不能下载")
		}
		if r.size <= 0 || r.size > maxUnpackedBytes {
			return sourceError("macOS 更新归档大小无效或超过限制")
		}
		archive, err = u.download(ctx, r)
		if err != nil {
			return err
		}
		source, asset, expected = r.source, r.assetName, r.version
		if r.topDir != "" {
			return packageError("macOS 原生更新包不允许额外顶层目录")
		}
	}
	if err := u.checkSpace(); err != nil {
		return err
	}
	ns := &nativeState{Format: nativeFormat, Phase: phasePreparing}
	ns.Target.Version = expected.String()
	ns.Target.Source = source
	ns.Target.Asset = asset
	if err := u.writeNativeState(ns); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			u.l.abortStaging(err.Error())
		}
	}()
	u.step("extracting", "正在解开 macOS 签名更新包")
	m, err := extractNativePackage(archive, u.nativePayload(), "", check)
	if err != nil {
		return err
	}
	v, _ := ParseVersion(m.Version)
	if source != "local" && v != expected {
		return packageError("更新包版本与发布版本不一致")
	}
	if cur, ok := LocalVersion(u.Root); ok && !cur.Less(v) {
		return packageError("更新包 %s 不比本地 %s 新", v.Display(), cur.Display())
	}
	u.step("verifying", "正在核对 ARM64 文件、应用签名与清单")
	if err := verifyDarwinPackageContext(ctx, u.nativePayload(), m, v); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	ns.ManifestSHA256, err = nativeHashFile(filepath.Join(u.nativePayload(), nativeManifestName))
	if err != nil {
		return err
	}
	ns.Target.Version = v.String()
	ns.Phase = phaseStaged
	return u.writeNativeState(ns)
}

func (u *Updater) checkedNativeStage(ctx context.Context, st *nativeState) (*nativeManifest, error) {
	mp := filepath.Join(u.nativePayload(), nativeManifestName)
	sum, err := nativeHashFile(mp)
	if err != nil || sum != st.ManifestSHA256 {
		return nil, packageError("暂存清单被修改或丢失")
	}
	m, err := readNativeManifest(mp)
	if err != nil {
		return nil, err
	}
	v, _ := ParseVersion(st.Target.Version)
	if m.Version != v.String() && "v"+m.Version != v.String() {
		return nil, packageError("暂存版本不一致")
	}
	if err := verifyDarwinPackageContext(ctx, u.nativePayload(), m, v); err != nil {
		return nil, err
	}
	return m, nil
}

// No transaction unit may be reached through a symlink in the installed root.
func (u *Updater) validateNativeDestinations(m *nativeManifest) error {
	for _, unit := range m.Units {
		p := u.l.at(unit)
		for {
			fi, err := os.Lstat(p)
			if err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return packageError("更新目标是符号链接: %s", p)
			}
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if p == u.Root.Dir {
				break
			}
			p = filepath.Dir(p)
			if p == filepath.Dir(p) {
				return packageError("更新根目录不正确")
			}
		}
	}
	return nil
}

func (u *Updater) completeNativeSwap(st *nativeState, m *nativeManifest) error {
	if len(st.Units) != len(m.Units) {
		return packageError("交换计划与签名包单元不一致")
	}
	for i, unit := range st.Units {
		if unit.Path != m.Units[i] {
			return packageError("交换计划与签名包单元顺序不一致")
		}
	}
	if err := u.validateNativeDestinations(m); err != nil {
		return err
	}
	// A cut rename may leave verified units in either location. Verify that
	// complete candidate, including signatures, before doing another rename.
	at := func(p string) string {
		unit := m.unit(p)
		base := filepath.Join(u.nativePayload(), filepath.FromSlash(unit))
		if !lexists(base) {
			base = u.l.at(unit)
		}
		if p == unit {
			return base
		}
		return filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(p, unit+"/")))
	}
	v, _ := ParseVersion(st.Target.Version)
	if err := verifyDarwinPackageAt(m, v, at); err != nil {
		return err
	}
	for i := range st.Units {
		unit := &st.Units[i]
		if err := u.validateNativeUnitPaths(unit.Path, nativeDirectoryUnit(unit.Path)); err != nil {
			return err
		}
		src := filepath.Join(u.nativePayload(), filepath.FromSlash(unit.Path))
		dst := u.l.at(unit.Path)
		old := u.nativeBackup(unit.Path)
		if unit.Step == "pending" {
			if unit.HadOld && !lexists(old) {
				if !lexists(dst) {
					return fmt.Errorf("old update unit missing: %s", unit.Path)
				}
				if err := os.MkdirAll(filepath.Dir(old), 0755); err != nil {
					return err
				}
				if err := os.Rename(dst, old); err != nil {
					return err
				}
			}
			unit.Step = "old_moved"
			if err := u.writeNativeState(st); err != nil {
				return err
			}
		}
		if unit.Step == "old_moved" {
			if lexists(src) {
				if lexists(dst) {
					return fmt.Errorf("ambiguous update unit: %s", unit.Path)
				}
				if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					return err
				}
				if err := os.Rename(src, dst); err != nil {
					return err
				}
			} else if !lexists(dst) {
				return fmt.Errorf("new update unit missing: %s", unit.Path)
			}
			unit.Step = "new_moved"
			if err := u.writeNativeState(st); err != nil {
				return err
			}
		}
	}
	if err := verifyDarwinPackage(u.Root.Dir, m, v); err != nil {
		return err
	}
	if err := u.recordNativeMetadata(st); err != nil {
		return err
	}
	result := jsonord.NewObject().Obj()
	result.Set("ok", jsonord.NewBool(true))
	result.Set("action", jsonord.NewString("update"))
	result.Set("version", jsonord.NewString(st.Target.Version))
	if err := u.l.writeState(newState(phaseIdle, "lastResult", objVal(result))); err != nil {
		return err
	}
	u.l.tidyUp(u.l.previous(), u.l.stage(), u.l.pkg())
	u.l.purgeTrash()
	return nil
}

func (u *Updater) recoverNative() (string, error) {
	st, err := u.readNativeState()
	if err != nil {
		return "", err
	}
	if st.Target.Source == "local" {
		u.localOperation = true
	}
	if err := u.validateNativeWorkspace(); err != nil {
		return "", err
	}
	if st.Phase == phaseSwapping || st.Phase == "rolling_back" {
		unlock, err := u.lockNativeSwap("恢复更新")
		if err != nil {
			return "", err
		}
		defer unlock()
	}
	switch st.Phase {
	case phasePreparing:
		if err := u.l.abortStaging("上次 macOS 更新准备未完成"); err != nil {
			return "", err
		}
		return "已丢弃上次未完成的 macOS 更新暂存", nil
	case phaseStaged:
		return "", nil
	case phaseSwapping:
		mp := filepath.Join(u.nativePayload(), nativeManifestName)
		sum, e := nativeHashFile(mp)
		if e == nil && sum == st.ManifestSHA256 {
			if m, e := readNativeManifest(mp); e == nil {
				if e := u.completeNativeSwap(st, m); e == nil {
					return "已离线补完 macOS 更新（" + displayVersion(st.Target.Version) + "）", nil
				}
			}
		}
		st.Phase = "rolling_back"
		if err := u.writeNativeState(st); err != nil {
			return "", err
		}
		return u.rollbackNative(st)
	case "rolling_back":
		return u.rollbackNative(st)
	default:
		return "", packageError("未知 macOS 更新事务状态")
	}
}

// A copied Windows/AUTO-MAS journal must never enter the native rename path.
func (u *Updater) recoverNativeWorkspace() (string, error) {
	if err := u.validateNativeWorkspace(); err != nil {
		return "", err
	}
	if !lexists(u.l.state()) {
		return "", nil
	}
	b, err := nativeReadFile(u.l.state(), 1<<20)
	if err != nil {
		return "", err
	}
	v, err := jsonord.Parse(b)
	if err != nil || v.Kind() != jsonord.Object {
		return "", packageError("更新状态文件损坏，已保留现场")
	}
	state := v.Obj()
	format, _ := state.Get("format").Str()
	if format == nativeFormat {
		return u.recoverNative()
	}
	if format != "" || phaseOf(state) != phaseIdle {
		return "", packageError("当前更新事务不是 macOS 原生格式，已保留现场")
	}
	u.l.purgeTrash()
	return "", nil
}

func (u *Updater) rollbackNative(st *nativeState) (string, error) {
	// If the staged payload is unavailable/corrupt, restore every unit from
	// this transaction's backup. This is crash recovery, not a retained rollback.
	for i := len(st.Units) - 1; i >= 0; i-- {
		unit := &st.Units[i]
		if err := u.validateNativeUnitPaths(unit.Path, nativeDirectoryUnit(unit.Path)); err != nil {
			return "", err
		}
		if unit.Step == "restored" {
			if lexists(u.l.at(unit.Path)) != unit.HadOld {
				return "", fmt.Errorf("restored update unit missing or unexpectedly present: %s", unit.Path)
			}
			continue
		}
		old := u.nativeBackup(unit.Path)
		dst := u.l.at(unit.Path)
		if lexists(old) {
			unit.Step = "restoring"
			if err := u.writeNativeState(st); err != nil {
				return "", err
			}
			if err := removeTree(dst); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return "", err
			}
			if err := os.Rename(old, dst); err != nil {
				return "", err
			}
		} else if !unit.HadOld && unit.Step != "pending" {
			if err := removeTree(dst); err != nil {
				return "", err
			}
		} else if unit.HadOld && !(unit.Step == "pending" && lexists(dst)) && !(unit.Step == "restoring" && lexists(dst)) {
			return "", fmt.Errorf("cannot restore missing backup: %s", unit.Path)
		}
		unit.Step = "restored"
		if err := u.writeNativeState(st); err != nil {
			return "", err
		}
	}
	if err := u.restoreNativeMetadata(st); err != nil {
		return "", err
	}
	result := jsonord.NewObject().Obj()
	result.Set("action", jsonord.NewString("update"))
	result.Set("ok", jsonord.NewBool(false))
	result.Set("error", jsonord.NewString("上次 macOS 更新损坏，已恢复原单元"))
	if err := u.l.writeState(newState(phaseIdle, "lastResult", objVal(result))); err != nil {
		return "", err
	}
	u.l.tidyUp(u.l.previous(), u.l.stage(), u.l.pkg())
	u.l.purgeTrash()
	return "上次 macOS 更新未完成，已恢复原单元", nil
}
