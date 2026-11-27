//go:build windows

package win

import (
	"testing"
	"time"

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
	// the cover is owned by the emulator window and stays above it
	if owner := windowOwner(mainCover); owner != f.Main {
		t.Errorf("owner of the cover %#x, want %#x", owner, f.Main)
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

func windowOwner(hwnd uintptr) uintptr {
	r, _, _ := procGetWindow.Call(hwnd, gwOwner)
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

	// minimized: the cover goes with it (an owned window); restored: it is back over the picture area
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
