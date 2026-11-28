//go:build windows

package emu

import (
	"context"
	"image"
	"io"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"modavd/internal/bootscreen"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/i18n"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

func init() { overlayOS = winWindows{} }

// winWindows is overlayWindows on the real windows.
type winWindows struct{}

func (winWindows) Windows(pid uint32) ([]uintptr, error) { return win.EmulatorWindows(pid) }
func (winWindows) IsToolbar(h uintptr) bool              { return win.IsEmulatorToolbar(h) }
func (winWindows) SetEnabled(h uintptr, on bool) bool    { return win.SetWindowEnabled(h, on) }
func (winWindows) Enabled(h uintptr) bool                { return win.IsWindowEnabled(h) }
func (winWindows) ProcessOf(h uintptr) uint32            { return win.WindowProcessID(h) }

// RunBootOverlay is `<exe> __bootoverlay <root> <i> <start pid> <start created>` (see overlay.go), started by
// startBootOverlay: it covers the windows of instance i's qemu until the instance is online, and returns the exit code.
// Its messages go to w (stderr: <root>\logs\modavd-bootoverlay.log).
func RunBootOverlay(args []string, w io.Writer) int {
	log := logx.New(logx.JSONSink(w, logx.Info))
	if len(args) != 4 {
		log.Errorf("开机遮罩的参数不对: %q", args)
		return 2
	}
	i, err1 := strconv.Atoi(args[1])
	pid, err2 := strconv.ParseUint(args[2], 10, 32)
	created, err3 := strconv.ParseUint(args[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || !root.ValidIndex(i) || pid == 0 {
		log.Errorf("开机遮罩的参数不对: %q", args)
		return 2
	}
	r, err := root.New(args[0])
	if err != nil {
		log.Errorf("开机遮罩找不到根目录 %s: %v", args[0], err)
		return errs.ExitCode(err)
	}
	i18n.Init()
	win.SetDPIAware()
	m := New(r, log)
	l, free, err := host.TryLock(m.lockPath(i, "overlay"))
	if err != nil {
		log.Errorf("实例 %d 开机遮罩的锁文件打不开: %v", i, err)
		return 1
	}
	if !free {
		return 0 // the overlay of this instance runs already
	}
	defer l.Unlock()
	return newOverlayRun(m, i, log, host.Process{PID: uint32(pid), Created: created}).run()
}

// overlayRun is one overlay process.
type overlayRun struct {
	m      *Manager
	i      int
	log    *logx.Logger
	ops    overlayWindows
	watch  host.Process
	screen bootscreen.Screen
	// what it looks at (the tests replace them): the qemu of the instance, and the guest's boot state
	find  func() (host.Process, bool, error)
	probe func(ctx context.Context) (booted, answered bool)
	fixed atomic.Pointer[overlayOut] // tests: publish this instead of the machine's decision (pictures of a state)

	start time.Time
	// the poller's last answer, read by the window thread
	out  atomic.Pointer[overlayOut]
	qpid atomic.Uint32 // the qemu the windows belong to
	qcr  atomic.Uint64

	// owned by the window thread
	disabled   map[uintptr]bool // windows this overlay disabled
	released   bool             // the windows were given back (ready, or the cap)
	ease       bootscreen.Ease
	lastEase   time.Time
	shownPct   float64
	indet      bool // no real percentage: the bar is indeterminate
	title      string
	note       string
	frame      *image.RGBA
	lastDraw   time.Time
	lastWindow time.Time
	markDirty  bool
	watched    uint32
}

func newOverlayRun(m *Manager, i int, log *logx.Logger, watch host.Process) *overlayRun {
	return &overlayRun{m: m, i: i, log: log, ops: overlayOS, watch: watch, screen: bootscreen.NewPlaceholder(),
		disabled: map[uintptr]bool{},
		find:     func() (host.Process, bool, error) { return m.findQemu(i) },
		probe:    func(ctx context.Context) (bool, bool) { return m.bootProbe(ctx, i, 2*time.Second, false) }}
}

func (r *overlayRun) run() int {
	r.start = time.Now()
	r.lastEase = r.start
	om := newOverlayMachine(r.start)
	first := overlayOut{Stage: "launch", Percent: -1}
	r.out.Store(&first)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var o *win.Overlay
	var oMu sync.Mutex
	quit := func() {
		oMu.Lock()
		defer oMu.Unlock()
		if o != nil {
			o.Quit()
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			r.log.Infof("实例 %d 开机遮罩收到结束信号", r.i)
			quit()
		case <-ctx.Done():
		}
	}()
	go r.poll(ctx, om, quit)

	// whatever ends the loop (a panic included), the windows are given back and the mark is deleted
	defer r.restore("exit")
	err := win.RunOverlay(win.OverlayOptions{
		TickEvery: 33 * time.Millisecond,
		Tick:      r.tick,
		Wake:      func(ov *win.Overlay) { oMu.Lock(); o = ov; oMu.Unlock(); r.maintain(ov, time.Now()) },
		Paint:     r.paint,
	})
	cancel()
	if err != nil {
		r.log.Errorf("实例 %d 开机遮罩出错: %v", r.i, err)
		return 1
	}
	r.log.Infof("实例 %d 开机遮罩结束，用了 %.1f 秒", r.i, time.Since(r.start).Seconds())
	return 0
}

// poll is the looking: every 100 ms what status would look at, the decision of the machine, published for the window
// thread. It also ends the overlay (quit) when the machine says so.
func (r *overlayRun) poll(ctx context.Context, om *overlayMachine, quit func()) {
	var booted, answered bool
	var lastProbe, lastScan time.Time
	var q host.Process
	var found bool
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		now := time.Now()
		if now.Sub(lastScan) >= 500*time.Millisecond || !found {
			if p, ok, err := r.find(); err == nil {
				q, found = p, ok
				lastScan = now
				if ok {
					r.qpid.Store(p.PID)
					r.qcr.Store(p.Created)
				}
			}
		}
		if found && !booted && now.Sub(lastProbe) >= time.Second {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			booted, answered = r.probe(pctx)
			cancel()
			lastProbe = time.Now()
		}
		startHeld := r.m.lockHeld(r.i, "start")
		in := overlayIn{Now: now, QemuFound: found, Answered: answered, Booted: booted, StartHeld: startHeld,
			WatchAlive: r.watch.Alive()}
		if found && booted {
			in.Setup = r.m.bootSetupOf(r.i, q, startHeld, now, r.m.guestBootIDOf(ctx, r.i))
		}
		out := om.Next(in)
		if f := r.fixed.Load(); f != nil {
			out = *f
		}
		r.out.Store(&out)
		if out.Exit {
			r.log.Infof("实例 %d 开机遮罩该结束了: %s", r.i, out.Reason)
			quit()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick runs on the window thread about every 33 ms.
func (r *overlayRun) tick(o *win.Overlay) {
	now := time.Now()
	out := r.out.Load()
	if out == nil {
		return
	}
	if out.Release && !r.released {
		r.release()
	}
	if now.Sub(r.lastWindow) >= 100*time.Millisecond {
		r.maintain(o, now)
	}
	// the bar and the lines
	r.indet = out.Percent < 0
	if !r.indet {
		r.ease.Step(out.Percent, now.Sub(r.lastEase))
	}
	r.lastEase = now
	r.shownPct = r.ease.Value()
	r.title = overlayTitle(out.Stage)
	r.note = ""
	if out.Note != "" {
		r.note = overlayNote(out.Note)
	}
	if now.Sub(r.lastDraw) >= 50*time.Millisecond {
		r.lastDraw = now
		o.Invalidate()
	}
}

// maintain makes the covers fit the emulator's windows now and disables the windows it has not disabled yet.
func (r *overlayRun) maintain(o *win.Overlay, now time.Time) {
	r.lastWindow = now
	pid := r.qpid.Load()
	if pid == 0 {
		return
	}
	if pid != r.watched {
		o.Watch(pid) // its window events wake us at once: a move, a minimize, the boss key
		r.watched = pid
	}
	wins, err := r.ops.Windows(pid)
	if err != nil {
		return
	}
	var specs []win.CoverSpec
	for _, h := range wins {
		g := win.GeometryOf(h)
		if !g.Exists {
			continue
		}
		spec := win.CoverSpec{Owner: h, Show: g.Visible && !g.Minimized, Main: !r.ops.IsToolbar(h)}
		if spec.Main {
			spec.Rect = g.Client
		} else {
			spec.Rect = g.Frame
		}
		specs = append(specs, spec)
	}
	o.Sync(specs)
	if r.released {
		return
	}
	// Qt may enable its windows again on its own (a restore, a show); while the cover is up they are disabled again at once.
	for h := range r.disabled {
		if r.ops.Enabled(h) {
			r.ops.SetEnabled(h, false)
		}
	}
	// Covers first, then the windows stop taking input: no moment in which neither holds. The mark names a window
	// before it is disabled, so a process that is ended between the two still leaves what the recovery needs.
	var fresh []uintptr
	for _, s := range specs {
		// only a window that takes input now is ours to disable and to give back; one that was disabled before is left as it was
		if !r.disabled[s.Owner] && o.CoverOf(s.Owner) != 0 && r.ops.Enabled(s.Owner) {
			r.disabled[s.Owner] = true
			fresh = append(fresh, s.Owner)
		}
	}
	if len(fresh) > 0 {
		r.writeMark()
		for _, h := range fresh {
			if !r.ops.SetEnabled(h, false) {
				delete(r.disabled, h)
				r.markDirty = true
			}
		}
		if r.markDirty {
			r.writeMark()
		}
	}
}

func (r *overlayRun) writeMark() {
	me, _ := host.FindProcess(uint32(os.Getpid()))
	k := overlayMark{OverlayPID: me.PID, OverlayCreated: me.Created, QemuPID: r.qpid.Load(), QemuCreated: r.qcr.Load()}
	for h := range r.disabled {
		k.Windows = append(k.Windows, uint64(h))
	}
	if err := r.m.writeOverlayMark(r.i, k); err != nil {
		r.log.Warnf("实例 %d 记不下遮罩标记（遮罩和起它的进程都没了的话，窗口要等下次 modavd 查到才恢复）: %v", r.i, err)
		return
	}
	r.markDirty = false
}

// release gives the windows back and deletes the mark; the covers go on being drawn (the cap's note).
func (r *overlayRun) release() {
	r.released = true
	r.restore("release")
}

// restore enables every window this overlay disabled and deletes the mark.
func (r *overlayRun) restore(why string) {
	n := 0
	for h := range r.disabled {
		if r.ops.SetEnabled(h, true) {
			n++
		}
		delete(r.disabled, h)
	}
	r.m.removeOverlayMark(r.i)
	if n > 0 {
		r.log.Infof("实例 %d 开机遮罩把 %d 个模拟器窗口恢复成能操作（%s）", r.i, n, why)
	}
}

// paint draws one frame of a cover (window thread).
func (r *overlayRun) paint(c *win.Canvas, main bool) {
	w, h := c.Size()
	if !main {
		c.Fill(bootscreen.Background)
		return
	}
	if r.frame == nil || r.frame.Bounds().Dx() != w || r.frame.Bounds().Dy() != h {
		r.frame = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	st := bootscreen.State{Progress: r.shownPct, Indeterminate: r.indet, Elapsed: time.Since(r.start), Step: r.title, Note: r.note}
	labels := r.screen.Draw(r.frame, st)
	c.Image(r.frame)
	for _, l := range labels {
		c.TextAligned(l.Text, win.Rect{Left: int32(l.X), Top: int32(l.Y), Right: int32(l.X + l.W), Bottom: int32(l.Y + l.H)}, l.Px, l.RGB, l.Bold, l.Left)
	}
}
