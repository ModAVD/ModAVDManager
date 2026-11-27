//go:build windows

package win

import (
	"errors"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Cover windows: the boot screen laid over the emulator's windows (ModAVD/ModAVD#259, emu overlay.go).
//
// A cover is a pop-up window owned by the emulator window it covers, so Windows keeps it above that window, hides it
// when the emulator window is minimized or hidden and brings it back with it. It never takes the focus
// (WS_EX_NOACTIVATE, SW_SHOWNOACTIVATE, MA_NOACTIVATE) and swallows the mouse. It has no taskbar button
// (WS_EX_TOOLWINDOW). All of it, the timer, the window-event hooks and the painting, runs on the one OS thread of
// RunOverlay; other goroutines reach it through Post and Quit.

// Rect is a rectangle in screen pixels (right and bottom are not part of it, as RECT).
type Rect struct{ Left, Top, Right, Bottom int32 }

// Width and Height of the rectangle.
func (r Rect) Width() int  { return int(r.Right - r.Left) }
func (r Rect) Height() int { return int(r.Bottom - r.Top) }

// Geometry of a window as the cover needs it, in physical pixels (the process is per-monitor DPI aware).
type Geometry struct {
	Exists    bool
	Visible   bool // IsWindowVisible
	Minimized bool // IsIconic
	Frame     Rect // GetWindowRect
	Client    Rect // the client area on the screen
}

var (
	modgdi32 = syscall.NewLazyDLL("gdi32.dll")

	procEnableWindow        = moduser32.NewProc("EnableWindow")
	procIsWindowEnabled     = moduser32.NewProc("IsWindowEnabled")
	procIsWindow            = moduser32.NewProc("IsWindow")
	procWindowFromPoint     = moduser32.NewProc("WindowFromPoint")
	procGetWindowRect       = moduser32.NewProc("GetWindowRect")
	procGetClientRect       = moduser32.NewProc("GetClientRect")
	procClientToScreen      = moduser32.NewProc("ClientToScreen")
	procSetWindowPos        = moduser32.NewProc("SetWindowPos")
	procShowWindow          = moduser32.NewProc("ShowWindow")
	procInvalidateRect      = moduser32.NewProc("InvalidateRect")
	procBeginPaint          = moduser32.NewProc("BeginPaint")
	procEndPaint            = moduser32.NewProc("EndPaint")
	procGetDC               = moduser32.NewProc("GetDC")
	procReleaseDC           = moduser32.NewProc("ReleaseDC")
	procDrawTextW           = moduser32.NewProc("DrawTextW")
	procLoadCursorW         = moduser32.NewProc("LoadCursorW")
	procSetCursor           = moduser32.NewProc("SetCursor")
	procSetWinEventHook     = moduser32.NewProc("SetWinEventHook")
	procUnhookWinEvent      = moduser32.NewProc("UnhookWinEvent")
	procSetProcessDPICtx    = moduser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware  = moduser32.NewProc("SetProcessDPIAware")
	procPrintWindow         = moduser32.NewProc("PrintWindow")
	procCreateCompatibleDC  = modgdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp = modgdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject        = modgdi32.NewProc("SelectObject")
	procDeleteObject        = modgdi32.NewProc("DeleteObject")
	procDeleteDC            = modgdi32.NewProc("DeleteDC")
	procBitBlt              = modgdi32.NewProc("BitBlt")
	procStretchDIBits       = modgdi32.NewProc("StretchDIBits")
	procCreateFontW         = modgdi32.NewProc("CreateFontW")
	procSetBkMode           = modgdi32.NewProc("SetBkMode")
	procSetTextColor        = modgdi32.NewProc("SetTextColor")
	procCreateDIBSection    = modgdi32.NewProc("CreateDIBSection")
)

const (
	wsPopup           = 0x80000000
	wsExToolWindow    = 0x00000080
	wsExNoActivate    = 0x08000000
	wsExTopmost       = 0x00000008
	swShowNoActivate  = 4
	swpNoZOrder       = 0x0004
	swpNoActivate     = 0x0010
	swpShowWindow     = 0x0040
	swpNoOwnerZOrder  = 0x0200
	wmPaint           = 0x000F
	wmEraseBkgnd      = 0x0014
	wmSetCursor       = 0x0020
	wmMouseActivate   = 0x0021
	wmNcHitTest       = 0x0084
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmPrint           = 0x0317
	wmPrintClient     = 0x0318
	wmEndSession      = 0x0016
	htClient          = 1
	maNoActivate      = 3
	idcArrow          = 32512
	srcCopy           = 0x00CC0020
	biRGB             = 0
	dibRGBColors      = 0
	transparentBk     = 1
	dtCenter          = 0x1
	dtVCenter         = 0x4
	dtSingleLine      = 0x20
	dtNoPrefix        = 0x800
	dtEndEllipsis     = 0x8000
	dtWordBreak       = 0x10
	fwBold            = 700
	fwNormal          = 400
	antialiasedQual   = 4
	defaultCharset    = 1
	pwRenderFull      = 2

	// WinEvents: object show / hide / destroy / location change, and system minimize start / end
	eventObjectDestroy     = 0x8001
	eventObjectShow        = 0x8002
	eventObjectHide        = 0x8003
	eventObjectLocation    = 0x800B
	eventSystemMinStart    = 0x0016
	eventSystemMinEnd      = 0x0017
	eventSystemMoveSizeEnd = 0x000B
	winEventOutOfContext   = 0x0000
	objidWindow            = 0

	coverClassName = "ModAVDBootCover"
	ctlClassName   = "ModAVDBootCoverCtl"
	timerID        = 1
	wmCoverWake    = wmApp + 10
	wmCoverCall    = wmApp + 11
	hwndMessage    = ^uintptr(2) // HWND_MESSAGE (-3)
)

type paintStruct struct {
	HDC      uintptr
	FErase   int32
	RcPaint  Rect
	FRestore int32
	FIncUpd  int32
	Reserved [32]byte
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPels, YPels  int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// SetDPIAware makes this process per-monitor DPI aware (v2), so window rectangles and our own windows are in
// physical pixels on every monitor and scale setting. Call it before the first window is made.
func SetDPIAware() {
	if procSetProcessDPICtx.Find() == nil {
		neg4 := ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
		if r, _, _ := procSetProcessDPICtx.Call(neg4); r != 0 {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
}

// EmulatorWindows are the main window and the side toolbar of emulator process pid (the windows a cover goes over).
func EmulatorWindows(pid uint32) ([]uintptr, error) { return emulatorWindows(pid) }

func rectOf(p *syscall.LazyProc, hwnd uintptr) (Rect, bool) {
	var r Rect
	ok, _, _ := p.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r, ok != 0
}

// GeometryOf reads hwnd's place and state.
func GeometryOf(hwnd uintptr) Geometry {
	if hwnd == 0 {
		return Geometry{}
	}
	if ok, _, _ := procIsWindow.Call(hwnd); ok == 0 {
		return Geometry{}
	}
	g := Geometry{Exists: true}
	if v, _, _ := procIsWindowVisible.Call(hwnd); v != 0 {
		g.Visible = true
	}
	if v, _, _ := procIsIconic.Call(hwnd); v != 0 {
		g.Minimized = true
	}
	g.Frame, _ = rectOf(procGetWindowRect, hwnd)
	if cr, ok := rectOf(procGetClientRect, hwnd); ok {
		pt := [2]int32{cr.Left, cr.Top}
		procClientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&pt[0])))
		g.Client = Rect{pt[0], pt[1], pt[0] + cr.Right - cr.Left, pt[1] + cr.Bottom - cr.Top}
	}
	return g
}

// WindowProcessID is the process that owns hwnd; 0 when the window is gone.
func WindowProcessID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// SetWindowEnabled is EnableWindow: a disabled window takes no mouse or keyboard input. It reports whether the window
// is in the state asked for afterwards.
func SetWindowEnabled(hwnd uintptr, enabled bool) bool {
	v := uintptr(0)
	if enabled {
		v = 1
	}
	procEnableWindow.Call(hwnd, v)
	return IsWindowEnabled(hwnd) == enabled
}

// IsWindowEnabled: the window takes input right now. A window that is gone reads as not enabled.
func IsWindowEnabled(hwnd uintptr) bool {
	r, _, _ := procIsWindowEnabled.Call(hwnd)
	return r != 0
}

// WindowFromPoint is the top-most window at screen point (x, y), 0 when there is none.
func WindowFromPoint(x, y int32) uintptr {
	// POINT is passed by value as one 64-bit argument
	r, _, _ := procWindowFromPoint.Call(uintptr(uint64(uint32(x)) | uint64(uint32(y))<<32))
	return r
}

// CapturePixels asks hwnd to paint itself (PrintWindow) and returns the pixels, top-down RGBA; for evidence
// pictures of one window, not of the desktop. ok=false when the window cannot be captured.
func CapturePixels(hwnd uintptr, full bool) (*image.RGBA, bool) {
	g := GeometryOf(hwnd)
	w, h := g.Frame.Width(), g.Frame.Height()
	if !g.Exists || w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, false
	}
	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		return nil, false
	}
	defer procReleaseDC.Call(0, screen)
	dc, _, _ := procCreateCompatibleDC.Call(screen)
	if dc == 0 {
		return nil, false
	}
	defer procDeleteDC.Call(dc)
	bmi := bitmapInfoHeader{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32, Compression: biRGB}
	bmi.Size = uint32(unsafe.Sizeof(bmi))
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bmi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return nil, false
	}
	defer procDeleteObject.Call(bmp)
	old, _, _ := procSelectObject.Call(dc, bmp)
	defer procSelectObject.Call(dc, old)
	flags := uintptr(0)
	if full {
		flags = pwRenderFull // what a GPU-drawn window needs; also draws what a window of ours did not ask for
	}
	if r, _, _ := procPrintWindow.Call(hwnd, dc, flags); r == 0 {
		return nil, false
	}
	src := unsafe.Slice((*byte)(bits), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for k := 0; k < w*h; k++ {
		img.Pix[k*4], img.Pix[k*4+1], img.Pix[k*4+2], img.Pix[k*4+3] = src[k*4+2], src[k*4+1], src[k*4], 255
	}
	return img, true
}

// ---- the overlay ----------------------------------------------------------------------------------------------

// CoverSpec says where the cover of one emulator window goes.
type CoverSpec struct {
	Owner uintptr // the emulator window; a cover is made for each Owner once
	Rect  Rect    // where, in screen pixels
	Show  bool    // visible now (the owner is shown and not minimized)
	Main  bool    // the main window's cover (gets the boot screen), else a plain one
}

// OverlayOptions of RunOverlay. Every function runs on the overlay's thread.
type OverlayOptions struct {
	// Tick is called about every TickEvery (default 33 ms): look at what changed, Sync, Invalidate.
	Tick      func(o *Overlay)
	TickEvery time.Duration
	// Wake is called right after a window event of WatchPID (a window shown, hidden, moved, minimized), and once at the
	// start; nil: Tick does it all.
	Wake func(o *Overlay)
	// Paint draws one frame of a cover into c.
	Paint func(c *Canvas, main bool)
	// Ended is called once when the loop ends, before the covers are destroyed (the session or the machine ends, Quit).
	Ended func()
}

// Overlay is the running set of covers. Methods marked "any goroutine" may be called from anywhere, the others only
// from the overlay's own functions.
type Overlay struct {
	opt    OverlayOptions
	ctl    uintptr
	covers map[uintptr]*cover
	calls  chan func()
	done   chan struct{}
	hooks  []uintptr
	quit   atomic.Bool
}

type cover struct {
	hwnd   uintptr
	owner  uintptr
	rect   Rect
	shown  bool
	main   bool
	canvas Canvas
}

var (
	overlayCur     atomic.Pointer[Overlay]
	overlayStartMu sync.Mutex
	coverProcCb    = sync.OnceValue(func() uintptr { return syscall.NewCallback(coverProc) })
	ctlProcCb      = sync.OnceValue(func() uintptr { return syscall.NewCallback(ctlProc) })
	winEventCb     = sync.OnceValue(func() uintptr { return syscall.NewCallback(winEventProc) })
	errOverlayRun  = errors.New("遮罩已经在这个进程里开着")
)

// RunOverlay runs the overlay on the calling goroutine's thread (locked to it) until Quit; one per process.
func RunOverlay(opt OverlayOptions) error {
	overlayStartMu.Lock()
	if overlayCur.Load() != nil {
		overlayStartMu.Unlock()
		return errOverlayRun
	}
	o := &Overlay{opt: opt, covers: map[uintptr]*cover{}, calls: make(chan func(), 64), done: make(chan struct{})}
	overlayCur.Store(o)
	overlayStartMu.Unlock()
	defer overlayCur.Store(nil)
	defer close(o.done)

	runtime.LockOSThread()
	inst, _, _ := procGetModuleHandleW.Call(0)
	for _, c := range []struct {
		name string
		proc uintptr
	}{{coverClassName, coverProcCb()}, {ctlClassName, ctlProcCb()}} {
		cls, _ := syscall.UTF16PtrFromString(c.name)
		wc := wndClassEx{WndProc: c.proc, Instance: inst, ClassName: cls}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, err := call(procRegisterClassExW, uintptr(unsafe.Pointer(&wc))); r == 0 && !errors.Is(err, errorClassAlreadyExists) {
			return errors.New("注册遮罩窗口类失败: " + err.Error())
		}
	}
	name, _ := syscall.UTF16PtrFromString(ctlClassName)
	ctl, err := call(procCreateWindowExW, 0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, inst, 0)
	if ctl == 0 {
		return errors.New("建遮罩控制窗口失败: " + err.Error())
	}
	o.ctl = ctl
	every := opt.TickEvery
	if every <= 0 {
		every = 33 * time.Millisecond
	}
	procSetTimer.Call(ctl, timerID, uintptr(every/time.Millisecond), 0)
	if opt.Wake != nil {
		opt.Wake(o)
	}

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	if opt.Ended != nil {
		opt.Ended()
	}
	o.Watch(0)
	procKillTimer.Call(ctl, timerID)
	for _, c := range o.covers {
		procDestroyWindow.Call(c.hwnd)
		c.canvas.release()
	}
	o.covers = nil
	procDestroyWindow.Call(ctl)
	return nil
}

// Watch makes the window events of process pid (shown, hidden, moved, minimized) wake the overlay at once; 0 stops
// watching. Only from the overlay's own functions.
func (o *Overlay) Watch(pid uint32) {
	for _, h := range o.hooks {
		procUnhookWinEvent.Call(h)
	}
	o.hooks = nil
	if pid == 0 {
		return
	}
	for _, r := range [][2]uintptr{{eventSystemMinStart, eventSystemMinEnd}, {eventObjectDestroy, eventObjectLocation},
		{eventSystemMoveSizeEnd, eventSystemMoveSizeEnd}} {
		if h, _, _ := procSetWinEventHook.Call(r[0], r[1], 0, winEventCb(), uintptr(pid), 0, winEventOutOfContext); h != 0 {
			o.hooks = append(o.hooks, h)
		}
	}
}

// Quit ends RunOverlay (any goroutine).
func (o *Overlay) Quit() {
	if o.quit.Swap(true) {
		return
	}
	procPostMessageW.Call(o.ctl, wmClose, 0, 0)
}

// Post runs f on the overlay's thread (any goroutine); false when the overlay has ended.
func (o *Overlay) Post(f func()) bool {
	select {
	case <-o.done:
		return false
	case o.calls <- f:
	}
	procPostMessageW.Call(o.ctl, wmCoverCall, 0, 0)
	return true
}

// CoverOf is the window of the cover over owner, 0 when there is none.
func (o *Overlay) CoverOf(owner uintptr) uintptr {
	if c := o.covers[owner]; c != nil {
		return c.hwnd
	}
	return 0
}

// Sync makes the covers what specs say: one per owner, in place, shown or hidden; covers of owners that are no longer
// listed are destroyed.
func (o *Overlay) Sync(specs []CoverSpec) {
	seen := map[uintptr]bool{}
	for _, s := range specs {
		seen[s.Owner] = true
		c := o.covers[s.Owner]
		if c == nil {
			c = o.newCover(s)
			if c == nil {
				continue
			}
			o.covers[s.Owner] = c
		}
		c.main = s.Main
		w, h := s.Rect.Width(), s.Rect.Height()
		if w <= 0 || h <= 0 {
			s.Show = false
		}
		if s.Rect != c.rect && w > 0 && h > 0 {
			flags := uintptr(swpNoActivate | swpNoZOrder | swpNoOwnerZOrder)
			procSetWindowPos.Call(c.hwnd, 0, uintptr(int(s.Rect.Left)), uintptr(int(s.Rect.Top)), uintptr(w), uintptr(h), flags)
			c.rect = s.Rect
			procInvalidateRect.Call(c.hwnd, 0, 0)
		}
		if s.Show != c.shown {
			cmd := uintptr(swHide)
			if s.Show {
				cmd = swShowNoActivate
			}
			procShowWindow.Call(c.hwnd, cmd)
			c.shown = s.Show
		}
	}
	for owner, c := range o.covers {
		if !seen[owner] {
			procDestroyWindow.Call(c.hwnd)
			c.canvas.release()
			delete(o.covers, owner)
		}
	}
}

// Invalidate asks for a repaint of every shown cover.
func (o *Overlay) Invalidate() {
	for _, c := range o.covers {
		if c.shown {
			procInvalidateRect.Call(c.hwnd, 0, 0)
		}
	}
}

func (o *Overlay) newCover(s CoverSpec) *cover {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := syscall.UTF16PtrFromString(coverClassName)
	w, h := max(s.Rect.Width(), 1), max(s.Rect.Height(), 1)
	// created hidden (no WS_VISIBLE); ShowWindow(SW_SHOWNOACTIVATE) shows it without taking the focus
	hwnd, _ := call(procCreateWindowExW, wsExToolWindow|wsExNoActivate, uintptr(unsafe.Pointer(cls)), 0, wsPopup,
		uintptr(int(s.Rect.Left)), uintptr(int(s.Rect.Top)), uintptr(w), uintptr(h), s.Owner, 0, inst, 0)
	if hwnd == 0 {
		return nil
	}
	return &cover{hwnd: hwnd, owner: s.Owner, main: s.Main}
}

func (o *Overlay) cover(hwnd uintptr) *cover {
	for _, c := range o.covers {
		if c.hwnd == hwnd {
			return c
		}
	}
	return nil
}

func ctlProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	o := overlayCur.Load()
	if o == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	}
	switch uint32(msg) {
	case wmTimer:
		if o.opt.Tick != nil && !o.quit.Load() {
			o.opt.Tick(o)
		}
		return 0
	case wmCoverWake:
		if o.opt.Wake != nil && !o.quit.Load() {
			o.opt.Wake(o)
		}
		return 0
	case wmCoverCall:
		for {
			select {
			case f := <-o.calls:
				f()
			default:
				return 0
			}
		}
	case wmClose:
		procPostQuitMessage.Call(0)
		return 0
	case wmEndSession:
		if wparam != 0 {
			procPostQuitMessage.Call(0)
		}
		return 0
	case wmQueryEndSession:
		return 1
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func winEventProc(_, event, hwnd, idObject, _, _, _ uintptr) uintptr {
	o := overlayCur.Load()
	if o == nil || o.ctl == 0 || (idObject != objidWindow && uint32(event) != eventSystemMinStart && uint32(event) != eventSystemMinEnd) {
		return 0
	}
	procPostMessageW.Call(o.ctl, wmCoverWake, 0, 0)
	return 0
}

func coverProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	o := overlayCur.Load()
	switch uint32(msg) {
	case wmNcHitTest:
		return htClient
	case wmMouseActivate:
		return maNoActivate
	case wmEraseBkgnd:
		return 1
	case wmSetCursor:
		cur, _, _ := procLoadCursorW.Call(0, idcArrow)
		procSetCursor.Call(cur)
		return 1
	case wmPaint:
		var ps paintStruct
		dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if o != nil && dc != 0 {
			if c := o.cover(hwnd); c != nil && o.opt.Paint != nil {
				o.paintCover(dc, c)
			}
		}
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmPrint, wmPrintClient:
		// PrintWindow (the evidence pictures) asks the window for its picture
		if o != nil && wparam != 0 {
			if c := o.cover(hwnd); c != nil && o.opt.Paint != nil {
				o.paintCover(wparam, c)
			}
		}
		return 0
	case wmClose:
		return 0 // nothing closes a cover but the overlay
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// ---- painting -------------------------------------------------------------------------------------------------

// Canvas is what Paint draws on: one frame, double-buffered, shown when Paint returns.
type Canvas struct {
	w, h   int
	dc     uintptr // memory DC with the frame bitmap
	bmp    uintptr
	old    uintptr
	fonts  map[int]uintptr
	bgra   []byte
	target uintptr // the window's DC while painting
}

func (cv *Canvas) release() {
	if cv.dc != 0 {
		procSelectObject.Call(cv.dc, cv.old)
		procDeleteObject.Call(cv.bmp)
		procDeleteDC.Call(cv.dc)
		cv.dc, cv.bmp = 0, 0
	}
	for _, f := range cv.fonts {
		procDeleteObject.Call(f)
	}
	cv.fonts = nil
}

func (cv *Canvas) ensure(dc uintptr, w, h int) bool {
	if cv.dc != 0 && cv.w == w && cv.h == h {
		return true
	}
	if cv.dc != 0 {
		procSelectObject.Call(cv.dc, cv.old)
		procDeleteObject.Call(cv.bmp)
		procDeleteDC.Call(cv.dc)
		cv.dc = 0
	}
	mem, _, _ := procCreateCompatibleDC.Call(dc)
	if mem == 0 {
		return false
	}
	bmp, _, _ := procCreateCompatibleBmp.Call(dc, uintptr(w), uintptr(h))
	if bmp == 0 {
		procDeleteDC.Call(mem)
		return false
	}
	cv.old, _, _ = procSelectObject.Call(mem, bmp)
	cv.dc, cv.bmp, cv.w, cv.h = mem, bmp, w, h
	return true
}

// Size of the frame in pixels.
func (cv *Canvas) Size() (int, int) { return cv.w, cv.h }

// Image draws img (its size is the frame's) over the whole frame.
func (cv *Canvas) Image(img *image.RGBA) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w <= 0 || h <= 0 || cv.dc == 0 {
		return
	}
	if len(cv.bgra) != w*h*4 {
		cv.bgra = make([]byte, w*h*4)
	}
	src := img.Pix
	if img.Stride != w*4 {
		return
	}
	dst := cv.bgra
	for k := 0; k+3 < len(src); k += 4 {
		dst[k], dst[k+1], dst[k+2], dst[k+3] = src[k+2], src[k+1], src[k], 255
	}
	bmi := bitmapInfoHeader{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32, Compression: biRGB}
	bmi.Size = uint32(unsafe.Sizeof(bmi))
	procStretchDIBits.Call(cv.dc, 0, 0, uintptr(cv.w), uintptr(cv.h), 0, 0, uintptr(w), uintptr(h),
		uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(&bmi)), dibRGBColors, srcCopy)
}

// Fill paints the whole frame one colour (0xRRGGBB).
func (cv *Canvas) Fill(rgb uint32) {
	bmi := bitmapInfoHeader{Width: 1, Height: -1, Planes: 1, BitCount: 32, Compression: biRGB}
	bmi.Size = uint32(unsafe.Sizeof(bmi))
	px := [4]byte{byte(rgb), byte(rgb >> 8), byte(rgb >> 16), 255}
	procStretchDIBits.Call(cv.dc, 0, 0, uintptr(cv.w), uintptr(cv.h), 0, 0, 1, 1, uintptr(unsafe.Pointer(&px[0])),
		uintptr(unsafe.Pointer(&bmi)), dibRGBColors, srcCopy)
}

func (cv *Canvas) font(px int, bold bool) uintptr {
	key := px
	if bold {
		key = -px
	}
	if f, ok := cv.fonts[key]; ok {
		return f
	}
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	weight := uintptr(fwNormal)
	if bold {
		weight = fwBold
	}
	f, _, _ := procCreateFontW.Call(uintptr(-px&0xFFFFFFFF), 0, 0, 0, weight, 0, 0, 0, defaultCharset, 0, 0, antialiasedQual, 0,
		uintptr(unsafe.Pointer(face)))
	if cv.fonts == nil {
		cv.fonts = map[int]uintptr{}
	}
	cv.fonts[key] = f
	return f
}

// Text draws one centred line (wrapped when it does not fit) of s, px pixels high, colour 0xRRGGBB, in the rectangle.
func (cv *Canvas) Text(s string, r Rect, px int, rgb uint32, bold bool) {
	if s == "" || cv.dc == 0 || px < 6 {
		return
	}
	u, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	f := cv.font(px, bold)
	old, _, _ := procSelectObject.Call(cv.dc, f)
	procSetBkMode.Call(cv.dc, transparentBk)
	procSetTextColor.Call(cv.dc, uintptr(rgb&0xFF)<<16|uintptr(rgb>>8&0xFF)<<8|uintptr(rgb>>16&0xFF))
	procDrawTextW.Call(cv.dc, uintptr(unsafe.Pointer(u)), ^uintptr(0), uintptr(unsafe.Pointer(&r)),
		dtCenter|dtVCenter|dtNoPrefix|dtEndEllipsis|dtSingleLine)
	procSelectObject.Call(cv.dc, old)
}

func (o *Overlay) paintCover(dc uintptr, c *cover) {
	w, h := c.rect.Width(), c.rect.Height()
	if w <= 0 || h <= 0 {
		return
	}
	cv := &c.canvas
	if !cv.ensure(dc, w, h) {
		return
	}
	o.opt.Paint(cv, c.main)
	procBitBlt.Call(dc, 0, 0, uintptr(w), uintptr(h), cv.dc, 0, 0, srcCopy)
}

// IsEmulatorToolbar: hwnd is the emulator's side toolbar (the tool window titled "Emulator"), not its main window.
func IsEmulatorToolbar(hwnd uintptr) bool {
	return windowText(procGetWindowTextW, hwnd) == emulatorToolbarTitle
}
