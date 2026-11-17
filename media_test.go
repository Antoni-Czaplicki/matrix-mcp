package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func pngOf(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 7), uint8(y * 13), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestThumbnail(t *testing.T) {
	out, w, h, ok := thumbnail(pngOf(2000, 1000), 768)
	if !ok {
		t.Fatal("expected a thumbnail for a large image")
	}
	if w != 768 || h != 384 {
		t.Errorf("size = %dx%d, want 768x384", w, h)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 768 || cfg.Height != 384 {
		t.Errorf("output is not a 768x384 JPEG: %+v %v", cfg, err)
	}

	if _, w, h, ok := thumbnail(pngOf(300, 600), 768); ok || w != 300 || h != 600 {
		t.Errorf("small image should be returned as is, got ok=%v %dx%d", ok, w, h)
	}
	if _, _, _, ok := thumbnail([]byte("not an image"), 768); ok {
		t.Error("garbage should not produce a thumbnail")
	}
}

func TestThumbnailPortrait(t *testing.T) {
	if _, w, h, ok := thumbnail(pngOf(300, 900), 768); !ok || w != 256 || h != 768 {
		t.Errorf("portrait: ok=%v %dx%d, want 256x768", ok, w, h)
	}
}
