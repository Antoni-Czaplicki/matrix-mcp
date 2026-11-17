package main

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// thumbnailMaxDim is the longest side of images returned with variant "thumbnail".
const thumbnailMaxDim = 768

// thumbnail scales an image down so its longest side is at most maxDim and re-encodes it as JPEG.
// ok is false when the data is not a decodable image or is already small enough; callers then use the original.
func thumbnail(data []byte, maxDim int) (out []byte, width, height int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, 0, 0, false
	}
	if cfg.Width <= maxDim && cfg.Height <= maxDim && len(data) <= 256<<10 {
		return nil, cfg.Width, cfg.Height, false
	}
	// Refuse absurd dimensions before allocating a decode buffer.
	if cfg.Width*cfg.Height > 80_000_000 {
		return nil, cfg.Width, cfg.Height, false
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, false
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	resized := w > maxDim || h > maxDim
	if resized {
		if w >= h {
			h = max(1, h*maxDim/w)
			w = maxDim
		} else {
			w = max(1, w*maxDim/h)
			h = maxDim
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// White background so transparent PNGs do not turn black in JPEG.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, 0, 0, false
	}
	// Token cost follows pixel count, so a downscaled image is worth it even if the file got bigger.
	if !resized && buf.Len() >= len(data) {
		return nil, w, h, false
	}
	return buf.Bytes(), w, h, true
}
