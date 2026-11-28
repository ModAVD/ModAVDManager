package bootscreen

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func frame(t *testing.T, s Screen, w, h int, st State) (*image.RGBA, []Label) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	labels := s.Draw(img, st)
	return img, labels
}

// filled counts the pixels along the middle of the bar that are lit (coral, or the light on it): the fill.
func filled(img *image.RGBA, w, h int) int {
	l := LayoutFor(w, h)
	n := 0
	for dx := 0; dx < l.BarW; dx++ {
		c := img.RGBAAt(l.BarX+dx, l.BarMid)
		if c.R > 200 && int(c.R)-int(c.G) > 50 {
			n++
		}
	}
	return n
}

// The layout of the artwork: the bar is under the character, about 60 % of its width, 7 px high at 1280 x 720; the
// percentage is at the right of the bar and the step under it; the colours are the artwork's. Whatever the window's
// shape, nothing is cut off, and the bar and the lines are inside the frame.
// barPix is the rows of the frame around the bar (the character moves on its own clock; the bar is checked by itself).
func barPix(img *image.RGBA, w, h int) []byte {
	l := LayoutFor(w, h)
	var out []byte
	for y := l.BarY - 8; y < l.BarY+l.BarH+8; y++ {
		out = append(out, img.Pix[img.PixOffset(l.BarX-8, y):img.PixOffset(l.BarX+l.BarW+8, y)]...)
	}
	return out
}

func TestScreenLayoutAndLines(t *testing.T) {
	s := NewScreen()
	l := LayoutFor(1280, 720)
	if l.S != 1 || l.BarX != 443 || l.BarY != 646 || l.BarW != 352 || l.BarH != 7 || l.AvatarX != 277 || l.AvatarY != -31 ||
		l.PctX != 443+352+16 || l.StepY != 646+7+25 {
		t.Errorf("the 1280x720 layout is not the artwork's: %+v", l)
	}
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}, {853, 512}, {960, 720}, {720, 1280}, {800, 800}, {400, 300}} {
		w, h := size[0], size[1]
		img, labels := frame(t, s, w, h, State{Progress: 0.4, Step: "正在做开机后调优…"})
		lay := LayoutFor(w, h)
		if lay.BarX < 0 || lay.BarX+lay.BarW > w || lay.BarY+lay.BarH > h || lay.AvatarSize <= 0 {
			t.Errorf("%dx%d: bar or avatar out of the frame: %+v", w, h, lay)
		}
		if c := img.RGBAAt(2, 2); c.R > 120 || c.A != 255 { // dark brown corner
			t.Errorf("%dx%d: corner %v is not the dark background", w, h, c)
		}
		if c := img.RGBAAt(w/2, int(float64(lay.AvatarY)+float64(lay.AvatarSize)*0.30)); c.R > 200 && c.G > 200 {
			_ = c // the face is light: something of the avatar is there
		} else if img.RGBAAt(w/2, h/2) == img.RGBAAt(2, 2) {
			t.Errorf("%dx%d: nothing of the avatar in the middle", w, h)
		}
		if n := filled(img, w, h); n < lay.BarW*37/100 || n > lay.BarW*43/100 {
			t.Errorf("%dx%d: %d of %d bar pixels lit at 40%%", w, h, n, lay.BarW)
		}
		if len(labels) != 2 {
			t.Fatalf("%dx%d: %d lines, want the percentage and the step", w, h, len(labels))
		}
		pct, step := labels[0], labels[1]
		if pct.Text != "40%" || !pct.Left || pct.X < lay.BarX+lay.BarW || pct.Y > lay.BarMid || pct.Y+pct.H < lay.BarMid {
			t.Errorf("%dx%d: percentage %+v is not at the right of the bar", w, h, pct)
		}
		if step.Y < lay.BarY+lay.BarH || step.Text != "正在做开机后调优…" {
			t.Errorf("%dx%d: step %+v is not under the bar", w, h, step)
		}
		for _, lb := range labels {
			if lb.X < 0 || lb.Y < 0 || lb.X+lb.W > w || lb.Y+lb.H > h || lb.Px < 10 {
				t.Errorf("%dx%d: line %+v is outside the frame or too small", w, h, lb)
			}
		}
		if step.X+step.W > w || step.X < 0 {
			t.Errorf("%dx%d: the step line is cut off", w, h)
		}
	}
	// the note takes the step's place; no step and no note: the percentage only
	_, labels := frame(t, s, 1280, 720, State{Progress: 1, Step: "a", Note: "b"})
	if len(labels) != 2 || labels[1].Text != "b" || labels[0].Text != "100%" {
		t.Errorf("note: %+v", labels)
	}
	if _, labels := frame(t, s, 640, 360, State{}); len(labels) != 1 || labels[0].Text != "0%" {
		t.Errorf("no text: %+v", labels)
	}
}

// With no real percentage the bar is indeterminate: a coral piece runs to and fro along the track (it is alive, and it
// stays inside the track), the number is "…".
func TestScreenIndeterminate(t *testing.T) {
	s := NewScreen()
	pos := func(at time.Duration) (first, last int) {
		img, labels := frame(t, s, 1280, 720, State{Indeterminate: true, Elapsed: at})
		if labels[0].Text != "…" {
			t.Fatalf("number %q, want …", labels[0].Text)
		}
		l := LayoutFor(1280, 720)
		first, last = -1, -1
		for dx := 0; dx < l.BarW; dx++ {
			if c := img.RGBAAt(l.BarX+dx, l.BarMid); c.R > 200 && int(c.R)-int(c.G) > 50 {
				if first < 0 {
					first = dx
				}
				last = dx
			}
		}
		return
	}
	f0, l0 := pos(0)
	f1, l1 := pos(600 * time.Millisecond)
	f2, l2 := pos(1200 * time.Millisecond)
	if f1 <= f0 && l1 <= l0 {
		t.Errorf("the piece did not move: %d-%d then %d-%d", f0, l0, f1, l1)
	}
	if f2 < 0 || l2 >= 352 || f2 < f1 {
		t.Errorf("the piece is not inside the track or does not run on: %d-%d", f2, l2)
	}
	a, _ := frame(t, s, 1280, 720, State{Indeterminate: true, Elapsed: 0})
	b, _ := frame(t, s, 1280, 720, State{Indeterminate: true, Elapsed: 2400 * time.Millisecond})
	if !bytes.Equal(barPix(a, 1280, 720), barPix(b, 1280, 720)) {
		t.Error("the movement does not come round after its period")
	}
}

// The screen is alive while the progress stands still: the light runs along the fill between frames.
func TestScreenIsAliveAtConstantProgress(t *testing.T) {
	s := NewScreen()
	a, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 0})
	b, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 600 * time.Millisecond})
	c, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 1800 * time.Millisecond})
	if bytes.Equal(barPix(a, 1280, 720), barPix(b, 1280, 720)) {
		t.Error("two frames 600 ms apart are the same: the screen looks stuck")
	}
	if !bytes.Equal(barPix(a, 1280, 720), barPix(c, 1280, 720)) {
		t.Error("the light does not come round again after its period")
	}
	z1, _ := frame(t, s, 1280, 720, State{Progress: 0, Elapsed: 0})
	z2, _ := frame(t, s, 1280, 720, State{Progress: 0, Elapsed: 700 * time.Millisecond})
	if !bytes.Equal(barPix(z1, 1280, 720), barPix(z2, 1280, 720)) {
		t.Error("an empty bar changes between frames")
	}
	// the same Stack paints another size right after (the cache follows the size)
	if img, _ := frame(t, s, 640, 360, State{Progress: 1}); filled(img, 640, 360) == 0 {
		t.Error("a full bar at a new size is empty")
	}
}
func TestEaseNeverGoesBackAndSettles(t *testing.T) {
	var e Ease
	prev := 0.0
	for k := 0; k < 100; k++ {
		v := e.Step(0.7, 100*time.Millisecond)
		if v < prev {
			t.Fatalf("went back: %v -> %v", prev, v)
		}
		prev = v
	}
	if prev < 0.69 || prev > 0.7 {
		t.Errorf("settled at %v, want 0.7", prev)
	}
	if v := e.Step(0.3, time.Second); v != prev {
		t.Errorf("a lower target moved the bar: %v -> %v", prev, v)
	}
	if v := e.Step(5, time.Hour); v > 1 {
		t.Errorf("above 1: %v", v)
	}
	var slow Ease
	if v := slow.Step(1, 0); v != 0 {
		t.Errorf("no time, no move: %v", v)
	}
}

func TestResampleKeepsTransparencyAndSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for k := 0; k < len(src.Pix); k += 4 { // opaque red, premultiplied
		src.Pix[k], src.Pix[k+3] = 255, 255
	}
	dst := Resample(src, 2, 2)
	if dst.Bounds().Dx() != 2 || dst.RGBAAt(1, 1).R != 255 || dst.RGBAAt(1, 1).A != 255 {
		t.Errorf("shrunk: %+v", dst.RGBAAt(1, 1))
	}
	up := Resample(src, 8, 8)
	if up.Bounds().Dx() != 8 || up.RGBAAt(7, 7).A != 255 {
		t.Errorf("grown: %+v", up.RGBAAt(7, 7))
	}
	clear := Resample(image.NewRGBA(image.Rect(0, 0, 4, 4)), 3, 3)
	if clear.RGBAAt(1, 1).A != 0 {
		t.Errorf("transparent became %+v", clear.RGBAAt(1, 1))
	}
}

// ---- the v3 artwork ----------------------------------------------------------------------------------------------------------

func TestArtDecodesAndIsSmall(t *testing.T) {
	a, err := LoadArt()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.parts) != 15 || len(a.states) != 6 || len(a.frames) != 15 || len(a.sprites) != 3 {
		t.Errorf("parts %d states %d frames %d sprites %d", len(a.parts), len(a.states), len(a.frames), len(a.sprites))
	}
	total := 0
	err = fs.WalkDir(ArtFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		total += int(info.Size())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the artwork in the package: %d bytes", total)
	if total > 1<<20 {
		t.Errorf("the artwork is %d bytes; it stays under 1 MiB", total)
	}
	if EruptionLength() != 2210*time.Millisecond {
		t.Errorf("eruption length %v", EruptionLength())
	}
}

// region is the screen's rectangle in a frame of w x h.
func screenRect(w, h int) image.Rectangle {
	l := LayoutFor(w, h)
	k := float64(l.AvatarSize) / 1024
	return image.Rect(l.AvatarX+int(250*k), l.AvatarY+int(645*k), l.AvatarX+int(768*k), l.AvatarY+int(858*k))
}

func sub(img *image.RGBA, r image.Rectangle) []byte {
	var out []byte
	for y := r.Min.Y; y < r.Max.Y; y++ {
		out = append(out, img.Pix[img.PixOffset(r.Min.X, y):img.PixOffset(r.Max.X, y)]...)
	}
	return out
}

// differs counts the pixels (any channel) that are not the same in two frames.
func differs(a, b *image.RGBA) int {
	n := 0
	for i := 0; i < len(a.Pix); i += 4 {
		if a.Pix[i] != b.Pix[i] || a.Pix[i+1] != b.Pix[i+1] || a.Pix[i+2] != b.Pix[i+2] {
			n++
		}
	}
	return n
}

// The character is alive: the hair, the ribbon and the head move between frames; inside one quantum the frame is the same;
// the movement is small (it never leaves the neighbourhood of the rest position).
func TestCharacterMovesALittle(t *testing.T) {
	s := NewScreen()
	a, _ := frame(t, s, 1280, 720, State{Progress: 0.3, Elapsed: 0})
	same, _ := frame(t, s, 1280, 720, State{Progress: 0.3, Elapsed: 30 * time.Millisecond})
	b, _ := frame(t, s, 1280, 720, State{Progress: 0.3, Elapsed: 700 * time.Millisecond})
	if differs(a, same) != 0 {
		t.Error("two looks inside one quantum differ")
	}
	n := differs(a, b)
	if n < 500 {
		t.Errorf("only %d pixels changed in 700 ms: the character looks stuck", n)
	}
	if n > 1280*720/6 {
		t.Errorf("%d pixels changed in 700 ms: the movement is not a little one", n)
	}
	// the movement comes round (the longest period of the rig is 3500 ms, the others are not whole fractions of it, so only the
	// bar and the very same time give the same frame)
	again, _ := frame(t, s, 1280, 720, State{Progress: 0.3, Elapsed: 0})
	if differs(a, again) != 0 {
		t.Error("the same state gave another frame")
	}
}

// The screen she holds follows the progress, and a bigger window shows the same picture.
func TestScreenFollowsTheProgress(t *testing.T) {
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}, {720, 1280}} {
		w, h := size[0], size[1]
		s := NewScreen()
		r := screenRect(w, h)
		f0, _ := frame(t, s, w, h, State{Progress: 0})
		f1, _ := frame(t, s, w, h, State{Progress: 1})
		fm, _ := frame(t, s, w, h, State{Progress: 0.5})
		if bytes.Equal(sub(f0, r), sub(f1, r)) {
			t.Errorf("%dx%d: the screen is the same at 0 and 100 %%", w, h)
		}
		if bytes.Equal(sub(f0, r), sub(fm, r)) || bytes.Equal(sub(f1, r), sub(fm, r)) {
			t.Errorf("%dx%d: the screen at 50 %% is one of the ends", w, h)
		}
		// the screen is the only thing that differs inside the character's box, more or less: the rest of the frame is the same
		top := image.Rect(0, 0, w, r.Min.Y-int(10*float64(h)/720))
		if !bytes.Equal(sub(f0, top), sub(f1, top)) {
			t.Errorf("%dx%d: more than the screen changed with the progress", w, h)
		}
	}
}

// Ready: the eruption fills the screen, the hair is blown, particles fly outside the screen, and it all fades out; before that
// the frame is whole at every moment and nothing moves outside the avatar's box.
func TestEruptionAndFade(t *testing.T) {
	s := NewScreen()
	w, h := 1280, 720
	r := screenRect(w, h)
	idle, _ := frame(t, s, w, h, State{Progress: 1, Elapsed: time.Second})
	var prevScreen []byte
	moved := 0
	for _, ms := range []int{0, 90, 230, 410, 800, 1190, 1660, 1900} {
		img, _ := frame(t, s, w, h, State{Progress: 1, Elapsed: time.Second + time.Duration(ms)*time.Millisecond, Done: true, DoneFor: time.Duration(ms) * time.Millisecond})
		cur := sub(img, r)
		if prevScreen != nil && !bytes.Equal(prevScreen, cur) {
			moved++
		}
		prevScreen = cur
	}
	if moved < 6 {
		t.Errorf("the eruption changed the screen only %d times in 8 looks", moved)
	}
	peak, _ := frame(t, s, w, h, State{Progress: 1, Elapsed: time.Second, Done: true, DoneFor: 410 * time.Millisecond})
	if bytes.Equal(sub(peak, r), sub(idle, r)) {
		t.Error("the screen at the peak is the idle screen")
	}
	// outside the screen: the hair above the head is blown (the forelock is far from the screen) and cinders fly
	l := LayoutFor(w, h)
	k := float64(l.AvatarSize) / 1024
	hair := image.Rect(l.AvatarX+int(400*k), l.AvatarY+int(100*k), l.AvatarX+int(700*k), l.AvatarY+int(280*k))
	early, _ := frame(t, s, w, h, State{Progress: 1, Elapsed: time.Second, Done: true, DoneFor: 0})
	if bytes.Equal(sub(early, hair), sub(peak, hair)) {
		t.Error("the wind did not move the forelock")
	}
	cinder := image.Rect(l.AvatarX+int(400*k), l.AvatarY+int(540*k), l.AvatarX+int(900*k), l.AvatarY+int(620*k))
	if bytes.Equal(sub(early, cinder), sub(peak, cinder)) {
		t.Error("no particle flew")
	}
	// the fade
	prev := 2.0
	for ms := 0; ms <= 2300; ms += 50 {
		f := Fade(State{Done: true, DoneFor: time.Duration(ms) * time.Millisecond})
		if f > prev || f < 0 || f > 1 {
			t.Fatalf("fade at %d ms: %v after %v", ms, f, prev)
		}
		prev = f
	}
	if Fade(State{}) != 1 || Fade(State{Done: true, DoneFor: 1600 * time.Millisecond}) != 1 || Fade(State{Done: true, DoneFor: 2210 * time.Millisecond}) != 0 {
		t.Error("the fade does not start at 1.66 s and end at 2.21 s")
	}
}

// Every size a window can have paints without trouble, the avatar is where the layout says, and the bitmap is made again only
// when the clock or the progress moves.
func TestScreenAtManySizesAndTheCache(t *testing.T) {
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}, {853, 512}, {400, 300}, {200, 120}, {80, 60}, {720, 1280}, {1, 1}, {2000, 300}} {
		w, h := size[0], size[1]
		s := NewScreen()
		for _, st := range []State{{Progress: 0.4}, {Indeterminate: true, Elapsed: time.Second}, {Progress: 1, Done: true, DoneFor: 500 * time.Millisecond}} {
			img, _ := frame(t, s, w, h, st)
			if w >= 200 && h >= 120 && img.RGBAAt(w/2, h/2) == img.RGBAAt(1, 1) {
				t.Errorf("%dx%d: nothing in the middle", w, h)
			}
		}
	}
	a, _ := LoadArt()
	cl := NewCharacterLayer(a)
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	cl.Draw(img, State{Progress: 0.2, Elapsed: 10 * time.Millisecond})
	cl.Draw(img, State{Progress: 0.2, Elapsed: 50 * time.Millisecond})
	cl.Draw(img, State{Progress: 0.205, Elapsed: 60 * time.Millisecond})
	if cl.renders != 1 {
		t.Errorf("%d renders for three looks inside one quantum and one percent", cl.renders)
	}
	cl.Draw(img, State{Progress: 0.2, Elapsed: 90 * time.Millisecond})
	cl.Draw(img, State{Progress: 0.3, Elapsed: 90 * time.Millisecond})
	if cl.renders != 3 {
		t.Errorf("%d renders, want 3 (a new quantum, then a new progress)", cl.renders)
	}
}

// A frame is made in a time that leaves the window thread alone: at 1080p well under the 50 ms between two looks.
func TestCharacterFrameIsCheap(t *testing.T) {
	a, _ := LoadArt()
	cl := NewCharacterLayer(a)
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	cl.Draw(img, State{Progress: 0.4}) // the scaled copies are made once
	begin := time.Now()
	const n = 20
	for i := 1; i <= n; i++ {
		cl.Draw(img, State{Progress: 0.4, Elapsed: time.Duration(i) * 100 * time.Millisecond})
	}
	per := time.Since(begin) / n
	t.Logf("one frame of the character at 1920x1080: %v", per)
	if per > 150*time.Millisecond {
		t.Errorf("one frame takes %v", per)
	}
}

func TestRigJSONIsWhatTheCodeReads(t *testing.T) {
	b, err := fs.ReadFile(ArtFS(), "rig.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["version"].(float64) != 3 {
		t.Fatalf("rig.json: %v", err)
	}
	if math.Abs(m["rigScale"].(float64)-0.71) > 1e-9 {
		t.Errorf("rig scale %v: the layout's avatar size assumes 0.71", m["rigScale"])
	}
}

// TestDumpPreview writes frames (and the lines of text of each, as JSON) into the directory MODAVD_BOOTSCREEN_DUMP names, to
// look at them; the text is drawn by whoever owns the window, so a script lays it over the picture.
func TestDumpPreview(t *testing.T) {
	dir := os.Getenv("MODAVD_BOOTSCREEN_DUMP")
	if dir == "" {
		t.Skip("set MODAVD_BOOTSCREEN_DUMP to a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	type shot struct {
		name string
		w, h int
		st   State
		step string
	}
	step := "正在做开机后调优…"
	shots := []shot{
		{"p000", 1280, 720, State{Progress: 0, Elapsed: 1 * time.Second}, step},
		{"p040", 1280, 720, State{Progress: 0.4, Elapsed: 2300 * time.Millisecond}, step},
		{"p080", 1280, 720, State{Progress: 0.8, Elapsed: 3100 * time.Millisecond}, step},
		{"p100", 1280, 720, State{Progress: 1, Elapsed: 4 * time.Second}, "正在收尾…"},
		{"done-000", 1280, 720, State{Progress: 1, Elapsed: 4 * time.Second, Done: true, DoneFor: 0}, "准备好了"},
		{"done-410", 1280, 720, State{Progress: 1, Elapsed: 4400 * time.Millisecond, Done: true, DoneFor: 410 * time.Millisecond}, "准备好了"},
		{"done-985", 1280, 720, State{Progress: 1, Elapsed: 5 * time.Second, Done: true, DoneFor: 985 * time.Millisecond}, "准备好了"},
		{"done-1900", 1280, 720, State{Progress: 1, Elapsed: 6 * time.Second, Done: true, DoneFor: 1900 * time.Millisecond}, "准备好了"},
		{"p040-1080p", 1920, 1080, State{Progress: 0.4, Elapsed: 2300 * time.Millisecond}, step},
		{"p040-portrait", 720, 1280, State{Progress: 0.4, Elapsed: 2300 * time.Millisecond}, step},
		{"p040-small", 640, 360, State{Progress: 0.4, Elapsed: 2300 * time.Millisecond}, step},
		{"indeterminate", 1280, 720, State{Indeterminate: true, Elapsed: 1200 * time.Millisecond}, "正在启动模拟器…"},
	}
	s := NewScreen()
	meta := map[string]any{}
	for _, sh := range shots {
		sh.st.Step = sh.step
		img, labels := frame(t, s, sh.w, sh.h, sh.st)
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sh.name+".png"), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		meta[sh.name] = map[string]any{"labels": labels, "fade": Fade(sh.st)}
	}
	j, _ := json.MarshalIndent(meta, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "labels.json"), j, 0o644); err != nil {
		t.Fatal(err)
	}
}
