package emu

import (
	"context"
	"slices"

	"modavd/internal/avd"
	"modavd/internal/bosskey"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// Boss keys (ModAVD/ModAVD#101): every instance has a global hot key (bosskey) that hides or shows the windows of
// the instances using it. The manager registers the keys (internal/ui) and calls ToggleBossKey when one is pressed;
// the windows are shown and hidden by SetWindowVisible, like `modavd window`, so it does not matter who hid a window
// last. Windows only, like the window operations.

// bossKeyOf is instance i's boss key ("" = none, and on other platforms).
func (m *Manager) bossKeyOf(i int) string {
	if !root.CurrentPlatform().Windows() {
		return ""
	}
	key, err := bosskey.Of(m.Root.InstanceMeta(i), i)
	if err != nil {
		m.Log.Debugf("%v", err)
	}
	return key
}

// setBossKey stores key (already Normalize'd; "" = none) as instances.<i>.bossKey, under the instance's config lock so
// a delete cannot write the instance back. AUTO-MAS keeps keys it does not know when it writes mas-avd.json, and a
// delete takes the key with the rest of instances.<i>.
func (m *Manager) setBossKey(i int, key string) error {
	l, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return err
	}
	defer l.Unlock()
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(bosskey.MetaKey, jsonord.NewString(key))
		return nil
	})
}

// BossKeyTarget is one instance as the boss keys see it.
type BossKeyTarget struct {
	Index   int
	BossKey string // "" = none
	// Windowed: it runs with the emulator window (a headless one has no window: its key does nothing for it).
	Windowed bool
	PID      uint32 // its qemu while running
}

// qemuWindowedOf is qemuWindowed; a test replaces it.
var qemuWindowedOf = qemuWindowed

// emulatorWindowsShown is host.EmulatorWindowsShown; a test replaces it.
var emulatorWindowsShown = host.EmulatorWindowsShown

// BossKeyTargets lists the instances with their boss keys and whether they run with a window: no adb, only the
// instance files and a process scan, so the manager can ask every few seconds.
func (m *Manager) BossKeyTargets(ctx context.Context) ([]BossKeyTarget, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	insts, err := avd.List(m.Root)
	if err != nil {
		return nil, err
	}
	indexes := make([]int, len(insts))
	for k, in := range insts {
		indexes[k] = in.Index
	}
	procs, err := m.qemus(indexes)
	if err != nil {
		return nil, err
	}
	out := make([]BossKeyTarget, 0, len(insts))
	for _, in := range insts {
		t := BossKeyTarget{Index: in.Index, BossKey: m.bossKeyOf(in.Index)}
		if p, ok := procs[in.Index]; ok {
			t.PID = p.PID
			t.Windowed, _ = qemuWindowedOf(p)
		}
		out = append(out, t)
	}
	return out, nil
}

// BossKeyToggle is what one press of a boss key did.
type BossKeyToggle struct {
	Key string
	// Visible: the windows were shown (false: hidden).
	Visible bool
	// Instances are those it showed or hid: the windowed running instances with this key, ascending.
	Instances []int
}

// ToggleBossKey is a press of boss key key: among the windowed running instances using it, all windows are hidden
// when one of them shows a window, else all are shown. Whether a window shows is asked of the window itself
// (IsWindowVisible), so a window hidden by AUTO-MAS or `modavd window` comes back too. Nothing to do (no such
// instance) is not an error.
func (m *Manager) ToggleBossKey(ctx context.Context, key string) (*BossKeyToggle, error) {
	targets, err := m.BossKeyTargets(ctx)
	if err != nil {
		return nil, err
	}
	res := &BossKeyToggle{Key: key}
	var pids []uint32
	for _, t := range targets {
		if t.Windowed && key != "" && t.BossKey == key {
			res.Instances = append(res.Instances, t.Index)
			pids = append(pids, t.PID)
		}
	}
	if len(res.Instances) == 0 {
		return res, nil
	}
	anyShown := false
	for _, pid := range pids {
		shown, _, err := emulatorWindowsShown(pid)
		if err != nil {
			if errs.CodeOf(err) == errs.Unsupported {
				return nil, err
			}
			m.Log.Warnf("看不了窗口是否显示: %v", err)
			continue
		}
		anyShown = anyShown || shown
	}
	res.Visible = !anyShown
	if err := m.setWindowsVisible(ctx, res.Instances, res.Visible); err != nil {
		return nil, err
	}
	return res, nil
}

// ShowHiddenWindows shows the windows of every windowed running instance that has one hidden (the tray icon, and
// the manager's exit: hidden windows would otherwise have nothing left to bring them back). Returns the instances
// it showed.
func (m *Manager) ShowHiddenWindows(ctx context.Context) ([]int, error) {
	targets, err := m.BossKeyTargets(ctx)
	if err != nil {
		return nil, err
	}
	var hidden []int
	for _, t := range targets {
		if !t.Windowed {
			continue
		}
		if shown, n, err := emulatorWindowsShown(t.PID); err == nil && n > 0 && !shown {
			hidden = append(hidden, t.Index)
		} else if err != nil && errs.CodeOf(err) == errs.Unsupported {
			return nil, err
		}
	}
	if err := m.setWindowsVisible(ctx, hidden, true); err != nil {
		return nil, err
	}
	return hidden, nil
}

func (m *Manager) setWindowsVisible(ctx context.Context, indexes []int, visible bool) error {
	for _, i := range slices.Sorted(slices.Values(indexes)) {
		if _, err := m.SetWindowVisible(ctx, i, visible); err != nil {
			if errs.CodeOf(err) == errs.Unsupported {
				return err
			}
			m.Log.Warnf("ModAVD 实例 %d 的窗口没切换成: %v", i, err)
		}
	}
	return nil
}
