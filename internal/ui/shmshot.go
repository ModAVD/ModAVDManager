package ui

import (
	"bytes"
	"errors"
	"image"
	"image/png"
)

// bgraPNG encodes a packed top-down BGRA frame (stride bytes per row) as PNG. The emulator's alpha is not meaningful
// for a screen, so the picture is written opaque.
func bgraPNG(pix []byte, width, height, stride int) ([]byte, error) {
	if width <= 0 || height <= 0 || stride < width*4 || len(pix) < stride*height {
		return nil, errors.New("shared frame has a bad size")
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		src := pix[y*stride : y*stride+width*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for x := 0; x < len(src); x += 4 {
			dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 0xff
		}
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
