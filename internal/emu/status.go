//go:build windows

package emu

// Queries: list / info and the InstanceInfo every result carries. Port of AUTO-MAS manager.py getInfo / _status_of /
// _device_info / instance_options.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"modavd/internal/avd"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

// qemuScan, when a test sets it, replaces the process scan behind List, Info, Running and qemuOf (not findQemu), so
// a test sees exactly the qemu processes it wants. nil: the real scan of the Manager's root. The real scan does not
// take another root's mas_<i> for the test root's (adb.QemuBelongsTo: the runtime file in
// %LOCALAPPDATA%\Temp\avd\running, before it exists the run lock in the instance directory), but a test should not
// depend on what runs on the machine either.
var qemuScan func(indexes []int) map[int]win.Process

// qemus is the process scan of the queries: qemuScan when a test set it, else m.scanQemu.
func (m *Manager) qemus(indexes []int) map[int]win.Process {
	if qemuScan != nil {
		return qemuScan(indexes)
	}
	return m.scanQemu(indexes)
}

// qemuOf is the qemu process of instance i of m's root, through qemuScan.
func (m *Manager) qemuOf(i int) (win.Process, bool) {
	p, ok := m.qemus([]int{i})[i]
	return p, ok
}

// Running is the indexes of this root's instances whose qemu runs, ascending: one process scan and no adb at all
// (AUTO-MAS close_running_instances takes its targets from _scan_processes the same way). stop --all uses it: List
// would first start the private server and ask every instance, up to 5 s each, out of a 30 s shutdown budget.
func (m *Manager) Running(ctx context.Context) ([]int, error) {
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
	procs := m.qemus(indexes)
	running := make([]int, 0, len(procs))
	for _, i := range indexes {
		if _, ok := procs[i]; ok {
			running = append(running, i)
		}
	}
	slices.Sort(running)
	return slices.Compact(running), nil
}

// List is every instance with its status (one process scan, getprop only for running ones).
func (m *Manager) List(ctx context.Context) (*ListResult, error) {
	insts, err := avd.List(m.Root)
	if err != nil {
		return nil, err
	}
	indexes := make([]int, len(insts))
	for k, in := range insts {
		indexes[k] = in.Index
	}
	procs := m.qemus(indexes)
	res := &ListResult{RootInfo: m.rootInfo(), Instances: make([]InstanceInfo, len(insts))}
	// AUTO-MAS asks the running instances one after another; each getprop may take up to statusAdbTimeout, so ask them
	// all at once (re-registration is serialized per serial inside adb.Client anyway). A private server that is down
	// is started once here first, not by every getprop at the same time. Its failure shows in the getprops.
	if len(procs) > 0 {
		m.ADB.EnsureServer(ctx, privateServerWait)
	}
	var wg sync.WaitGroup
	for k, in := range insts {
		var p *win.Process
		if q, ok := procs[in.Index]; ok {
			p = &q
		}
		wg.Go(func() { res.Instances[k] = m.instanceInfo(ctx, in, p, nil) })
	}
	wg.Wait()
	return res, nil
}

// Info is one instance. errs.NotFound when it does not exist.
func (m *Manager) Info(ctx context.Context, i int) (*InstanceInfo, error) {
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	var p *win.Process
	if q, ok := m.qemuOf(i); ok {
		p = &q
	}
	info := m.instanceInfo(ctx, in, p, nil)
	return &info, nil
}

// instanceInfo builds the InstanceInfo of in. p is its qemu process when running. booted, when not nil, is the
// already known boot state (skips the getprop).
func (m *Manager) instanceInfo(ctx context.Context, in *avd.Instance, p *win.Process, booted *bool) InstanceInfo {
	i := in.Index
	console := root.ConsolePort(i)
	info := InstanceInfo{
		Index:      i,
		Name:       in.Name,
		Title:      in.Title,
		Serial:     root.Serial(i),
		AdbAddress: fmt.Sprintf("127.0.0.1:%d", root.AdbPort(i)),
		Ports:      Ports{Console: console, Adb: root.AdbPort(i), Grpc: root.GrpcPort(i)},
		Screencap:  Screencap{Shm: shmName(i)},
		Path:       in.Dir,
	}
	if p != nil {
		info.Running = true
		info.PID = int(p.PID)
		if t := p.StartedAt(); !t.IsZero() {
			info.StartedAt = t.Format(time.RFC3339)
			info.StartedAtMS = t.UnixMilli()
		}
		if booted != nil {
			info.Booted = *booted
		} else {
			info.Booted = m.bootCompleted(ctx, i, statusAdbTimeout, true)
		}
		// Only for a running instance: the name is per console port, not per root, so an offline instance would
		// report another root's mapping on the same port.
		info.Screencap.ShmReady = win.SharedMemoryExists(info.Screencap.Shm)
	}
	info.Status = statusOf(info.Running, info.Booted)
	info.Busy = busyOf(m.stopping(i), m.lockHeld(i, "start"), m.lockHeld(i, "bridge"))

	cfg := InstanceConfig{MemoryMB: avd.MemoryFor(m.Root, i), FPS: in.FPS()}
	if n, ok := in.CPU(); ok {
		cfg.CPU = n
	}
	if n, ok := in.DataPartitionGB(); ok {
		cfg.DataPartitionGB = n
	}
	cfg.Width, cfg.Height, cfg.DPI = in.Display()
	info.Config = cfg

	if meta := m.Root.InstanceMeta(i); meta != nil {
		info.Initialized = metaTruthy(meta.Get("initialized"))
		info.Launcher = metaText(meta.Get("launcher"))
		info.Renderer = metaText(meta.Get("renderer"))
		info.SoftwareRenderer = metaTruthy(meta.Get("softwareRenderer"))
	}
	info.Bridge = m.bridgeState(ctx, i, info.Booted, info.Initialized)
	return info
}

// statusOf maps the two observations to a Status (AUTO-MAS _status_of without the freeze marker, which only lives in
// AUTO-MAS's watchdog).
func statusOf(running, booted bool) Status {
	switch {
	case !running:
		return StatusOffline
	case booted:
		return StatusOnline
	default:
		return StatusStarting
	}
}

// busyOf: a stop that runs wins over a start (a stop does not wait for a start, and ends it). The bridge install
// wins over both: it stops and starts the instance itself, a cold boot or two, while holding the bridge lock.
func busyOf(stopping, starting, bridging bool) string {
	switch {
	case bridging:
		return "bridging"
	case stopping:
		return "stopping"
	case starting:
		return "starting"
	default:
		return ""
	}
}

// Bridge states (InstanceInfo.Bridge).
const (
	bridgeStateOff       = "off"       // instances.<i>.bridge is false: no start installs it
	bridgeStatePending   = "pending"   // wanted and not there yet: the next start modavd launches installs it
	bridgeStateInstalled = "installed" // the booted guest runs it
	bridgeStateUnknown   = "unknown"   // off or still booting with the marker: installed, or not yet (see bridgeState)
)

// bridgeState is InstanceInfo.Bridge. Only a booted guest tells installed from not (one getprop, statusAdbTimeout).
// The boot marker stays for as long as the bridge is installed (it keeps the system writable), so on an instance that
// is off or still booting the marker alone means "installed, or the last install failed": unknown. Pending there is
// what a start would install for sure: no marker yet (it places one first), or the marker of an instance whose first
// boot (which comes before the install) has not run.
func (m *Manager) bridgeState(ctx context.Context, i int, booted, initialized bool) string {
	if !m.bridgeWanted(i) {
		return bridgeStateOff
	}
	if booted {
		switch b := m.guestBridgeWithin(ctx, i, statusAdbTimeout); {
		case b == bridgeProp:
			return bridgeStateInstalled
		case b != "":
			return bridgeStatePending
		}
		return bridgeStateUnknown
	}
	if !regularFile(bridgeMarker(m.Root, i)) || !initialized {
		return bridgeStatePending
	}
	return bridgeStateUnknown
}

// lockHeld: some process (this one included: the lock is per handle) holds lock kind of instance i right now.
func (m *Manager) lockHeld(i int, kind string) bool {
	l, ok, err := win.TryLock(m.lockPath(i, kind))
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	l.Unlock()
	return false
}

// metaTruthy is Python's bool() of a JSON value: AUTO-MAS reads these flags with bool(meta.get(key, False)).
func metaTruthy(v *jsonord.Value) bool {
	switch v.Kind() {
	case jsonord.Bool:
		b, _ := v.BoolVal()
		return b
	case jsonord.Number:
		if n, ok := v.Int(); ok {
			return n != 0
		}
		if num, ok := v.ToGo().(json.Number); ok {
			f, err := num.Float64()
			return err != nil || f != 0
		}
		return true
	case jsonord.String:
		s, _ := v.Str()
		return s != ""
	case jsonord.Array:
		return len(v.Items()) > 0
	case jsonord.Object:
		o := v.Obj()
		return o != nil && o.Len() > 0
	default:
		return false
	}
}

// metaText is a string value, "" for anything else (AUTO-MAS meta.get(key) or "").
func metaText(v *jsonord.Value) string {
	if v.Kind() != jsonord.String {
		return ""
	}
	s, _ := v.Str()
	return s
}
