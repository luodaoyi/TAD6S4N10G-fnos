package main

import (
	"image"
	"testing"
)

func TestDecodeSource(t *testing.T) {
	src, err := decodeSource()
	if err != nil {
		t.Fatal(err)
	}
	if src.Bounds() != image.Rect(0, 0, 128, 128) {
		t.Fatalf("source bounds = %v, want 128x128", src.Bounds())
	}
	if countOpaque(src) == 0 {
		t.Fatal("source icon is fully transparent")
	}
}

func TestResizeIcons(t *testing.T) {
	src, err := decodeSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{64, 256} {
		img := resizeNRGBA(src, size, size)
		if img.Bounds() != image.Rect(0, 0, size, size) {
			t.Fatalf("size %d bounds = %v", size, img.Bounds())
		}
		if countOpaque(img) == 0 {
			t.Fatalf("size %d icon is fully transparent", size)
		}
	}
}

func countOpaque(img *image.NRGBA) int {
	opaque := 0
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if img.NRGBAAt(x, y).A > 0 {
				opaque++
			}
		}
	}
	return opaque
}
