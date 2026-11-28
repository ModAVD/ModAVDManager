package bootscreen

// The v3 artwork (art/): the character in 16 parts that move a little (rig.json: each part's pivot, parent, amplitude, period
// and phase), the screen she holds (screen-state-N.png, N the progress in percent, cross-faded between the two nearest), and
// the eruption when the instance is ready (the screen's 15 key frames, five hair parts that the wind moves, seven small
// particles outside the screen, then everything fades). The program draws only the bar, the number and the step line (Lines).
//
// The composition is built in a bitmap of the rig's 1024 x 1024 space scaled by k (the avatar's size on the frame / 1024), so
// a part is drawn once at its final size: it is scaled first (area average, kept for the size), then only moved and turned a
// little (bilinear, a fraction of a pixel to a few pixels) when a frame is made. The bitmap is re-made only when the clock has
// moved a quantum (80 ms; 33 ms during the eruption) or the screen's progress has moved by 1 %, and laid over the frame.

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/fs"
	"math"
	"sort"
	"sync"
	"time"
)

//go:embed art/*.png art/rig.json
var artFS embed.FS

// ArtFS is the artwork's files (rig.json and the pngs it names); the manager's page serves the same files.
func ArtFS() fs.FS {
	sub, err := fs.Sub(artFS, "art")
	if err != nil {
		return artFS
	}
	return sub
}

type rigMotion struct {
	RotateDeg  float64 `json:"rotateDeg"`
	TranslateX float64 `json:"translateX"`
	TranslateY float64 `json:"translateY"`
	PeriodMs   float64 `json:"periodMs"`
	Phase      float64 `json:"phase"`
}

type rigPart struct {
	ID     string     `json:"id"`
	File   string     `json:"file"`
	Z      int        `json:"z"`
	Origin [2]float64 `json:"origin"`
	Size   [2]float64 `json:"size"`
	Pivot  [2]float64 `json:"pivot"`
	Parent string     `json:"parent"`
	Motion rigMotion  `json:"motion"`
}

type rigGroup struct {
	Parent *string    `json:"parent"`
	Pivot  [2]float64 `json:"pivot"`
	Motion rigMotion  `json:"motion"`
}

type rigParticle struct {
	Sprite     string     `json:"sprite"`
	BirthMs    float64    `json:"birthMs"`
	DurationMs float64    `json:"durationMs"`
	StartRig   [2]float64 `json:"startRig"`
	EndRig     [2]float64 `json:"endRig"`
	MaxWidth   float64    `json:"maxWidth"`
	RotateDeg  float64    `json:"rotateDeg"`
}

type rigEruption struct {
	FrameMs       []float64            `json:"frameMs"`
	Frames        []string             `json:"frames"`
	WindAmplitude [][2]float64         `json:"windAmplitude"`
	WindImpulse   map[string]rigMotion `json:"windImpulse"`
	Particles     []rigParticle        `json:"particles"`
	FadeStartMs   float64              `json:"fadeStartMs"`
	FadeEndMs     float64              `json:"fadeEndMs"`
}

type rigFile struct {
	Version      int `json:"version"`
	ScreenStates []struct {
		File string  `json:"file"`
		P    float64 `json:"p"`
	} `json:"screenStates"`
	Parts    []rigPart           `json:"parts"`
	Groups   map[string]rigGroup `json:"groups"`
	Eruption rigEruption         `json:"eruption"`
}

// Art is the decoded artwork (premultiplied RGBA at native size).
type Art struct {
	rig     rigFile
	parts   map[string]*image.RGBA
	states  []*image.RGBA
	frames  []*image.RGBA
	sprites map[string]*image.RGBA
	screen  rigPart // the part with no file: where the screen goes
}

var (
	artOnce sync.Once
	artVal  *Art
	artErr  error
)

// LoadArt decodes the embedded artwork once.
func LoadArt() (*Art, error) {
	artOnce.Do(func() { artVal, artErr = decodeArt(ArtFS()) })
	return artVal, artErr
}

func decodePNG(fsys fs.FS, name string) (*image.RGBA, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	return out, nil
}

func decodeArt(fsys fs.FS) (*Art, error) {
	raw, err := fs.ReadFile(fsys, "rig.json")
	if err != nil {
		return nil, err
	}
	a := &Art{parts: map[string]*image.RGBA{}, sprites: map[string]*image.RGBA{}}
	if err := json.Unmarshal(raw, &a.rig); err != nil {
		return nil, fmt.Errorf("rig.json: %w", err)
	}
	if a.rig.Version != 3 || len(a.rig.Parts) == 0 || len(a.rig.ScreenStates) == 0 {
		return nil, fmt.Errorf("rig.json: not the v3 rig")
	}
	sort.SliceStable(a.rig.Parts, func(i, j int) bool { return a.rig.Parts[i].Z < a.rig.Parts[j].Z })
	for _, p := range a.rig.Parts {
		if p.File == "" {
			a.screen = p
			continue
		}
		if a.parts[p.ID], err = decodePNG(fsys, p.File); err != nil {
			return nil, err
		}
	}
	if a.screen.ID == "" {
		return nil, fmt.Errorf("rig.json: no screen part")
	}
	for _, s := range a.rig.ScreenStates {
		img, err := decodePNG(fsys, s.File)
		if err != nil {
			return nil, err
		}
		a.states = append(a.states, img)
	}
	e := a.rig.Eruption
	if len(e.Frames) == 0 || len(e.Frames) != len(e.FrameMs) {
		return nil, fmt.Errorf("rig.json: the eruption frames do not match their times")
	}
	for _, f := range e.Frames {
		img, err := decodePNG(fsys, f)
		if err != nil {
			return nil, err
		}
		a.frames = append(a.frames, img)
	}
	for _, p := range e.Particles {
		if a.sprites[p.Sprite] != nil {
			continue
		}
		if a.sprites[p.Sprite], err = decodePNG(fsys, "sprite-"+p.Sprite+".png"); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// EruptionLength is how long the picture goes on after the instance is ready (the eruption and the fade); the overlay shows it
// that long.
func EruptionLength() time.Duration {
	if a, err := LoadArt(); err == nil {
		return time.Duration(a.rig.Eruption.FadeEndMs * float64(time.Millisecond))
	}
	return 2210 * time.Millisecond
}

// Fade is how opaque the whole picture is at st (1, then falling to 0 at the end of the eruption), for the window or the page
// to apply to all of it, the bar and the lines included.
func Fade(st State) float64 {
	if !st.Done {
		return 1
	}
	from, to := 1660.0, 2210.0
	if a, err := LoadArt(); err == nil {
		from, to = a.rig.Eruption.FadeStartMs, a.rig.Eruption.FadeEndMs
	}
	ms := float64(st.DoneFor) / float64(time.Millisecond)
	if ms <= from {
		return 1
	}
	if ms >= to {
		return 0
	}
	x := (ms - from) / (to - from)
	return 1 - x*x*(3-2*x)
}

// ---- transforms ------------------------------------------------------------------------------------------------------------

// xf is "turn by theta about the pivot, then move by t" (rig units).
type xf struct{ px, py, c, s, tx, ty float64 }

func motionXf(pivot [2]float64, m rigMotion, tMs float64, extra rigMotion, wind float64) xf {
	j := 0.0
	if m.PeriodMs > 0 {
		j = math.Sin(2*math.Pi*tMs/m.PeriodMs + m.Phase)
	}
	rot := (m.RotateDeg*j + extra.RotateDeg*wind) * math.Pi / 180
	return xf{px: pivot[0], py: pivot[1], c: math.Cos(rot), s: math.Sin(rot), tx: m.TranslateX*j + extra.TranslateX*wind,
		ty: m.TranslateY*j + extra.TranslateY*wind}
}

func (f xf) fwd(x, y float64) (float64, float64) {
	dx, dy := x-f.px, y-f.py
	return f.px + f.c*dx - f.s*dy + f.tx, f.py + f.s*dx + f.c*dy + f.ty
}

func (f xf) inv(x, y float64) (float64, float64) {
	dx, dy := x-f.px-f.tx, y-f.py-f.ty
	return f.px + f.c*dx + f.s*dy, f.py - f.s*dx + f.c*dy
}

// chain applies the transforms in order (a part's own, then its parents').
type chain []xf

func (c chain) fwd(x, y float64) (float64, float64) {
	for _, f := range c {
		x, y = f.fwd(x, y)
	}
	return x, y
}

func (c chain) inv(x, y float64) (float64, float64) {
	for i := len(c) - 1; i >= 0; i-- {
		x, y = c[i].inv(x, y)
	}
	return x, y
}

// ---- the artwork scaled to a frame size ---------------------------------------------------------------------------------------

// padded is an image with a transparent border of 1 pixel, so that bilinear sampling needs no edge cases.
type padded struct {
	img  *image.RGBA
	w, h int // the picture's own size
}

func newPadded(src *image.RGBA, w, h int) *padded {
	w, h = max(w, 1), max(h, 1)
	s := Resample(src, w, h)
	out := image.NewRGBA(image.Rect(0, 0, w+2, h+2))
	for y := 0; y < h; y++ {
		copy(out.Pix[(y+1)*out.Stride+4:], s.Pix[y*s.Stride:y*s.Stride+4*w])
	}
	return &padded{img: out, w: w, h: h}
}

type scaledSet struct {
	n       int // the bitmap's side: the rig's 1024 at scale k
	k       float64
	parts   map[string]*padded
	states  []*image.RGBA
	frames  []*image.RGBA
	sprites map[string]*padded
}

func (a *Art) scaled(n int) *scaledSet {
	k := float64(n) / 1024
	set := &scaledSet{n: n, k: k, parts: map[string]*padded{}, sprites: map[string]*padded{}}
	for _, p := range a.rig.Parts {
		if img := a.parts[p.ID]; img != nil {
			set.parts[p.ID] = newPadded(img, int(math.Round(p.Size[0]*k)), int(math.Round(p.Size[1]*k)))
		}
	}
	sz := func(img *image.RGBA) (int, int) {
		return int(math.Round(float64(img.Bounds().Dx()) * k)), int(math.Round(float64(img.Bounds().Dy()) * k))
	}
	for _, s := range a.states {
		w, h := sz(s)
		set.states = append(set.states, Resample(s, w, h))
	}
	for _, f := range a.frames {
		w, h := sz(f)
		set.frames = append(set.frames, Resample(f, w, h))
	}
	for _, p := range a.rig.Eruption.Particles {
		src := a.sprites[p.Sprite]
		if set.sprites[p.Sprite] != nil || src == nil {
			continue
		}
		w := max(int(math.Round(p.MaxWidth*k)), 2)
		set.sprites[p.Sprite] = newPadded(src, w, int(math.Round(float64(w)*float64(src.Bounds().Dy())/float64(src.Bounds().Dx()))))
	}
	return set
}

// ---- drawing ---------------------------------------------------------------------------------------------------------------

// blit lays the picture src (which fills the rig rectangle ox, oy, w, h before the transforms c) over dst, scaled by k and
// multiplied by alpha: bilinear, the inverse mapping evaluated as an affine function along the rows.
func blit(dst *image.RGBA, src *padded, ox, oy, w, h float64, c chain, k, alpha float64) {
	if w <= 0 || h <= 0 || alpha <= 0 {
		return
	}
	db := dst.Bounds()
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range [4][2]float64{{ox, oy}, {ox + w, oy}, {ox, oy + h}, {ox + w, oy + h}} {
		x, y := c.fwd(p[0], p[1])
		minX, maxX = math.Min(minX, x*k), math.Max(maxX, x*k)
		minY, maxY = math.Min(minY, y*k), math.Max(maxY, y*k)
	}
	x0, x1 := max(int(math.Floor(minX))-1, db.Min.X), min(int(math.Ceil(maxX))+1, db.Max.X)
	y0, y1 := max(int(math.Floor(minY))-1, db.Min.Y), min(int(math.Ceil(maxY))+1, db.Max.Y)
	if x0 >= x1 || y0 >= y1 {
		return
	}
	sx, sy := float64(src.w)/w, float64(src.h)/h
	uv := func(px, py float64) (float64, float64) {
		x, y := c.inv((px+0.5)/k, (py+0.5)/k)
		return (x-ox)*sx - 0.5 + 1, (y-oy)*sy - 0.5 + 1 // + 1: the border
	}
	u00, v00 := uv(0, 0)
	u10, v10 := uv(1, 0)
	u01, v01 := uv(0, 1)
	dudx, dvdx, dudy, dvdy := u10-u00, v10-v00, u01-u00, v01-v00
	pix, st := src.img.Pix, src.img.Stride
	limU, limV := float64(src.w+1), float64(src.h+1)
	for y := y0; y < y1; y++ {
		u := u00 + dudx*float64(x0) + dudy*float64(y)
		v := v00 + dvdx*float64(x0) + dvdy*float64(y)
		o := (y-db.Min.Y)*dst.Stride + (x0-db.Min.X)*4
		for x := x0; x < x1; x, o, u, v = x+1, o+4, u+dudx, v+dvdx {
			if u < 0 || v < 0 || u >= limU || v >= limV {
				continue
			}
			ui, vi := int(u), int(v)
			fu, fv := u-float64(ui), v-float64(vi)
			p := vi*st + ui*4
			a00, a10, a01, a11 := float64(pix[p+3]), float64(pix[p+7]), float64(pix[p+st+3]), float64(pix[p+st+7])
			if a00+a10+a01+a11 == 0 {
				continue
			}
			w00, w10, w01, w11 := (1-fu)*(1-fv), fu*(1-fv), (1-fu)*fv, fu*fv
			a := (a00*w00 + a10*w10 + a01*w01 + a11*w11) * alpha
			if a < 0.5 {
				continue
			}
			inv := 1 - a/255
			sc := func(ch int) float64 {
				return (float64(pix[p+ch])*w00 + float64(pix[p+4+ch])*w10 + float64(pix[p+st+ch])*w01 + float64(pix[p+st+4+ch])*w11) * alpha
			}
			d := dst.Pix[o : o+4 : o+4]
			d[0] = clamp8(sc(0) + float64(d[0])*inv)
			d[1] = clamp8(sc(1) + float64(d[1])*inv)
			d[2] = clamp8(sc(2) + float64(d[2])*inv)
			d[3] = clamp8(a + float64(d[3])*inv)
		}
	}
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// crossfade is a * (1 - f) + b * f (premultiplied RGBA of one size) into out.
func crossfade(out, a, b *image.RGBA, f float64) {
	w := int(f*256 + 0.5)
	for i := range out.Pix {
		out.Pix[i] = uint8((int(a.Pix[i])*(256-w) + int(b.Pix[i])*w + 128) >> 8)
	}
}

// piecewise finds where t falls among the sorted times xs: the index i of the segment and the fraction in it (the last one
// beyond the end, the first before the start).
func piecewise(xs []float64, t float64) (int, float64) {
	if len(xs) < 2 || t <= xs[0] {
		return 0, 0
	}
	for i := 0; i+1 < len(xs); i++ {
		if t < xs[i+1] {
			return i, (t - xs[i]) / (xs[i+1] - xs[i])
		}
	}
	return len(xs) - 2, 1
}

func (a *Art) windAt(ms float64) float64 {
	w := a.rig.Eruption.WindAmplitude
	if ms <= 0 || len(w) == 0 || ms >= w[len(w)-1][0] {
		return 0
	}
	for i := 0; i+1 < len(w); i++ {
		if ms < w[i+1][0] {
			return w[i][1] + (w[i+1][1]-w[i][1])*(ms-w[i][0])/(w[i+1][0]-w[i][0])
		}
	}
	return 0
}

// CharacterLayer draws the character, the screen and the eruption.
type CharacterLayer struct {
	art *Art

	mu      sync.Mutex
	set     *scaledSet
	bmp     *image.RGBA
	mixed   *image.RGBA // the screen's cross-fade
	key     int64
	prog    float64
	done    bool
	have    bool
	renders int // how many times the bitmap was made (the tests)
}

// NewCharacterLayer is the layer of art.
func NewCharacterLayer(a *Art) *CharacterLayer { return &CharacterLayer{art: a} }

func (l *CharacterLayer) Static() bool { return false }

// Quanta of the animation clock.
const (
	idleQuantum     = 80 * time.Millisecond
	eruptionQuantum = 33 * time.Millisecond
)

func (l *CharacterLayer) Draw(dst *image.RGBA, st State) {
	b := dst.Bounds()
	lay := LayoutFor(b.Dx(), b.Dy())
	n := lay.AvatarSize
	if n < 16 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set == nil || l.set.n != n {
		l.set = l.art.scaled(n)
		l.bmp = image.NewRGBA(image.Rect(0, 0, n, n))
		l.mixed = nil
		l.have = false
	}
	prog := math.Min(math.Max(st.Progress, 0), 1)
	if st.Indeterminate {
		prog = 0
	}
	clock, q := st.Elapsed, idleQuantum
	if st.Done {
		clock, q = st.DoneFor, eruptionQuantum
	}
	key := int64(clock / q)
	if !l.have || key != l.key || st.Done != l.done || (!st.Done && math.Abs(prog-l.prog) >= 0.01) {
		l.render(st, prog)
		l.have, l.key, l.prog, l.done = true, key, prog, st.Done
	}
	r := image.Rect(b.Min.X+lay.AvatarX, b.Min.Y+lay.AvatarY, b.Min.X+lay.AvatarX+n, b.Min.Y+lay.AvatarY+n)
	// the part of the bitmap that is inside the frame
	clip := r.Intersect(b)
	if clip.Empty() {
		return
	}
	draw.Draw(dst, clip, l.bmp, image.Pt(clip.Min.X-r.Min.X, clip.Min.Y-r.Min.Y), draw.Over)
}

func (l *CharacterLayer) render(st State, prog float64) {
	l.renders++
	a, set, bmp := l.art, l.set, l.bmp
	for i := range bmp.Pix {
		bmp.Pix[i] = 0
	}
	tMs := float64(st.Elapsed) / float64(time.Millisecond)
	er, wind := -1.0, 0.0
	if st.Done {
		er = float64(st.DoneFor) / float64(time.Millisecond)
		wind = a.windAt(er)
	}
	parts := map[string]rigPart{}
	for _, p := range a.rig.Parts {
		parts[p.ID] = p
	}
	groupChain := func(name string) chain {
		var c chain
		for name != "" {
			g, ok := a.rig.Groups[name]
			if !ok {
				break
			}
			if g.Motion.PeriodMs > 0 {
				c = append(c, motionXf(g.Pivot, g.Motion, tMs, rigMotion{}, 0))
			}
			if g.Parent == nil {
				break
			}
			name = *g.Parent
		}
		return c
	}
	for _, p := range a.rig.Parts {
		if p.File == "" {
			l.drawScreen(st, prog, er)
			continue
		}
		c := append(chain{motionXf(p.Pivot, p.Motion, tMs, a.rig.Eruption.WindImpulse[p.ID], wind)}, groupChain(p.Parent)...)
		blit(bmp, set.parts[p.ID], p.Origin[0], p.Origin[1], p.Size[0], p.Size[1], c, set.k, 1)
	}
	// outside the screen, after every part of the character
	if er >= 0 {
		for _, p := range a.rig.Eruption.Particles {
			u := (er - p.BirthMs) / p.DurationMs
			sp := set.sprites[p.Sprite]
			if u < 0 || u >= 1 || sp == nil {
				continue
			}
			e := 1 - (1-u)*(1-u)
			cx, cy := p.StartRig[0]+(p.EndRig[0]-p.StartRig[0])*e, p.StartRig[1]+(p.EndRig[1]-p.StartRig[1])*e
			w := p.MaxWidth * (0.45 + 0.55*math.Min(1, u*3))
			h := w * float64(sp.h) / float64(sp.w)
			rot := xf{px: cx, py: cy, c: math.Cos(p.RotateDeg * u * math.Pi / 180), s: math.Sin(p.RotateDeg * u * math.Pi / 180)}
			alpha := math.Min(1, u*8) * math.Pow(1-u, 0.9)
			blit(bmp, sp, cx-w/2, cy-h/2, w, h, chain{rot}, set.k, alpha)
		}
	}
}

// drawScreen paints what is in the screen: the state the progress is at (two of them cross-faded), or, once the instance is
// ready, the eruption's key frames.
func (l *CharacterLayer) drawScreen(st State, prog, er float64) {
	a, set := l.art, l.set
	var img *image.RGBA
	if er >= 0 {
		i, f := piecewise(a.rig.Eruption.FrameMs, er)
		img = l.blend(set.frames, i, f)
	} else {
		ps := make([]float64, len(a.rig.ScreenStates))
		for i, s := range a.rig.ScreenStates {
			ps[i] = s.P
		}
		i, f := piecewise(ps, prog)
		img = l.blend(set.states, i, f)
	}
	x, y := int(math.Round(a.screen.Origin[0]*set.k)), int(math.Round(a.screen.Origin[1]*set.k))
	r := image.Rect(x, y, x+img.Bounds().Dx(), y+img.Bounds().Dy())
	draw.Draw(l.bmp, r, img, image.Point{}, draw.Over)
}

func (l *CharacterLayer) blend(imgs []*image.RGBA, i int, f float64) *image.RGBA {
	if f <= 0.004 || i+1 >= len(imgs) {
		return imgs[i]
	}
	if f >= 0.996 {
		return imgs[i+1]
	}
	if l.mixed == nil || l.mixed.Bounds() != imgs[i].Bounds() {
		l.mixed = image.NewRGBA(imgs[i].Bounds())
	}
	crossfade(l.mixed, imgs[i], imgs[i+1], f)
	return l.mixed
}

// NewScreen is the boot screen: the brand's dark background, the character with the screen she holds, the bar, the
// percentage and the step line. When the artwork cannot be read (it is embedded, so only a broken build) the picture is the
// background and the bar.
func NewScreen() *Stack {
	layers := []Layer{RadialLayer{}}
	if a, err := LoadArt(); err == nil {
		layers = append(layers, NewCharacterLayer(a))
	}
	layers = append(layers, BarLayer{})
	return &Stack{Layers: layers, Lines: Lines}
}
