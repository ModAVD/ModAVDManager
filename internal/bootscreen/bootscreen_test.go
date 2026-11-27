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

// filled counts the pixels of the bar's row that have the fill colour or the light on it (anything but the track).
func filled(img *image.RGBA, w, h int) int {
	x, y, bw, bh := BarGeometry(w, h)
	n := 0
	for dx := 0; dx < bw; dx++ {
		c := img.RGBAAt(x+dx, y+bh/2)
		if c != rgba(BarTrack) {
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

// The bar follows Progress, the avatar is in the middle, the brand brown is the background, and the lines of text are
// inside the frame, above, under and at the bottom of the bar.
func TestPlaceholderFrame(t *testing.T) {
	s := NewPlaceholder()
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}, {853, 512}, {400, 300}} {
		w, h := size[0], size[1]
		img, labels := frame(t, s, w, h, State{Progress: 0.4, Title: "正在开机", Detail: "40%", Note: "n"})
		if img.RGBAAt(2, 2) != rgba(Background) || img.RGBAAt(w-3, h-3) != rgba(Background) {
			t.Errorf("%dx%d: corners are not the background", w, h)
		}
		if c := img.RGBAAt(w/2, int(float64(h)*0.38)); c == rgba(Background) {
			t.Errorf("%dx%d: nothing of the avatar in the middle", w, h)
		}
		_, _, bw, _ := BarGeometry(w, h)
		if n := filled(img, w, h); n < bw*38/100 || n > bw*42/100 {
			t.Errorf("%dx%d: %d of %d bar pixels filled at 40%%", w, h, n, bw)
		}
		if len(labels) != 3 {
			t.Fatalf("%dx%d: %d lines, want title, detail and note", w, h, len(labels))
		}
		_, by, _, bh := BarGeometry(w, h)
		if !(labels[0].Y+labels[0].H <= by && labels[1].Y >= by+bh && labels[2].Y > labels[1].Y) {
			t.Errorf("%dx%d: lines are not above, under and below the bar: %+v (bar at %d)", w, h, labels, by)
		}
		for _, l := range labels {
			if l.X < 0 || l.Y < 0 || l.X+l.W > w || l.Y+l.H > h || l.Px < 10 {
				t.Errorf("%dx%d: line %+v is outside the frame or too small", w, h, l)
			}
		}
	}
	// no text, no lines
	if _, labels := frame(t, s, 640, 360, State{}); len(labels) != 0 {
		t.Errorf("lines without text: %+v", labels)
	}
}

// The screen is alive while the progress stands still: the light moves along the bar between frames; with no progress
// there is nothing to light and nothing moves.
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
