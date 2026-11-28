package bootscreen

import (
	"bytes"
	"image"
	"image/png"
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
func TestAvatarIsEmbeddedAndTransparent(t *testing.T) {
	a := Avatar()
	if a == nil {
		t.Fatal("the avatar does not decode")
	}
	if b := a.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Errorf("avatar %v, want 512x512", b)
	}
	if _, _, _, alpha := a.At(2, 2).RGBA(); alpha != 0 {
		t.Errorf("the corner of the avatar is not transparent (alpha %d)", alpha)
	}
	// the page serves the same bytes from its own assets
	web, err := os.ReadFile(filepath.Join("..", "ui", "web", "assets", "boot-avatar.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(web, AvatarPNG()) {
		t.Error("ui/web/assets/boot-avatar.png differs from the avatar the overlay embeds")
	}
	if n := len(AvatarPNG()); n > 1<<20 {
		t.Errorf("avatar is %d bytes; the art in the package stays under 1 MiB", n)
	}
}

// The layout of the artwork: the bar is under the character, about 60 % of its width, 7 px high at 1280 x 720; the
// percentage is at the right of the bar and the step under it; the colours are the artwork's. Whatever the window's
// shape, nothing is cut off, and the bar and the lines are inside the frame.
func TestPlaceholderLayoutAndLines(t *testing.T) {
	s := NewPlaceholder()
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
func TestPlaceholderIndeterminate(t *testing.T) {
	s := NewPlaceholder()
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
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Error("the movement does not come round after its period")
	}
}

// The screen is alive while the progress stands still: the light runs along the fill between frames.
func TestPlaceholderIsAliveAtConstantProgress(t *testing.T) {
	s := NewPlaceholder()
	a, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 0})
	b, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 600 * time.Millisecond})
	c, _ := frame(t, s, 1280, 720, State{Progress: 0.5, Elapsed: 1800 * time.Millisecond})
	if bytes.Equal(a.Pix, b.Pix) {
		t.Error("two frames 600 ms apart are the same: the screen looks stuck")
	}
	if !bytes.Equal(a.Pix, c.Pix) {
		t.Error("the light does not come round again after its period")
	}
	z1, _ := frame(t, s, 1280, 720, State{Progress: 0, Elapsed: 0})
	z2, _ := frame(t, s, 1280, 720, State{Progress: 0, Elapsed: 700 * time.Millisecond})
	if !bytes.Equal(z1.Pix, z2.Pix) {
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

// TestDumpPreview writes the placeholder at 40% as a PNG when MODAVD_BOOTSCREEN_DUMP names a file (to look at it; no
// text, which is drawn by whoever owns the window).
func TestDumpPreview(t *testing.T) {
	path := os.Getenv("MODAVD_BOOTSCREEN_DUMP")
	if path == "" {
		t.Skip("set MODAVD_BOOTSCREEN_DUMP to a file name")
	}
	img, _ := frame(t, NewPlaceholder(), 1280, 720, State{Progress: 0.4, Elapsed: 500 * time.Millisecond})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
