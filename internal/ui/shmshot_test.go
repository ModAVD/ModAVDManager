package ui

import (
	"bytes"
	"image/png"
	"testing"
)

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
