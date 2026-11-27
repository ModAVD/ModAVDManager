package ui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestTurnPortrait(t *testing.T) {
	enc := func(w, h int, set map[[2]int]color.RGBA) []byte {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for p, c := range set {
			img.SetRGBA(p[0], p[1], c)
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	red, green := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}
	// 3x2 canvas: red at the top-left corner, green at the top-right one
	data := enc(3, 2, map[[2]int]color.RGBA{{0, 0}: red, {2, 0}: green})
	out, err := turnPortrait(data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 3 {
		t.Fatalf("bounds %v", b)
	}
	// clockwise: top-left goes to top-right, top-right to bottom-right
	at := func(x, y int) color.RGBA { return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA) }
	if at(1, 0) != red || at(1, 2) != green {
		t.Errorf("red at (1,0) = %v, green at (1,2) = %v", at(1, 0), at(1, 2))
	}
	// already upright: untouched
	tall := enc(2, 3, nil)
	if got, err := turnPortrait(tall); err != nil || !bytes.Equal(got, tall) {
		t.Errorf("portrait frame changed: %v", err)
	}
	if _, err := turnPortrait([]byte("garbage")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestBGRAPNG(t *testing.T) {
	// 2x2, stride 12 (4 bytes of padding per row): the padding must not leak into the picture
	pix := []byte{
		10, 20, 30, 0, 40, 50, 60, 0, 9, 9, 9, 9,
		70, 80, 90, 7, 100, 110, 120, 7, 9, 9, 9, 9,
	}
	data, err := bgraPNG(pix, 2, 2, 12)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 2 {
		t.Fatalf("bounds %v", b)
	}
	for _, c := range []struct {
		x, y    int
		r, g, b uint32
	}{{0, 0, 30, 20, 10}, {1, 0, 60, 50, 40}, {0, 1, 90, 80, 70}, {1, 1, 120, 110, 100}} {
		r, g, b, a := img.At(c.x, c.y).RGBA()
		if r>>8 != c.r || g>>8 != c.g || b>>8 != c.b || a>>8 != 0xff {
			t.Errorf("(%d,%d) = %d %d %d %d", c.x, c.y, r>>8, g>>8, b>>8, a>>8)
		}
	}
	for _, bad := range [][]int{{0, 2, 8}, {2, 2, 7}} {
		if _, err := bgraPNG(pix, bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := bgraPNG(pix[:20], 2, 2, 12); err == nil {
		t.Error("short buffer accepted")
	}
}
