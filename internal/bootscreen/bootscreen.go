// Package bootscreen draws the boot screen: what is laid over the emulator's window from the moment it shows until the
// instance is ready (ModAVD/ModAVD#259), and what the manager's view of an instance shows while it boots. No window
// code here: a Screen paints a frame into an image and says which lines of text go over it, and whoever owns a window
// (emu's overlay, the page) shows that.
//
// The picture is the placeholder: the brand avatar (avatar.png, the brand's transparent avatar at 512 px) in the
// middle on the deep brown of the brand banner, an ordinary progress bar under it, and a light that keeps moving along
// the bar, so the screen is alive while the progress stands still. It is built from layers drawn back to front
// (Layer), so the final artwork (the character in parts, the screen she holds filling in with the progress) replaces
// the avatar layer and adds layers that read State.Progress, without touching the windows or the progress logic.
package bootscreen

import (
	"bytes"
	_ "embed"
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

// The brand colours of the placeholder (0xRRGGBB, as the banner has them).
const (
	Background = 0x36211E
	BarTrack   = 0x55372F
	BarFill    = 0xF2A65A
	BarGlow    = 0xFFE2B8
	TextMain   = 0xF6E7D8
	TextSub    = 0xC9A894
	TextNote   = 0xFFC98A
)

// State is everything one frame depends on.
type State struct {
	// Progress is the bar, 0 to 1 (already smoothed by the caller, see Ease).
	Progress float64
	// Elapsed is the animation clock: any increasing time, the light along the bar goes by it.
	Elapsed time.Duration
	// Title is the main line (what is being done), Detail the small line under the bar (the percentage and the like),
	// Note a line at the bottom for what the user should know (the setup did not finish in time). All already in the
	// language of the manager.
	Title, Detail, Note string
}

// Label is a line of text to draw over the frame: centred in the rectangle, Px pixels high.
type Label struct {
	Text       string
	X, Y, W, H int
	Px         int
	RGB        uint32
	Bold       bool
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

// FillLayer paints the whole frame one colour.
type FillLayer struct{ RGB uint32 }

func (FillLayer) Static() bool { return true }
func (f FillLayer) Draw(dst *image.RGBA, _ State) {
	draw.Draw(dst, dst.Bounds(), &image.Uniform{rgba(f.RGB)}, image.Point{}, draw.Src)
}

// ImageLayer paints an image (with its transparency) of a fixed share of the frame's height, centred horizontally and
// with its centre at CenterY of the frame's height.
type ImageLayer struct {
	Img     image.Image
	HeightF float64 // the picture's height as a share of the frame's
	CenterY float64 // where its middle is, as a share of the frame's height
}

func (ImageLayer) Static() bool { return true }
func (l ImageLayer) Draw(dst *image.RGBA, _ State) {
	b := dst.Bounds()
	h := int(float64(b.Dy())*l.HeightF + 0.5)
	sb := l.Img.Bounds()
	if h <= 0 || sb.Dy() <= 0 {
		return
	}
	w := h * sb.Dx() / sb.Dy()
	scaled := Resample(l.Img, w, h)
	x := b.Min.X + (b.Dx()-w)/2
	y := b.Min.Y + int(float64(b.Dy())*l.CenterY) - h/2
	draw.Draw(dst, image.Rect(x, y, x+w, y+h), scaled, image.Point{}, draw.Over)
}

// BarGeometry is where the progress bar is in a frame of w x h pixels: the track's rectangle, and how round it is.
func BarGeometry(w, h int) (x, y, bw, bh int) {
	bw = max(int(float64(w)*0.46), min(w-16, 240))
	bh = max(int(float64(h)*0.014), 6)
	x = (w - bw) / 2
	y = int(float64(h) * 0.775)
	return
}

// BarLayer is the progress bar and the light that moves along it.
type BarLayer struct{}

func (BarLayer) Static() bool { return false }
func (BarLayer) Draw(dst *image.RGBA, st State) {
	b := dst.Bounds()
	x0, y0, bw, bh := BarGeometry(b.Dx(), b.Dy())
	x0 += b.Min.X
	y0 += b.Min.Y
	p := math.Min(math.Max(st.Progress, 0), 1)
	fill := int(float64(bw)*p + 0.5)
	// the light: a soft band travelling along the filled part, a new one every 1.8 s
	const period = 1800 * time.Millisecond
	phase := float64(st.Elapsed%period) / float64(period)
	band := math.Max(float64(bw)*0.18, 24)
	centre := phase*(float64(fill)+band) - band/2
	track, fillC, glow := rgba(BarTrack), rgba(BarFill), rgba(BarGlow)
	r := float64(bh) / 2
	for dy := 0; dy < bh; dy++ {
		for dx := 0; dx < bw; dx++ {
			// pill shape: the ends are half circles
			cx := math.Min(math.Max(float64(dx)+0.5, r), float64(bw)-r)
			d := math.Hypot(float64(dx)+0.5-cx, float64(dy)+0.5-r)
			if d > r {
				continue
			}
			c := track
			if dx < fill {
				c = fillC
				if k := 1 - math.Abs(float64(dx)-centre)/(band/2); k > 0 && fill > 0 {
					c = mix(fillC, glow, k*k*0.85)
				}
			}
			if d > r-1 { // a soft edge
				c = mix(rgba(Background), c, r-d)
			}
			dst.SetRGBA(x0+dx, y0+dy, c)
		}
	}
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	t = math.Min(math.Max(t, 0), 1)
	f := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t + 0.5) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 255}
}

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

// NewPlaceholder is the placeholder screen: the avatar on the brand brown, the bar, and the lines of State.
func NewPlaceholder() *Stack {
	layers := []Layer{FillLayer{Background}}
	if a := Avatar(); a != nil {
		layers = append(layers, ImageLayer{Img: a, HeightF: 0.62, CenterY: 0.38})
	}
	layers = append(layers, BarLayer{})
	return &Stack{Layers: layers, Lines: placeholderLines}
}

// placeholderLines puts the title above the bar, the detail under it and the note at the bottom.
func placeholderLines(w, h int, st State) []Label {
	x, y, bw, bh := BarGeometry(w, h)
	pad := bw / 8
	line := func(text string, top, px int, rgb uint32, bold bool) Label {
		return Label{Text: text, X: x - pad, Y: top, W: bw + 2*pad, H: px * 3 / 2, Px: px, RGB: rgb, Bold: bold}
	}
	big, small := max(h*34/1000, 14), max(h*26/1000, 12)
	var out []Label
	if st.Title != "" {
		out = append(out, line(st.Title, y-big*2-bh, big, TextMain, true))
	}
	if st.Detail != "" {
		out = append(out, line(st.Detail, y+bh+small/2, small, TextSub, false))
	}
	if st.Note != "" {
		out = append(out, line(st.Note, y+bh+small*3, small, TextNote, true))
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
