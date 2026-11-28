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

func (winWindows) Windows(pid uint32) ([]uintptr, error)         { return win.EmulatorWindows(pid) }
func (winWindows) IsToolbar(h uintptr) bool                      { return win.IsEmulatorToolbar(h) }
func (winWindows) SetEnabled(h uintptr, on bool) bool            { return win.SetWindowEnabled(h, on) }
func (winWindows) Enabled(h uintptr) bool                        { return win.IsWindowEnabled(h) }
func (winWindows) Identity(h uintptr) (pid uint32, class string) { return win.WindowIdentity(h) }

// RunBootOverlay is `<exe> __bootoverlay <root> <i> <start pid> <start created> <round>` (see overlay.go), started by
// startBootOverlay: it covers the windows of instance i's qemu until the instance is online, and returns the exit code.
// Its messages go to w (stderr: <root>\logs\modavd-bootoverlay.log).
func RunBootOverlay(args []string, w io.Writer) int {
	log := logx.New(logx.JSONSink(w, logx.Info))
	if len(args) != 5 {
		log.Errorf("开机遮罩的参数不对: %q", args)
		return 2
	}
	i, err1 := strconv.Atoi(args[1])
	pid, err2 := strconv.ParseUint(args[2], 10, 32)
	created, err3 := strconv.ParseUint(args[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || !root.ValidIndex(i) || pid == 0 || args[4] == "" {
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
	return newOverlayRun(m, i, log, host.Process{PID: uint32(pid), Created: created}, args[4]).run()
}

// overlayRun is one overlay process.
type overlayRun struct {
	m      *Manager
	i      int
	log    *logx.Logger
	ops    overlayWindows
	watch  host.Process // the process that started it, waited for with a handle
	round  string
	screen bootscreen.Screen
	keeper *windowKeeper
	// what it looks at (the tests replace them): the qemu of the instance, and the guest's boot state
	find   func() (host.Process, bool, error)
	probe  func(ctx context.Context) (booted, answered bool)
	bootID func(ctx context.Context) string // the guest's boot_id ("" when it cannot be read)
	fixed  atomic.Pointer[overlayOut]       // tests: publish this instead of the machine's decision (pictures of a state)

	start time.Time
	// the poller's last answer, read by the window thread
	out  atomic.Pointer[overlayOut]
	qemu atomic.Pointer[host.Process] // the qemu the windows belong to: published whole, so its identity is never half of one
	// an end was asked for (by the poller, a signal, a close): a latch the window thread looks at on every turn, and the
	// keeper looks at before every disable
	quitReq   atomic.Bool
	watchGone atomic.Bool // the process that started it has ended (a handle wait says so)

	// owned by the window thread
	released   bool // the windows were given back (ready, or the cap)
	ease       bootscreen.Ease
	lastEase   time.Time
	shownPct   float64
	indet      bool // no real percentage: the bar is indeterminate
	title      string
	note       string
	frame      *image.RGBA
	lastDraw   time.Time
	lastWindow time.Time
	watched    uint32
}

func newOverlayRun(m *Manager, i int, log *logx.Logger, watch host.Process, round string) *overlayRun {
	r := &overlayRun{m: m, i: i, log: log, ops: overlayOS, watch: watch, round: round, screen: bootscreen.NewPlaceholder(),
		find:   func() (host.Process, bool, error) { return m.findQemu(i) },
		probe:  func(ctx context.Context) (bool, bool) { return m.bootProbe(ctx, i, 2*time.Second, false) },
		bootID: func(ctx context.Context) string { _, id, _ := m.guestBoot(ctx, i, false); return id }}
	return r
}

func (r *overlayRun) run() int {
	r.start = time.Now()
	r.lastEase = r.start
	om := newOverlayMachine(r.start)
	first := overlayOut{Stage: "launch", Percent: -1}
	r.out.Store(&first)
	me, _ := host.FindProcess(uint32(os.Getpid())) // zero when unreadable: the keeper then disables nothing
	// windows an overlay that is gone left disabled are given back before this one disables any
	if _, done := r.m.recoverOverlayMark(r.i, "", r.ops); !done {
		r.log.Warnf("实例 %d 上一个遮罩留下的窗口还没恢复完", r.i)
	}
	r.keeper = newWindowKeeper(r.m, r.i, r.ops, r.log, r.round, me, func(q host.Process) bool { return r.m.watchRunningFor(r.i, q) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var o *win.Overlay
	var oMu sync.Mutex
	// quit asks the loop to end: the latch (the window thread and the keeper look at it) first, then the message
	quit := func() {
		r.quitReq.Store(true)
		r.keeper.Stop()
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
	defer r.keeper.Close("exit", 3*time.Second)
	err := win.RunOverlay(win.OverlayOptions{
		TickEvery: 33 * time.Millisecond,
		Tick:      r.tick,
		Wake: func(ov *win.Overlay) {
			oMu.Lock()
			o = ov
			oMu.Unlock()
			if r.quitReq.Load() { // an end was asked for before the loop began
				ov.Quit()
				return
			}
			r.maintain(ov, time.Now())
		},
		Paint: r.paint,
		OnClose: func(why string) {
			r.log.Infof("实例 %d 开机遮罩被要求结束（%s）", r.i, why)
			r.quitReq.Store(true)
			r.keeper.Close(why, time.Second) // the process may be ended right after the answer: the windows go back now
		},
	})
	cancel()
	if err != nil {
		r.log.Errorf("实例 %d 开机遮罩出错: %v", r.i, err)
		return 1
	}
	r.log.Infof("实例 %d 开机遮罩结束，用了 %.1f 秒", r.i, time.Since(r.start).Seconds())
	return 0
}

// overlaySnap is what the slow looks (adb) found out, read by the poller without waiting for them.
type overlaySnap struct {
	booted, answered atomic.Bool
	bootID           atomic.Pointer[string]
	stop             atomic.Bool // the poller has what it needed
}

func (s *overlaySnap) guestBootID() string {
	if p := s.bootID.Load(); p != nil {
		return *p
	}
	return ""
}

// probeLoop is the slow looking, on its own goroutine so that nothing the poller decides ever waits for adb: once a
// second, whether Android has booted (a getprop) and, from then on, the guest's boot_id (what bootSetupOf compares a
// record with).
func (r *overlayRun) probeLoop(ctx context.Context, s *overlaySnap) {
	for {
		if r.qemu.Load() != nil && !s.stop.Load() {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if !s.booted.Load() {
				b, a := r.probe(pctx)
				s.answered.Store(a)
				s.booted.Store(b)
			} else if id := r.bootID(pctx); id != "" {
				s.bootID.Store(&id)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// poll is the looking: every 100 ms what status would look at, the decision of the machine, published for the window
// thread. It also ends the overlay (quit) when the machine says so. It never waits for adb (probeLoop), for the other
// process's windows (windowKeeper) or for the process that started the overlay (a handle wait).
func (r *overlayRun) poll(ctx context.Context, om *overlayMachine, quit func()) {
	snap := &overlaySnap{}
	go r.probeLoop(ctx, snap)
	starter := make(chan struct{}, 1)
	go func() {
		r.watch.Wait(6 * time.Hour) // a handle to the process that started it: returns the moment it ends
		r.watchGone.Store(true)
		select {
		case starter <- struct{}{}:
		default:
		}
	}()
	var lastScan time.Time
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
					pp := p
					r.qemu.Store(&pp)
				}
			}
		}
		startHeld := r.m.lockHeld(r.i, "start")
		booted := snap.booted.Load()
		in := overlayIn{Now: now, QemuFound: found, Answered: snap.answered.Load(), Booted: booted, StartHeld: startHeld,
			WatchAlive: !r.watchGone.Load(), Tracked: r.roundTracked()}
		if found && booted {
			in.Setup = r.m.bootSetupOf(r.i, q, startHeld, now, snap.guestBootID, r.round)
		}
		out := om.Next(in)
		if f := r.fixed.Load(); f != nil {
			out = *f
			out.Decided = true
		}
		r.out.Store(&out)
		if out.Release && in.Setup.Ready {
			snap.stop.Store(true)
		}
		if out.Exit {
			r.log.Infof("实例 %d 开机遮罩该结束了: %s", r.i, out.Reason)
			quit()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-starter:
		case <-t.C:
		}
	}
}

// roundTracked: this round's setup marker (written by the start before it started this overlay) or setup record is on disk.
func (r *overlayRun) roundTracked() bool {
	if mk, ok := r.m.readSetupMarker(r.i); ok && mk.Round == r.round {
		return true
	}
	rec, ok := r.m.readReady(r.i)
	return ok && rec.Round == r.round
}

// tick runs on the window thread about every 33 ms.
func (r *overlayRun) tick(o *win.Overlay) {
	if r.quitReq.Load() {
		o.Quit() // the latch: the request is not only a message
		return
	}
	now := time.Now()
	out := r.out.Load()
	if out == nil {
		return
	}
	if out.Decided {
		r.keeper.Decide()
	}
	if out.Release && !r.released {
		r.release(o)
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

// maintain makes the covers fit the emulator's windows now and tells the keeper which windows have a cover.
func (r *overlayRun) maintain(o *win.Overlay, now time.Time) {
	r.lastWindow = now
	qp := r.qemu.Load()
	if qp == nil {
		return
	}
	q := *qp
	pid := q.PID
	if pid != r.watched {
		o.Watch(pid) // its window events wake us at once: a move, a minimize, the boss key, the z-order
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
	if r.released || o.Quitting() || r.quitReq.Load() {
		return
	}
	// Covers first, then the windows stop taking input (the keeper does that, on its own goroutine): no moment in which
	// neither holds.
	var covered []uintptr
	for _, s := range specs {
		if o.CoverOf(s.Owner) != 0 {
			covered = append(covered, s.Owner)
		}
	}
	r.keeper.Track(q, covered)
}

// release turns the covers into a layer the mouse goes through (the input is free at once, whatever the other process's
// windows do) and has the keeper give the windows back, in the background; the picture goes on being drawn (the cap's note).
func (r *overlayRun) release(o *win.Overlay) {
	r.released = true
	o.SetPassThrough(true)
	r.keeper.Release()
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
