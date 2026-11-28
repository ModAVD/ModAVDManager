// Package bootscreen draws the boot screen: what is laid over the emulator's window from the moment it shows until the
// instance is ready (ModAVD/ModAVD#259), and what the manager's view of an instance shows while it boots. No window
// code here: a Screen paints a frame into an image and says which lines of text go over it, and whoever owns a window
// (emu's overlay, the page) shows that.
//
// The picture is the placeholder: the brand avatar (avatar.png, the brand's transparent avatar at 512 px) on the brand
// brown with the layout the final artwork (v3, layers.json "progressUi") has: under the character a thin rounded
// progress bar about 60 % as wide as the character (cream 20 % track, coral #F07060 fill, a small warm light at the head
// of the fill), the percentage to the right of the bar and the step that is being done under it, the lines drawn by
// the code, not by the picture. While there is no real percentage the bar is indeterminate (a light running to and fro)
// and the number is "…". It is built from layers drawn back to front (Layer), so the final artwork (the character in
// parts that move a little, the screen she holds filling in with the progress, the eruption at the end) replaces the
// avatar layer and adds layers that read State.Progress, without touching the windows or the progress logic.
package bootscreen

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"
	"time"
)

//go:embed avatar.png
var avatarPNG []byte

// Colours (0xRRGGBB): the progress bar and the text, from layers.json of the artwork.
const (
	Background = 0x251915 // the dark end of the background's radial gradient
	BarFill    = 0xF07060
	TextPct    = 0xFFEDDB
	TextStep   = 0xEED7C5
	TextNote   = 0xFFC98A
)

// State is everything one frame depends on.
type State struct {
	// Progress is the bar, 0 to 1 (already smoothed by the caller, see Ease); Indeterminate: there is no real
	// percentage to show (the bar runs to and fro and the number is "…").
	Progress      float64
	Indeterminate bool
	// Elapsed is the animation clock: any increasing time, the movement of the bar goes by it.
	Elapsed time.Duration
	// Step is the line under the bar (what is being done), Note replaces it when there is something the user should
	// know (the setup did not finish in time). Both already in the language of the manager.
	Step, Note string
}

// Label is a line of text to draw over the frame in the rectangle X, Y, W, H, Px pixels high: centred, or at the left of
// the rectangle (Left).
type Label struct {
	Text       string
	X, Y, W, H int
	Px         int
	RGB        uint32
	Bold       bool
	Left       bool
}

// Screen paints frames.
type Screen interface {
	// Draw paints the frame for st into dst (any size, its bounds are the frame) and returns the lines of text that go
	// over it.
	Draw(dst *image.RGBA, st State) []Label
}

// Layer is one part of a Screen: it paints into dst, which already holds the layers behind it. A layer that does not
// depend on State is Static: the Screen then draws it once per frame size and copies the result.
type Layer interface {
	Static() bool
	Draw(dst *image.RGBA, st State)
}

// Stack is a Screen of layers back to front, with the lines of text the caller's Lines function makes.
type Stack struct {
	Layers []Layer
	Lines  func(w, h int, st State) []Label

	mu     sync.Mutex
	base   *image.RGBA // the static layers at size w x h
	w, h   int
	static int // how many leading layers are static (cached in base)
}

// Draw implements Screen.
func (s *Stack) Draw(dst *image.RGBA, st State) []Label {
	b := dst.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	s.mu.Lock()
	if s.base == nil || s.w != w || s.h != h {
		s.base = image.NewRGBA(image.Rect(0, 0, w, h))
		s.w, s.h, s.static = w, h, 0
		for _, l := range s.Layers {
			if !l.Static() {
				break
			}
			l.Draw(s.base, st)
			s.static++
		}
	}
	base, static := s.base, s.static
	s.mu.Unlock()
	if b.Min == (image.Point{}) && dst.Stride == base.Stride {
		copy(dst.Pix, base.Pix)
	} else {
		draw.Draw(dst, b, base, image.Point{}, draw.Src)
	}
	for _, l := range s.Layers[static:] {
		l.Draw(dst, st)
	}
	if s.Lines == nil {
		return nil
	}
	return s.Lines(w, h, st)
}

func rgba(rgb uint32) color.RGBA { return color.RGBA{byte(rgb >> 16), byte(rgb >> 8), byte(rgb), 255} }

// The reference composition of the artwork is 1280 x 720 around the middle of the frame; everything scales with
// s = min(height / 720, width / 810) and is placed by its offset from the middle (layers.json "responsive"), so a wide,
// a square and a tall window keep the same picture without cropping or letterboxing.
const (
	refW, refH   = 1280, 720
	refAvatarX   = 277    // the avatar's top-left in the reference composition (its rig space is 1024 px at 0.71)
	refAvatarY   = -31    // (the picture's own margin is transparent)
	refAvatarPx  = 727.04 // 1024 * 0.71
	refBarX      = 443
	refBarY      = 646
	refBarW      = 352
	refBarH      = 7
	refPctGap    = 16
	refPctPx     = 18
	refStepDown  = 25 // from the bar's bottom to the middle of the step line
	refStepPx    = 16
	refCapCenter = 640
)

// Layout is where everything is in a frame of w x h pixels.
type Layout struct {
	S                  float64
	AvatarX, AvatarY   int
	AvatarSize         int
	BarX, BarY         int
	BarW, BarH         int
	PctX, PctW, PctPx  int // the percentage, at the right of the bar, left aligned
	StepY, StepPx      int // the middle of the step line, and its size
	CenterX            int
	HeadR, HaloR       float64
	BarMid             int
	NoteW, StepW, PctH int
}

// LayoutFor computes the layout of a frame of w x h pixels.
func LayoutFor(w, h int) Layout {
	s := math.Min(float64(h)/refH, float64(w)/810)
	cx, cy := float64(w)/2, float64(h)/2
	at := func(ref float64, c float64) float64 { return c + ref*s }
	l := Layout{S: s, CenterX: int(cx + 0.5)}
	l.AvatarX = int(math.Round(at(refAvatarX-refW/2, cx)))
	l.AvatarY = int(math.Round(at(refAvatarY-refH/2, cy)))
	l.AvatarSize = int(refAvatarPx*s + 0.5)
	l.BarX = int(math.Round(at(refBarX-refW/2, cx)))
	l.BarY = int(math.Round(at(refBarY-refH/2, cy)))
	l.BarW = max(int(refBarW*s+0.5), 40)
	l.BarH = max(int(refBarH*s+0.5), 4)
	l.BarMid = l.BarY + l.BarH/2
	l.PctX = l.BarX + l.BarW + max(int(refPctGap*s+0.5), 6)
	l.PctPx = max(int(refPctPx*s+0.5), 11)
	l.PctW = l.PctPx * 4
	l.PctH = l.PctPx * 3 / 2
	l.StepY = l.BarY + l.BarH + max(int(refStepDown*s+0.5), 8)
	l.StepPx = max(int(refStepPx*s+0.5), 11)
	l.StepW = min(w-8, int(float64(l.BarW)*1.6))
	l.NoteW = min(w-8, int(float64(l.BarW)*2.0))
	l.HeadR, l.HaloR = math.Max(3*s, 1.5), math.Max(5*s, 2.5)
	return l
}

// RadialLayer paints the background: the brand brown with a lighter glow behind the character (layers.json
// "canvas.background": an elliptical radial gradient).
type RadialLayer struct{}

func (RadialLayer) Static() bool { return true }
func (RadialLayer) Draw(dst *image.RGBA, _ State) {
	b := dst.Bounds()
	w, h := b.Dx(), b.Dy()
	s := math.Min(float64(h)/refH, float64(w)/810)
	cx, cy := float64(w)/2, float64(h)/2+(296-refH/2)*s
	rx, ry := 710*s, 514*s
	stops := []struct {
		at      float64
		r, g, b float64
	}{{0, 82, 53, 42}, {0.48, 60, 39, 31}, {1, 37, 25, 21}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := (float64(x)+0.5-cx)/rx, (float64(y)+0.5-cy)/ry
			t := math.Min(math.Sqrt(dx*dx+dy*dy), 1)
			a, c := stops[0], stops[1]
			if t > stops[1].at {
				a, c = stops[1], stops[2]
			}
			k := (t - a.at) / (c.at - a.at)
			dst.SetRGBA(b.Min.X+x, b.Min.Y+y, color.RGBA{uint8(a.r + (c.r-a.r)*k + 0.5), uint8(a.g + (c.g-a.g)*k + 0.5),
				uint8(a.b + (c.b-a.b)*k + 0.5), 255})
		}
	}
}

// AvatarLayer paints the avatar where the artwork's rig has it. (The final artwork replaces this layer by the parts of
// the character.)
type AvatarLayer struct{ Img image.Image }

func (AvatarLayer) Static() bool { return true }
func (l AvatarLayer) Draw(dst *image.RGBA, _ State) {
	b := dst.Bounds()
	lay := LayoutFor(b.Dx(), b.Dy())
	if lay.AvatarSize <= 0 {
		return
	}
	scaled := Resample(l.Img, lay.AvatarSize, lay.AvatarSize)
	x, y := b.Min.X+lay.AvatarX, b.Min.Y+lay.AvatarY
	draw.Draw(dst, image.Rect(x, y, x+lay.AvatarSize, y+lay.AvatarSize), scaled, image.Point{}, draw.Over)
}

// BarLayer is the progress bar: a cream track of 20 % opacity, the coral fill with a warm light at its head and a
// brighter band running along it, or, while the percentage is unknown, a coral piece running to and fro.
type BarLayer struct{}

func (BarLayer) Static() bool { return false }
func (BarLayer) Draw(dst *image.RGBA, st State) {
	b := dst.Bounds()
	lay := LayoutFor(b.Dx(), b.Dy())
	x0, y0, bw, bh := b.Min.X+lay.BarX, b.Min.Y+lay.BarY, lay.BarW, lay.BarH
	r := float64(bh) / 2
	fill := rgba(BarFill)
	cream := color.RGBA{255, 245, 229, 255}
	// coverage of the pill at (dx, dy): 1 inside, soft at the edge
	cover := func(dx, dy int) float64 {
		cx := math.Min(math.Max(float64(dx)+0.5, r), float64(bw)-r)
		d := math.Hypot(float64(dx)+0.5-cx, float64(dy)+0.5-r)
		return math.Min(math.Max(r-d+0.5, 0), 1)
	}
	var lo, hi, band float64 // the lit part of the bar in pixels, and where the bright band is
	if st.Indeterminate {
		seg := float64(bw) * 0.3
		phase := float64(st.Elapsed%(2400*time.Millisecond)) / float64(2400*time.Millisecond)
		pp := 1 - math.Abs(2*phase-1) // 0 -> 1 -> 0
		pp = pp * pp * (3 - 2*pp)
		lo = pp * (float64(bw) - seg)
		hi = lo + seg
		band = -1
	} else {
		p := math.Min(math.Max(st.Progress, 0), 1)
		hi = float64(bw) * p
		const period = 1800 * time.Millisecond
		band = float64(st.Elapsed%period)/float64(period)*(hi+float64(bw)*0.18) - float64(bw)*0.09
	}
	for dy := 0; dy < bh; dy++ {
		for dx := 0; dx < bw; dx++ {
			cov := cover(dx, dy)
			if cov <= 0 {
				continue
			}
			px := dst.RGBAAt(x0+dx, y0+dy)
			c := blend(px, cream, 0.2*cov) // the track
			if fx := float64(dx) + 0.5; fx >= lo && fx < hi {
				a := cov
				if st.Indeterminate { // soft ends of the moving piece
					a *= math.Min(1, math.Min(fx-lo, hi-fx)/(float64(bw)*0.05))
				}
				fc := fill
				if band >= 0 {
					if k := 1 - math.Abs(fx-band)/(float64(bw)*0.09); k > 0 {
						fc = mix(fill, color.RGBA{255, 214, 190, 255}, k*k*0.55)
					}
				}
				c = blend(c, fc, a)
			}
			dst.SetRGBA(x0+dx, y0+dy, c)
		}
	}
	if !st.Indeterminate && hi > 0 {
		// the warm light at the head of the fill, kept inside the track
		hx := math.Min(math.Max(hi, lay.HaloR), float64(bw)-lay.HaloR)
		dot(dst, float64(x0)+hx, float64(y0)+r, lay.HaloR, color.RGBA{255, 153, 91, 255}, 57.0/255)
		dot(dst, float64(x0)+hx, float64(y0)+r, lay.HeadR, color.RGBA{255, 189, 109, 255}, 1)
	}
}

// dot blends a disc of colour c (opacity alpha) around (cx, cy) into dst.
func dot(dst *image.RGBA, cx, cy, rad float64, c color.RGBA, alpha float64) {
	b := dst.Bounds()
	for y := int(cy - rad - 1); y <= int(cy+rad+1); y++ {
		for x := int(cx - rad - 1); x <= int(cx+rad+1); x++ {
			if !(image.Point{x, y}).In(b) {
				continue
			}
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if k := math.Min(math.Max(rad-d+0.5, 0), 1); k > 0 {
				dst.SetRGBA(x, y, blend(dst.RGBAAt(x, y), c, alpha*k))
			}
		}
	}
}

func blend(under, over color.RGBA, a float64) color.RGBA {
	a = math.Min(math.Max(a, 0), 1)
	f := func(u, o uint8) uint8 { return uint8(float64(u)*(1-a) + float64(o)*a + 0.5) }
	return color.RGBA{f(under.R, over.R), f(under.G, over.G), f(under.B, over.B), 255}
}

func mix(a, b color.RGBA, t float64) color.RGBA { return blend(a, b, t) }

var (
	avatarOnce sync.Once
	avatarImg  image.Image
)

// Avatar is the brand avatar the placeholder shows (transparent background); nil when the embedded picture cannot be
// read.
func Avatar() image.Image {
	avatarOnce.Do(func() {
		img, err := png.Decode(bytes.NewReader(avatarPNG))
		if err == nil {
			avatarImg = img
		}
	})
	return avatarImg
}

// AvatarPNG is the embedded avatar file as it is (the manager's page serves the same bytes from its own assets).
func AvatarPNG() []byte { return avatarPNG }

// NewPlaceholder is the placeholder screen: the avatar on the brand brown, the bar, the percentage and the step.
func NewPlaceholder() *Stack {
	layers := []Layer{RadialLayer{}}
	if a := Avatar(); a != nil {
		layers = append(layers, AvatarLayer{Img: a})
	}
	layers = append(layers, BarLayer{})
	return &Stack{Layers: layers, Lines: placeholderLines}
}

// Percent is the number shown at the right of the bar: "NN%", or "…" while there is no real percentage.
func Percent(st State) string {
	if st.Indeterminate {
		return "…"
	}
	return fmt.Sprintf("%d%%", int(math.Min(math.Max(st.Progress, 0), 1)*100+0.5))
}

// placeholderLines puts the percentage at the right of the bar, centred under it the step, or the note instead.
func placeholderLines(w, h int, st State) []Label {
	l := LayoutFor(w, h)
	out := []Label{{Text: Percent(st), X: l.PctX, Y: l.BarMid - l.PctH/2, W: min(l.PctW, w-l.PctX), H: l.PctH, Px: l.PctPx, RGB: TextPct,
		Bold: true, Left: true}}
	switch {
	case st.Note != "":
		out = append(out, Label{Text: st.Note, X: l.CenterX - l.NoteW/2, Y: l.StepY - l.StepPx*3/4, W: l.NoteW, H: l.StepPx * 3 / 2,
			Px: l.StepPx, RGB: TextNote, Bold: true})
	case st.Step != "":
		out = append(out, Label{Text: st.Step, X: l.CenterX - l.StepW/2, Y: l.StepY - l.StepPx*3/4, W: l.StepW, H: l.StepPx * 3 / 2,
			Px: l.StepPx, RGB: TextStep})
	}
	return out
}

// Resample scales img to w x h (area average when it shrinks, bilinear when it grows) keeping its transparency.
func Resample(img image.Image, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := img.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if w <= 0 || h <= 0 || sw <= 0 || sh <= 0 {
		return out
	}
	src, ok := img.(*image.RGBA)
	if !ok {
		src = image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(src, src.Bounds(), img, sb.Min, draw.Src)
	}
	for y := 0; y < h; y++ {
		sy0, sy1 := float64(y)*float64(sh)/float64(h), float64(y+1)*float64(sh)/float64(h)
		for x := 0; x < w; x++ {
			sx0, sx1 := float64(x)*float64(sw)/float64(w), float64(x+1)*float64(sw)/float64(w)
			var r, g, b, a, n float64
			for yy := int(sy0); float64(yy) < sy1 && yy < sh; yy++ {
				wy := math.Min(float64(yy+1), sy1) - math.Max(float64(yy), sy0)
				for xx := int(sx0); float64(xx) < sx1 && xx < sw; xx++ {
					wx := math.Min(float64(xx+1), sx1) - math.Max(float64(xx), sx0)
					k := wx * wy
					o := src.PixOffset(xx, yy)
					r += float64(src.Pix[o]) * k
					g += float64(src.Pix[o+1]) * k
					b += float64(src.Pix[o+2]) * k
					a += float64(src.Pix[o+3]) * k
					n += k
				}
			}
			if n == 0 { // growing: one source pixel
				o := src.PixOffset(min(int(sx0), sw-1), min(int(sy0), sh-1))
				copy(out.Pix[out.PixOffset(x, y):], src.Pix[o:o+4])
				continue
			}
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = uint8(r/n+0.5), uint8(g/n+0.5), uint8(b/n+0.5), uint8(a/n+0.5)
		}
	}
	return out
}

// Ease moves a shown value toward its target the way a progress bar should: quickly at first, settling, never back.
type Ease struct {
	v float64
	// Tau is the time constant (default 0.6 s): after Tau the gap to the target is a third of what it was.
	Tau time.Duration
}

// Step moves toward target by the time dt that passed and returns the value to show (0 to 1).
func (e *Ease) Step(target float64, dt time.Duration) float64 {
	tau := e.Tau
	if tau <= 0 {
		tau = 600 * time.Millisecond
	}
	target = math.Min(math.Max(target, 0), 1)
	if target > e.v {
		e.v += (target - e.v) * (1 - math.Exp(-float64(dt)/float64(tau)))
	}
	return e.v
}

// Value is the value shown now.
func (e *Ease) Value() float64 { return e.v }
