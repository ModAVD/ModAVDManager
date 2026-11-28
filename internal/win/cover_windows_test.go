//go:build windows

package win

import (
	"syscall"
	"testing"
	"time"
	"unsafe"

	"modavd/internal/win/wintest"
)

// The windows of these tests belong to a child process (wintest): windows with the emulator's class names and titles,
// far off the screen, so nothing shows on the desktop and no real window is touched.
type fakeEmulator = wintest.Emulator

func startFakeEmulator(t *testing.T) *fakeEmulator { return wintest.Start(t) }

// overlayHarness runs RunOverlay on its own thread and lets the test run code on it.
type overlayHarness struct {
	o      *Overlay
	ended  chan error
	t      *testing.T
	events chan struct{}
}

func startOverlay(t *testing.T, opt OverlayOptions) *overlayHarness {
	t.Helper()
	h := &overlayHarness{ended: make(chan error, 1), t: t, events: make(chan struct{}, 64)}
	ready := make(chan *Overlay, 1)
	userWake, userTick := opt.Wake, opt.Tick
	opt.Wake = func(o *Overlay) {
		select {
		case ready <- o:
		default:
		}
		if userWake != nil {
			userWake(o)
		}
		select {
		case h.events <- struct{}{}:
		default:
		}
	}
	opt.Tick = func(o *Overlay) {
		if userTick != nil {
			userTick(o)
		}
	}
	go func() { h.ended <- RunOverlay(opt) }()
	select {
	case h.o = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the overlay did not start")
	}
	t.Cleanup(func() {
		h.o.Quit()
		select {
		case <-h.ended:
		case <-time.After(10 * time.Second):
		}
	})
	return h
}

// on runs f on the overlay's thread and waits for it.
func (h *overlayHarness) on(f func(o *Overlay)) {
	h.t.Helper()
	done := make(chan struct{})
	if !h.o.Post(func() { f(h.o); close(done) }) {
		h.t.Fatal("the overlay has ended")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the overlay thread does not answer")
	}
}

func foregroundWindow() uintptr {
	r, _, _ := moduser32.NewProc("GetForegroundWindow").Call()
	return r
}

func center(r Rect) (int32, int32) { return (r.Left + r.Right) / 2, (r.Top + r.Bottom) / 2 }

// specsFor is what an overlay has to say about the fake emulator: a cover over the client area of the main window and
// over the whole toolbar, shown while their windows are shown and not minimized.
func specsFor(f *fakeEmulator) []CoverSpec {
	var specs []CoverSpec
	wins, _ := EmulatorWindows(f.PID)
	for _, w := range wins {
		g := GeometryOf(w)
		if !g.Exists {
			continue
		}
		show := g.Visible && !g.Minimized
		if w == f.Main {
			specs = append(specs, CoverSpec{Owner: w, Rect: g.Client, Show: show, Main: true})
		} else {
			specs = append(specs, CoverSpec{Owner: w, Rect: g.Frame, Show: show})
		}
	}
	return specs
}

// While the covers are up, the point in the middle of the picture area and the point on the toolbar belong to a cover,
// not to the emulator's windows, and the emulator's windows take no input; they come back when enabled again. All of
// it is read with queries (WindowFromPoint, IsWindowEnabled), nothing is clicked or typed.
func TestCoverIsOnTopAndTheEmulatorWindowsTakeNoInput(t *testing.T) {
	f := startFakeEmulator(t)
	wins, err := EmulatorWindows(f.PID)
	if err != nil || len(wins) != 2 {
		t.Fatalf("emulator windows %v %v, want the main window and the toolbar", wins, err)
	}
	before := foregroundWindow()
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)) })

	var mainCover, toolCover uintptr
	h.on(func(o *Overlay) { mainCover, toolCover = o.CoverOf(f.Main), o.CoverOf(f.Tool) })
	if mainCover == 0 || toolCover == 0 {
		t.Fatalf("covers: main %#x toolbar %#x", mainCover, toolCover)
	}
	mainG, toolG := GeometryOf(f.Main), GeometryOf(f.Tool)
	mx, my := center(mainG.Client)
	tx, ty := center(toolG.Frame)
	if got := WindowFromPoint(mx, my); got != mainCover {
		t.Errorf("top window at the middle of the picture area: %#x, want the cover %#x (emulator window %#x)", got, mainCover, f.Main)
	}
	if got := WindowFromPoint(tx, ty); got != toolCover {
		t.Errorf("top window on the toolbar: %#x, want its cover %#x (toolbar %#x)", got, toolCover, f.Tool)
	}
	// the title bar is not covered
	if got := WindowFromPoint(mx, mainG.Frame.Top+5); got != f.Main {
		t.Errorf("top window on the title bar: %#x, want the emulator window %#x", got, f.Main)
	}
	// the cover has no owner (an owner would tie its input queue to the emulator window's thread) and sits right above
	// the emulator window
	if owner := windowOwner(mainCover); owner != 0 {
		t.Errorf("owner of the cover %#x, want none", owner)
	}
	if next := windowAt(mainCover, gwHwndNext); next != f.Main {
		t.Errorf("the window right below the cover is %#x, want the emulator window %#x", next, f.Main)
	}
	if fg := foregroundWindow(); fg == mainCover || fg == toolCover || (fg == f.Main && before != f.Main) {
		t.Errorf("a cover or an emulator window took the foreground (%#x, was %#x)", fg, before)
	}

	for _, w := range wins {
		if !SetWindowEnabled(w, false) {
			t.Fatalf("cannot disable %#x", w)
		}
		if IsWindowEnabled(w) {
			t.Fatalf("%#x still takes input", w)
		}
	}
	for _, w := range wins {
		if !SetWindowEnabled(w, true) || !IsWindowEnabled(w) {
			t.Fatalf("cannot enable %#x again", w)
		}
	}
}

func windowOwner(hwnd uintptr) uintptr { return windowAt(hwnd, gwOwner) }

func windowAt(hwnd, rel uintptr) uintptr {
	r, _, _ := procGetWindow.Call(hwnd, rel)
	return r
}

// The cover follows the window: a move, a minimize and the restore, and hiding and showing it (the boss key).
func TestCoverFollowsMoveMinimizeAndHide(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var mainCover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); mainCover = o.CoverOf(f.Main) })

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	atCenter := func() bool {
		g := GeometryOf(f.Main)
		x, y := center(g.Client)
		return WindowFromPoint(x, y) == mainCover
	}
	if !atCenter() {
		t.Fatal("not covered at the start")
	}

	// moved and resized: the picture area has to be covered again where it is now
	f.Move(wintest.X+200, wintest.Y+100, 500, 300)
	waitFor("the cover follows a move and a resize", func() bool { return GeometryOf(mainCover).Frame == GeometryOf(f.Main).Client })
	if !atCenter() {
		t.Error("the middle of the moved picture area is not covered")
	}

	// minimized: the cover is hidden by the next look at the window; restored: it is back over the picture area
	f.Minimize()
	waitFor("the cover goes with a minimized window", func() bool { return !GeometryOf(mainCover).Visible })
	f.Restore()
	waitFor("the cover comes back with the window", func() bool { return GeometryOf(mainCover).Visible && atCenter() })

	// hidden (boss key, modavd window hide): the cover is hidden; shown again: it is back
	f.Hide()
	waitFor("the cover is hidden with the window", func() bool { return !GeometryOf(mainCover).Visible })
	f.Show()
	waitFor("the cover comes back with the shown window", func() bool { return GeometryOf(mainCover).Visible && atCenter() })
}

// A frame is painted: the cover's window shows what Paint drew (read back with PrintWindow, not from the desktop).
func TestCoverPaintsAFrame(t *testing.T) {
	f := startFakeEmulator(t)
	painted := make(chan struct{}, 1)
	h := startOverlay(t, OverlayOptions{Wake: func(o *Overlay) { o.Sync(specsFor(f)) },
		Paint: func(c *Canvas, main bool) {
			c.Fill(0x2A1B14)
			w, hh := c.Size()
			c.Text("hello", Rect{0, 0, int32(w), int32(hh)}, 24, 0xFFFFFF, true)
			select {
			case painted <- struct{}{}:
			default:
			}
		}})
	var mainCover uintptr
	h.on(func(o *Overlay) { o.Sync(specsFor(f)); o.Invalidate(); mainCover = o.CoverOf(f.Main) })
	select {
	case <-painted:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was painted")
	}
	time.Sleep(200 * time.Millisecond)
	img, ok := CapturePixels(mainCover, false)
	if !ok {
		t.Fatal("cannot read the cover back")
	}
	r, g, b, _ := img.At(5, 5).RGBA()
	if r>>8 != 0x2A || g>>8 != 0x1B || b>>8 != 0x14 {
		t.Errorf("background pixel %02x%02x%02x, want 2a1b14", r>>8, g>>8, b>>8)
	}
	white := 0
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			if p := img.RGBAAt(x, y); p.R > 200 && p.G > 200 && p.B > 200 {
				white++
			}
		}
	}
	if white < 20 {
		t.Errorf("no text in the picture (%d light pixels)", white)
	}
}

// A disabled window can still be minimized and restored from outside (Win+D and back, the boss key): ShowWindow works
// on it, and the cover follows. Qt's own windows may enable themselves again meanwhile, which the overlay answers by
// disabling them again (emu overlay_windows.go maintain).
func TestDisabledWindowMinimizesAndRestores(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var mainCover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); mainCover = o.CoverOf(f.Main) })
	if !SetWindowEnabled(f.Main, false) {
		t.Fatal("cannot disable")
	}
	f.Minimize()
	deadline := time.Now().Add(3 * time.Second)
	for !GeometryOf(f.Main).Minimized && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !GeometryOf(f.Main).Minimized {
		t.Fatal("did not minimize")
	}
	f.Restore()
	deadline = time.Now().Add(3 * time.Second)
	for GeometryOf(f.Main).Minimized && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if GeometryOf(f.Main).Minimized {
		t.Fatal("a disabled window did not restore")
	}
	deadline = time.Now().Add(3 * time.Second)
	for !GeometryOf(mainCover).Visible && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !GeometryOf(mainCover).Visible {
		t.Error("the cover did not come back")
	}
}

// Qt's hidden helper window of the emulator process (Qt..ScreenChangeObserverWindow: no title, never shown, as big as a
// screen) is no emulator window: it is not listed, and a whole overlay session (covers, disabling, enabling, minimize,
// restore) leaves it hidden, enabled and where it was. (11-28: a script that showed it put a white 1920 x 1023 window on
// the desktop.)
func TestHelperWindowIsNeverTouched(t *testing.T) {
	f := startFakeEmulator(t)
	state := func() (Geometry, bool) { return GeometryOf(f.Helper), IsWindowEnabled(f.Helper) }
	before, enabled := state()
	if !before.Exists || before.Visible || !enabled {
		t.Fatalf("the stand-in helper window is not hidden and enabled to begin with: %+v %v", before, enabled)
	}
	wins, err := EmulatorWindows(f.PID)
	if err != nil || len(wins) != 2 {
		t.Fatalf("emulator windows %v %v", wins, err)
	}
	for _, w := range wins {
		if w == f.Helper {
			t.Fatal("the helper window is listed as an emulator window")
		}
	}
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)) })
	for _, w := range wins {
		SetWindowEnabled(w, false)
	}
	f.Minimize()
	time.Sleep(200 * time.Millisecond)
	f.Restore()
	f.Hide()
	f.Show()
	time.Sleep(200 * time.Millisecond)
	for _, w := range wins {
		SetWindowEnabled(w, true)
	}
	h.on(func(o *Overlay) { o.Sync(nil) })
	after, enabledAfter := state()
	if after != before || enabledAfter != enabled {
		t.Errorf("the helper window changed: %+v enabled %v, was %+v enabled %v", after, enabledAfter, before, enabled)
	}
}

var procSendMessageW = moduser32.NewProc("SendMessageW")

// A cover stays right above its emulator window: when the window is raised over it (the user clicks the emulator window
// in the taskbar), the next look puts the cover back above it (SetWindowPos on the cover only).
func TestCoverStaysAboveItsWindowWhenRaised(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{TickEvery: 50 * time.Millisecond, Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Tick: func(o *Overlay) { o.Sync(specsFor(f)) }, Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var mainCover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); mainCover = o.CoverOf(f.Main) })
	procSetWindowPos.Call(f.Main, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate) // HWND_TOP: over the cover
	deadline := time.Now().Add(5 * time.Second)
	for windowAt(mainCover, gwHwndNext) != f.Main && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if next := windowAt(mainCover, gwHwndNext); next != f.Main {
		t.Fatalf("the cover is not back right above the emulator window (below it: %#x)", next)
	}
	x, y := center(GeometryOf(f.Main).Client)
	if WindowFromPoint(x, y) != mainCover {
		t.Error("the middle of the picture area is not covered after the window was raised")
	}
}

// A cover that only shows: the mouse goes through it to the emulator window below, the cover stays visible; and back.
func TestPassThroughLetsTheMouseGoThrough(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var mainCover, toolCover uintptr
	h.on(func(o *Overlay) {
		o.Watch(f.PID)
		o.Sync(specsFor(f))
		mainCover, toolCover = o.CoverOf(f.Main), o.CoverOf(f.Tool)
	})
	x, y := center(GeometryOf(f.Main).Client)
	tx, ty := center(GeometryOf(f.Tool).Frame)
	if WindowFromPoint(x, y) != mainCover {
		t.Fatal("not covered to begin with")
	}
	h.on(func(o *Overlay) { o.SetPassThrough(true) })
	if got := WindowFromPoint(x, y); got != f.Main {
		t.Errorf("with the cover passing the mouse through, the window at the middle is %#x, want the emulator window %#x", got, f.Main)
	}
	if got := WindowFromPoint(tx, ty); got != f.Tool {
		t.Errorf("on the toolbar: %#x, want the toolbar %#x", got, f.Tool)
	}
	if !GeometryOf(mainCover).Visible || !GeometryOf(toolCover).Visible {
		t.Error("the covers are not shown any more")
	}
	// a cover made after that passes the mouse through too
	h.on(func(o *Overlay) { o.Sync(nil); o.Sync(specsFor(f)); mainCover = o.CoverOf(f.Main) })
	if got := WindowFromPoint(x, y); got != f.Main {
		t.Errorf("a new cover took the mouse: %#x", got)
	}
	h.on(func(o *Overlay) { o.SetPassThrough(false) })
	if got := WindowFromPoint(x, y); got != mainCover {
		t.Errorf("after pass-through was turned off the window at the middle is %#x, want the cover %#x", got, mainCover)
	}
}

// The picture fades by the window's own opacity once the covers only show; set before that it waits, and a cover made later
// gets it too.
func TestAlphaFadesTheCoversThatOnlyShow(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	getAlpha := func(hwnd uintptr) (uint8, bool) {
		var key uint32
		var a uint8
		var flags uint32
		r, _, _ := moduser32.NewProc("GetLayeredWindowAttributes").Call(hwnd, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&a)), uintptr(unsafe.Pointer(&flags)))
		return a, r != 0 && flags&lwaAlpha != 0
	}
	var mainCover uintptr
	h.on(func(o *Overlay) {
		o.Watch(f.PID)
		o.Sync(specsFor(f))
		o.SetAlpha(90) // not passing through yet: nothing is layered, nothing changes
		mainCover = o.CoverOf(f.Main)
	})
	if _, ok := getAlpha(mainCover); ok {
		t.Error("a cover that takes the mouse is a layered window")
	}
	h.on(func(o *Overlay) { o.SetPassThrough(true); o.SetAlpha(100) })
	if a, ok := getAlpha(mainCover); !ok || a != 100 {
		t.Errorf("alpha %d (layered %v), want 100", a, ok)
	}
	h.on(func(o *Overlay) { o.SetAlpha(0) })
	if a, ok := getAlpha(mainCover); !ok || a != 0 {
		t.Errorf("alpha %d (layered %v), want 0", a, ok)
	}
	h.on(func(o *Overlay) { o.Sync(nil); o.Sync(specsFor(f)); mainCover = o.CoverOf(f.Main) })
	if a, ok := getAlpha(mainCover); !ok || a != 0 {
		t.Errorf("a cover made later: alpha %d (layered %v), want 0", a, ok)
	}
}

// A request to end is a flag the loop looks at, not only a message: with the posted message lost, the loop still ends on
// the next turn of its timer.
func TestQuitSurvivesALostMessage(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{TickEvery: 30 * time.Millisecond, Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	h.o.quit.Store(true) // as Quit does, without the PostMessage that would carry it
	select {
	case err := <-h.ended:
		if err != nil {
			t.Errorf("RunOverlay: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the loop did not end on the latched request")
	}
}

// A close of a cover and the end of the session take the same road as Quit: OnClose runs (before the answer: the process
// may be ended right after it), and the loop ends. WM_QUERYENDSESSION says yes.
func TestCloseAndEndSessionAskTheOverlayToEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		send func(cover uintptr)
		why  string
	}{
		{"close", func(cover uintptr) { moduser32.NewProc("PostMessageW").Call(cover, wmClose, 0, 0) }, "close"},
		{"end of the session", func(cover uintptr) { procSendMessageW.Call(cover, wmEndSession, 1, 0) }, "endsession"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := startFakeEmulator(t)
			closed := make(chan string, 4)
			h := startOverlay(t, OverlayOptions{Paint: func(cv *Canvas, main bool) { cv.Fill(0x2A1B14) },
				Wake:    func(o *Overlay) { o.Sync(specsFor(f)) },
				OnClose: func(why string) { closed <- why }})
			var cover uintptr
			h.on(func(o *Overlay) { o.Sync(specsFor(f)); cover = o.CoverOf(f.Main) })
			if r, _, _ := procSendMessageW.Call(cover, wmQueryEndSession, 0, 0); r != 1 {
				t.Errorf("WM_QUERYENDSESSION answered %d, want 1", r)
			}
			c.send(cover)
			select {
			case why := <-closed:
				if why != c.why {
					t.Errorf("OnClose(%q), want %q", why, c.why)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("OnClose was not called")
			}
			select {
			case <-h.ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the loop did not end")
			}
		})
	}
}

// ---- layers: the z-order follows the emulator window's layer ----------------------------------------------------------

var procDestroyWindowT = moduser32.NewProc("DestroyWindow")

// thirdPartyTopmost makes a small always-on-top window of this (test) process over rect r: somebody else's window that sits
// over the emulator window.
func thirdPartyTopmost(t *testing.T, r Rect) uintptr {
	t.Helper()
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _ := call(procCreateWindowExW, wsExTopmost|wsExToolWindow|wsExNoActivate, uintptr(unsafe.Pointer(cls)), 0,
		wsPopup|0x10000000, // WS_VISIBLE
		uintptr(int(r.Left)), uintptr(int(r.Top)), uintptr(r.Width()), uintptr(r.Height()), 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatal("cannot make the topmost window")
	}
	t.Cleanup(func() { procDestroyWindowT.Call(hwnd) })
	return hwnd
}

func above(a, b uintptr) bool {
	for w, n := a, 0; w != 0 && n < 100000; n++ {
		if w == b {
			return true
		}
		w = windowAt(w, gwHwndNext)
	}
	return false
}

// A third party's always-on-top window over the emulator window stays over the cover: the cover is not topmost, never
// lands above that window, and is back right above the emulator window when that is raised.
func TestCoverNeverGoesOverAThirdPartyTopmostWindow(t *testing.T) {
	f := startFakeEmulator(t)
	g := GeometryOf(f.Main)
	cx, cy := center(g.Client)
	top := thirdPartyTopmost(t, Rect{cx - 40, cy - 40, cx + 40, cy + 40})
	h := startOverlay(t, OverlayOptions{TickEvery: 50 * time.Millisecond, Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Tick: func(o *Overlay) { o.Sync(specsFor(f)) }, Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var cover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); cover = o.CoverOf(f.Main) })
	check := func(when string) {
		t.Helper()
		if exStyleOf(cover)&wsExTopmost != 0 {
			t.Errorf("%s: the cover is topmost", when)
		}
		if WindowFromPoint(cx, cy) != top {
			t.Errorf("%s: the third party's topmost window is not on top at its place (%#x)", when, WindowFromPoint(cx, cy))
		}
		if !above(top, cover) {
			t.Errorf("%s: the topmost window is not above the cover", when)
		}
		if WindowFromPoint(cx+60, cy+60) != cover {
			t.Errorf("%s: the rest of the picture area is not covered", when)
		}
	}
	check("at the start")
	procSetWindowPos.Call(f.Main, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate) // the emulator window is raised
	deadline := time.Now().Add(5 * time.Second)
	for windowAt(cover, gwHwndNext) != f.Main && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if windowAt(cover, gwHwndNext) != f.Main {
		t.Fatal("the cover is not back above the raised emulator window")
	}
	check("after the emulator window was raised")
}

// An always-on-top emulator window gets an always-on-top cover right above it; when it is no longer on top the cover is not
// either. (The stand-in's side toolbar is the window that can be put on top from here: the stand-in's main window does not
// take that from another process.)
func TestCoverFollowsTheTopmostStateOfTheEmulatorWindow(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{TickEvery: 50 * time.Millisecond, Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Tick: func(o *Overlay) { o.Sync(specsFor(f)) }, Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var cover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); cover = o.CoverOf(f.Tool) })
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !ok() {
			t.Fatalf("timed out: %s", what)
		}
	}
	procSetWindowPos.Call(f.Tool, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	wait("a topmost cover right above the topmost window", func() bool {
		return exStyleOf(cover)&wsExTopmost != 0 && windowAt(cover, gwHwndNext) == f.Tool
	})
	procSetWindowPos.Call(f.Tool, hwndNoTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	wait("a cover that is not topmost, right above the window", func() bool {
		return exStyleOf(cover)&wsExTopmost == 0 && windowAt(cover, gwHwndNext) == f.Tool
	})
}

// Two emulator windows over each other: each cover stays right above its own window, so the window that is above the other
// keeps its cover above it and the lower one's cover does not come over it.
func TestCoversOfTwoOverlappingEmulatorsStayWithTheirWindows(t *testing.T) {
	f1, f2 := startFakeEmulator(t), startFakeEmulator(t)
	specs := func() []CoverSpec { return append(specsFor(f1), specsFor(f2)...) }
	h := startOverlay(t, OverlayOptions{TickEvery: 50 * time.Millisecond, Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Tick: func(o *Overlay) { o.Sync(specs()) }, Wake: func(o *Overlay) { o.Sync(specs()) }})
	var c1, c2 uintptr
	h.on(func(o *Overlay) { o.Sync(specs()); c1, c2 = o.CoverOf(f1.Main), o.CoverOf(f2.Main) })
	procSetWindowPos.Call(f2.Main, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate) // f2 over f1
	deadline := time.Now().Add(5 * time.Second)
	ok := func() bool {
		return windowAt(c1, gwHwndNext) == f1.Main && windowAt(c2, gwHwndNext) == f2.Main && above(f2.Main, c1) && above(c2, f2.Main)
	}
	for !ok() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !ok() {
		t.Fatalf("below c1 %#x (f1 %#x), below c2 %#x (f2 %#x), f2 over c1 %v, c2 over f2 %v", windowAt(c1, gwHwndNext), f1.Main,
			windowAt(c2, gwHwndNext), f2.Main, above(f2.Main, c1), above(c2, f2.Main))
	}
	x, y := center(GeometryOf(f2.Main).Client)
	if got := WindowFromPoint(x, y); got != c2 {
		t.Errorf("the top window at the overlap is %#x, want the upper emulator's cover %#x", got, c2)
	}
}

// A cover that cannot be made a layer that only shows is hidden for good, not left taking the mouse.
func TestPassThroughThatDoesNotTakeHidesTheCover(t *testing.T) {
	f := startFakeEmulator(t)
	h := startOverlay(t, OverlayOptions{Paint: func(c *Canvas, main bool) { c.Fill(0x2A1B14) },
		Wake: func(o *Overlay) { o.Sync(specsFor(f)) }})
	var cover uintptr
	h.on(func(o *Overlay) { o.Watch(f.PID); o.Sync(specsFor(f)); cover = o.CoverOf(f.Main) })
	saved := setExStyle
	t.Cleanup(func() { setExStyle = saved })
	setExStyle = func(uintptr, uintptr) {} // the style does not take
	h.on(func(o *Overlay) { o.SetPassThrough(true) })
	if GeometryOf(cover).Visible {
		t.Error("a cover that could not become pass-through is still shown")
	}
	h.on(func(o *Overlay) { o.Sync(specsFor(f)) })
	time.Sleep(200 * time.Millisecond)
	if GeometryOf(cover).Visible {
		t.Error("the next look showed the cover again")
	}
	x, y := center(GeometryOf(f.Main).Client)
	if got := WindowFromPoint(x, y); got != f.Main {
		t.Errorf("the mouse does not reach the emulator window: %#x (main %#x tool %#x cover %#x) class %q", got, f.Main, f.Tool, cover, windowText(procGetClassNameW, got))
	}
}
