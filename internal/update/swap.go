//go:build windows

package update

// Staging state, the swap and recovery: AUTO-MAS update_stage.py begin_staging / finish_staging / abort_staging /
// _check_staged / swap_in / _finish_install / recover, same order of state writes relative to each rename. Every
// state write here is flushed before the next rename (AUTO-MAS writes without flushing; both read the same files).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

// swapBlocked: the swap did not start (or was undone) because something is still running or held; the prepared
// version stays, the next install uses it.
func swapBlocked(format string, args ...any) *errs.Error {
	return errs.Newf(errs.Busy, format, args...)
}

func lastResultOf(st *jsonord.Obj) *jsonord.Value {
	if v := st.Get("lastResult"); v != nil {
		return v
	}
	return jsonord.NewNull()
}

// beginStaging: phase preparing for target; any earlier pkg\ / stage\ is thrown away.
func (l layout) beginStaging(target *jsonord.Obj) error {
	st := l.readState()
	if err := l.writeState(newState(phasePreparing, "target", objVal(target), "lastResult", lastResultOf(st))); err != nil {
		return err
	}
	for _, p := range []string{l.pkg(), l.stage()} {
		if err := l.toTrash(p); err != nil {
			return err
		}
	}
	l.purgeTrash()
	return nil
}

// finishStaging: the stage is built and verified; record its fingerprint and go to staged.
func (l layout) finishStaging(target *jsonord.Obj, verified string, files int, mode string) error {
	fp, err := l.fingerprint()
	if err != nil {
		return err
	}
	version, _ := target.Get("version").Str()
	marker := jsonord.NewObject().Obj()
	marker.Set("version", jsonord.NewString(version))
	marker.Set("fingerprint", jsonord.NewString(fp))
	if err := l.writeJSON(l.stagedMarker(), marker); err != nil {
		return err
	}
	staged := jsonord.NewObject().Obj()
	staged.Set("verified", jsonord.NewString(verified))
	staged.Set("files", jsonord.NewInt(int64(files)))
	staged.Set("mode", jsonord.NewString(mode))
	staged.Set("version", jsonord.NewString(version))
	staged.Set("fingerprint", jsonord.NewString(fp))
	st := l.readState()
	return l.writeState(newState(phaseStaged, "target", objVal(target), "staged", objVal(staged), "lastResult", lastResultOf(st)))
}

// abortStaging drops pkg\ and stage\ and goes back to idle with the reason (only from preparing or staged).
func (l layout) abortStaging(reason string) error {
	st := l.readState()
	if p := phaseOf(st); p != phasePreparing && p != phaseStaged {
		return nil
	}
	for _, p := range []string{l.pkg(), l.stage()} {
		if err := l.toTrash(p); err != nil {
			return err
		}
	}
	res := jsonord.NewObject().Obj()
	res.Set("action", jsonord.NewString("prepare"))
	res.Set("ok", jsonord.NewBool(false))
	res.Set("error", jsonord.NewString(reason))
	err := l.writeState(newState(phaseIdle, "lastResult", objVal(res)))
	l.purgeTrash()
	return err
}

// stagedVersion is the version waiting to be installed: phase staged with its stage\emulator there.
func (l layout) stagedVersion() (Version, bool) {
	st := l.readState()
	if phaseOf(st) != phaseStaged || !isDir(l.stageEmulator()) {
		return Version{}, false
	}
	s, _ := st.Get("target").Obj().Get("version").Str()
	return ParseVersion(s)
}

// checkStaged is AUTO-MAS _check_staged: the stage on disk is exactly what was verified (version and fingerprint
// both in state.json and stage\staged.json). A mismatch throws it away.
func (l layout) checkStaged(st *jsonord.Obj) error {
	target := st.Get("target").Obj()
	version, _ := target.Get("version").Str()
	problem := ""
	switch {
	case phaseOf(st) != phaseStaged || version == "":
		problem = "还没有准备好的新版，请先下载"
	case !isDir(l.stageEmulator()):
		problem = "暂存的新版不见了，请重新下载"
	default:
		staged := st.Get("staged").Obj()
		sv, _ := staged.Get("version").Str()
		sf, _ := staged.Get("fingerprint").Str()
		mv, mf := "", ""
		if b, err := root.ReadFileShared(l.stagedMarker()); err == nil {
			if v, err := jsonord.Parse(trimBOM(b)); err == nil {
				mv, _ = v.Obj().Get("version").Str()
				mf, _ = v.Obj().Get("fingerprint").Str()
			}
		}
		now, err := l.fingerprint()
		if err != nil || sv != version || mv != version || sf == "" || sf != mf || sf != now {
			problem = "暂存的新版和记录对不上（准备时可能被打断过），已丢掉，请重新下载"
		}
	}
	if problem == "" {
		return nil
	}
	if phaseOf(st) == phaseStaged {
		l.abortStaging(problem)
	}
	return packageError("%s", problem)
}

// ---- root files

type rootPlan struct{ replace, delete, created []string }

func (p rootPlan) value() *jsonord.Value {
	o := jsonord.NewObject()
	o.Obj().Set("replace", strs(p.replace))
	o.Obj().Set("delete", strs(p.delete))
	o.Obj().Set("created", strs(p.created))
	return o
}

func planOf(v *jsonord.Value) rootPlan {
	o := v.Obj()
	return rootPlan{strList(o.Get("replace")), strList(o.Get("delete")), strList(o.Get("created"))}
}

// planRootFiles is AUTO-MAS plan_root_files: replace = every file left in pkg\; delete = files of the current root
// manifest (but sdk\emulator) that the new package lacks; created = replaced files the root does not have yet.
func (l layout) planRootFiles() (rootPlan, error) {
	var p rootPlan
	if err := walkFiles(l.pkg(), func(rel string, _ os.FileInfo) { p.replace = append(p.replace, rel) }); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	sort.Strings(p.replace)
	newKeys := map[string]bool{}
	for _, rel := range p.replace {
		newKeys[strings.ToLower(rel)] = true
		if !lexists(l.at(rel)) {
			p.created = append(p.created, rel)
		}
	}
	if m, ok := readManifest(filepath.Join(l.root, manifestName)); ok {
		if files, err := manifestFiles(m); err == nil {
			old := []string{manifestName}
			for rel := range files {
				if !strings.HasPrefix(strings.ToLower(rel), "sdk/emulator/") {
					old = append(old, rel)
				}
			}
			for _, rel := range old {
				if !newKeys[strings.ToLower(rel)] && isFile(l.at(rel)) {
					p.delete = append(p.delete, rel)
				}
			}
			sort.Strings(p.delete)
		}
	}
	return p, nil
}

func inFold(list []string, rel string) bool {
	for _, x := range list {
		if strings.EqualFold(x, rel) {
			return true
		}
	}
	return false
}

// backupRootFiles copies the files the plan replaces or deletes into previous\root (copies: the originals stay until
// they are replaced, so a crash before that changes nothing).
func (l layout) backupRootFiles(p rootPlan) error {
	for _, rel := range append(append([]string{}, p.replace...), p.delete...) {
		if inFold(p.created, rel) || !isFile(l.at(rel)) || lexists(l.prevAt(rel)) {
			continue
		}
		if err := copyFile(l.at(rel), l.prevAt(rel)); err != nil {
			return err
		}
	}
	return nil
}

// applyRootFiles moves every file still in pkg\ over the root's and deletes the plan's deletions; re-runnable. A
// target that is the running program itself (tools\modavd.exe running this update) cannot be replaced in place: it
// is renamed into the trash first (a running image can be renamed), its copy in previous\root still restores it.
func (l layout) applyRootFiles(p rootPlan) error {
	self := selfExe()
	for _, rel := range p.replace {
		src := l.pkgAt(rel)
		if !isFile(src) {
			continue
		}
		dst := l.at(rel)
		if self != "" && strings.EqualFold(dst, self) && isFile(dst) {
			if err := l.toTrash(dst); err != nil {
				return err
			}
		}
		if err := replaceFile(src, dst); err != nil {
			return err
		}
	}
	for _, rel := range p.delete {
		dst := l.at(rel)
		os.Chmod(dst, 0o666)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// restoreRootFiles undoes applyRootFiles from previous\root.
func (l layout) restoreRootFiles(p rootPlan) error {
	for _, rel := range p.created {
		if err := os.Remove(l.at(rel)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	self := selfExe()
	for _, rel := range append(append([]string{}, p.replace...), p.delete...) {
		if !isFile(l.prevAt(rel)) {
			continue
		}
		dst := l.at(rel)
		if self != "" && strings.EqualFold(dst, self) && isFile(dst) {
			if err := l.toTrash(dst); err != nil {
				return err
			}
		}
		if err := replaceFile(l.prevAt(rel), dst); err != nil {
			return err
		}
	}
	return nil
}

func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}

// ---- swap

// swapIn is AUTO-MAS swap_in: the two renames of sdk\emulator with the root files, each step recorded first.
// Returns the installed version.
func (u *Updater) swapIn() (string, error) {
	l := u.l
	st := l.readState()
	if err := l.checkStaged(st); err != nil {
		return "", err
	}
	if _, err := u.Root.ReadMetadata(); err != nil {
		return "", errs.Newf(errs.NotReady, "%s。先修好或删掉它再更新，什么都没动", errs.As(err).Message)
	}
	if err := u.ensureNotInUse("装"); err != nil {
		return "", err
	}
	from := ""
	if v, ok := LocalVersion(u.Root); ok {
		from = v.String()
	}
	if err := l.toTrash(l.previous()); err != nil {
		return "", swapBlocked("上次留下、要删的旧版文件还被占用（%v），什么都没动，稍后再试", err)
	}
	l.purgeTrash()
	plan, err := l.planRootFiles()
	if err != nil {
		return "", err
	}
	if err := l.backupRootFiles(plan); err != nil {
		return "", err
	}
	st.Set("phase", jsonord.NewString(phaseSwapping))
	st.Set("plan", plan.value())
	st.Set("swap", stepVal(stepBegin))
	st.Set("from", jsonord.NewString(from))
	if err := l.writeState(st); err != nil {
		return "", err
	}
	// From here on recover() (ours or AUTO-MAS's) owns the outcome.
	if err := os.MkdirAll(l.previous(), 0o755); err != nil {
		return "", u.recoverAfter(err)
	}
	if err := rename(l.emulator(), l.previousEmulator()); err != nil {
		l.backToStaged(st)
		return "", swapBlocked("模拟器文件还在被占用（%v），什么都没动。请先关掉所有实例和用到模拟器的程序再装", err)
	}
	st.Set("swap", stepVal(stepOldMoved))
	if err := l.writeState(st); err != nil {
		return "", u.recoverAfter(err)
	}
	if err := rename(l.stageEmulator(), l.emulator()); err != nil {
		if rerr := rename(l.previousEmulator(), l.emulator()); rerr != nil {
			return "", u.recoverAfter(fmt.Errorf("%v；换回旧版也失败: %v", err, rerr))
		}
		l.backToStaged(st)
		return "", swapBlocked("换上新版失败，已恢复原样: %v", err)
	}
	st.Set("swap", stepVal(stepNewMoved))
	if err := l.writeState(st); err != nil {
		return "", u.recoverAfter(err)
	}
	if err := l.applyRootFiles(plan); err != nil {
		return "", u.recoverAfter(err)
	}
	version, _ := st.Get("target").Obj().Get("version").Str()
	if err := u.finishInstall(st); err != nil {
		return "", u.recoverAfter(err)
	}
	return version, nil
}

func stepVal(step string) *jsonord.Value {
	o := jsonord.NewObject()
	o.Obj().Set("step", jsonord.NewString(step))
	return o
}

// recoverAfter: an error after the point of no return; recover at once, as AUTO-MAS _recover_now does.
func (u *Updater) recoverAfter(err error) error {
	msg, rerr := u.recover()
	switch {
	case rerr != nil:
		msg = fmt.Sprintf("当场恢复也失败了（%v），下次更新或 AUTO-MAS 启动时会再按记录恢复", rerr)
	case msg == "":
		msg = "状态已经完整"
	}
	return errs.Wrap(errs.Internal, err, fmt.Sprintf("换上中途出错（%v）；%s", err, msg))
}

// backToStaged: nothing was swapped after all; the stage stays installable.
func (l layout) backToStaged(st *jsonord.Obj) {
	st.Set("phase", jsonord.NewString(phaseStaged))
	st.Set("swap", jsonord.NewNull())
	st.Set("plan", jsonord.NewNull())
	l.writeState(st)
	l.toTrash(l.previous())
	l.purgeTrash()
}

// finishInstall records the update in mas-avd.json first, then goes idle, then cleans up.
func (u *Updater) finishInstall(st *jsonord.Obj) error {
	l := u.l
	target := st.Get("target").Obj()
	version, _ := target.Get("version").Str()
	from, _ := st.Get("from").Str()
	source, _ := target.Get("source").Str()
	asset, _ := target.Get("asset").Obj().Get("name").Str()
	u.recordHistory(from, version, source, asset)
	res := jsonord.NewObject().Obj()
	res.Set("action", jsonord.NewString("update"))
	res.Set("ok", jsonord.NewBool(true))
	res.Set("version", jsonord.NewString(version))
	if err := l.writeState(newState(phaseIdle, "lastResult", objVal(res))); err != nil {
		return err
	}
	l.tidyUp(l.previous(), l.pkg(), l.stage(), l.download())
	l.purgeTrash()
	return nil
}

func (l layout) tidyUp(paths ...string) {
	for _, p := range paths {
		l.toTrash(p) // a path still held stays for the next time
	}
}

// recordHistory is AUTO-MAS _record_history: components.emulator and an updateHistory entry (last 20, a repeat of the
// last entry not added). An unreadable mas-avd.json is never overwritten.
func (u *Updater) recordHistory(from, to, source, asset string) {
	err := u.Root.EditMetadata(func(o *jsonord.Obj) (bool, error) {
		var history []*jsonord.Value
		for _, it := range o.Get("updateHistory").Items() {
			if it.Kind() == jsonord.Object {
				history = append(history, it)
			}
		}
		if n := len(history); n > 0 {
			last := history[n-1].Obj()
			a, _ := last.Get("action").Str()
			f, _ := last.Get("from").Str()
			t, _ := last.Get("to").Str()
			if a == "update" && f == from && t == to {
				return false, nil
			}
		}
		entry := jsonord.NewObject()
		e := entry.Obj()
		e.Set("action", jsonord.NewString("update"))
		e.Set("at", jsonord.NewString(root.NowISO()))
		e.Set("from", jsonord.NewString(from))
		e.Set("to", jsonord.NewString(to))
		e.Set("source", jsonord.NewString(source))
		e.Set("asset", jsonord.NewString(asset))
		history = append(history, entry)
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
		components := o.Get("components")
		if components.Kind() != jsonord.Object {
			components = jsonord.NewObject()
		} else {
			components = components.Clone()
		}
		components.Obj().Set("emulator", jsonord.NewString(to))
		o.Set("components", components)
		o.Set("updateHistory", jsonord.NewArray(history...))
		return true, nil
	})
	if err != nil {
		u.Log.Warnf("%s，这次不记更新记录", errs.As(err).Message)
	}
}

// recover is AUTO-MAS recover(): finish or undo a swap that was cut off, drop a preparation that was, clean up
// leftovers. The message says what it did ("" when nothing).
func (u *Updater) recover() (string, error) {
	l := u.l
	st := l.readState()
	phase := phaseOf(st)
	var notes []string
	if phase == phasePreparing {
		if err := l.abortStaging("上次准备新版没做完"); err != nil {
			return "", err
		}
		notes = append(notes, "上次准备新版没做完，已丢掉暂存（下载的包还在）")
		phase = phaseIdle
	}
	if phase == phaseSwapping {
		P, S, O := isDir(l.emulator()), isDir(l.stageEmulator()), isDir(l.previousEmulator())
		step, _ := st.Get("swap").Obj().Get("step").Str()
		V := truthy(st.Get("staged").Obj().Get("verified"))
		version, _ := st.Get("target").Obj().Get("version").Str()
		plan := planOf(st.Get("plan"))
		switch {
		case !P && V && S:
			if err := rename(l.stageEmulator(), l.emulator()); err != nil {
				return "", err
			}
			if err := l.applyRootFiles(plan); err != nil {
				return "", err
			}
			if err := u.finishInstall(st); err != nil {
				return "", err
			}
			return fmt.Sprintf("补完了上次没装完的更新（%s）", displayVersion(version)), nil
		case !P && O:
			if err := rename(l.previousEmulator(), l.emulator()); err != nil {
				return "", err
			}
			if err := l.restoreRootFiles(plan); err != nil {
				return "", err
			}
			res := jsonord.NewObject().Obj()
			res.Set("action", jsonord.NewString("update"))
			res.Set("ok", jsonord.NewBool(false))
			res.Set("error", jsonord.NewString("上次更新没装完，已退回旧版"))
			if err := l.writeState(newState(phaseIdle, "lastResult", objVal(res))); err != nil {
				return "", err
			}
			l.tidyUp(l.previous(), l.pkg(), l.stage())
			l.purgeTrash()
			return "上次更新没装完，已退回旧版", nil
		case !P:
			return `sdk\emulator 不见了，也没有可用的新版或旧版`, nil
		case step == stepNewMoved || (!S && O):
			if err := l.applyRootFiles(plan); err != nil {
				return "", err
			}
			if err := u.finishInstall(st); err != nil {
				return "", err
			}
			return fmt.Sprintf("补完了上次没装完的更新（%s）", displayVersion(version)), nil
		default:
			l.backToStaged(st)
			return "上次更新还没开始换，已回到「待安装」", nil
		}
	}
	if lexists(l.previous()) {
		l.tidyUp(l.previous())
		notes = append(notes, "丢掉了上次没删掉的旧版")
	}
	l.purgeTrash()
	return strings.Join(notes, "；"), nil
}

func truthy(v *jsonord.Value) bool {
	switch v.Kind() {
	case jsonord.Null:
		return false
	case jsonord.Bool:
		b, _ := v.BoolVal()
		return b
	case jsonord.String:
		s, _ := v.Str()
		return s != ""
	case jsonord.Number:
		lit, _ := v.NumberLiteral()
		return strings.Trim(lit, "0.-") != ""
	case jsonord.Array:
		return len(v.Items()) > 0
	case jsonord.Object:
		return v.Obj().Len() > 0
	}
	return false
}

// ---- who is using the root's programs

// processUse is a process that uses this root's sdk\emulator or runs one of its two programs.
type processUse struct {
	PID     uint32
	Name    string
	Unknown bool // its path could not be read; matched by name
}

// emulatorNames are the image names that count as the emulator's when a process's path cannot be read (another
// user's, a protected one). Not crashpad_handler (AUTO-MAS lists it): every Chromium-based service has one, often
// running as SYSTEM, and the emulator's own runs only beside its emulator.exe / qemu, which are caught anyway.
var emulatorNames = []string{"qemu-system", "emulator", "netsimd"}

// processesUsing lists the processes whose image is under <root>\sdk\emulator (emulator, qemu, netsimd ...): running
// exes do not stop a directory rename, so they are checked first. With programs, also the other processes running the
// root's ModAVD.exe or tools\modavd.exe (the manager, MCP servers, helpers), which the swap replaces.
func (u *Updater) processesUsing(programs bool) ([]processUse, error) {
	if u.scan != nil {
		return u.scan(programs)
	}
	var prefixes []string
	for _, p := range []string{l2abs(u.l.emulator()), realOrSame(u.l.emulator())} {
		prefixes = append(prefixes, strings.ToLower(p)+`\`)
	}
	progs := map[string]bool{}
	for _, rel := range []string{"ModAVD.exe", "tools/modavd.exe"} {
		progs[strings.ToLower(realOrSame(u.l.at(rel)))] = true
	}
	procs, err := win.ListProcesses()
	if err != nil {
		return nil, err
	}
	self := uint32(os.Getpid())
	var out []processUse
	for _, p := range procs {
		if p.PID == self || p.PID == 0 {
			continue
		}
		path, err := p.ImagePath()
		if err != nil {
			name := strings.ToLower(p.Name)
			for _, n := range emulatorNames {
				if strings.HasPrefix(name, n) {
					out = append(out, processUse{p.PID, p.Name, true})
					break
				}
			}
			continue
		}
		lp := strings.ToLower(realOrSame(path))
		hit := progs[lp] && programs
		for _, pre := range prefixes {
			if strings.HasPrefix(lp, pre) {
				hit = true
			}
		}
		if hit {
			out = append(out, processUse{PID: p.PID, Name: p.Name})
		}
	}
	return out, nil
}

func l2abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func realOrSame(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return l2abs(p)
}

func (u *Updater) ensureNotInUse(verb string) error {
	uses, err := u.processesUsing(true)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "查不了有没有程序在用模拟器文件，什么都没动: "+err.Error())
	}
	if len(uses) == 0 {
		return nil
	}
	var parts []string
	for _, p := range uses {
		s := fmt.Sprintf("%s（pid %d", p.Name, p.PID)
		if p.Unknown {
			s += "，读不到路径"
		}
		parts = append(parts, s+"）")
	}
	return swapBlocked("还有程序在用模拟器或管理器的文件：%s。什么都没动，请先关掉所有实例、管理器窗口和 MCP 客户端再%s",
		strings.Join(parts, "、"), verb)
}
