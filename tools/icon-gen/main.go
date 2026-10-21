package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

//go:embed source.png
var sourcePNG []byte

func main() {
	out := flag.String("out", ".", "output directory")
	flag.Parse()
	src, err := decodeSource()
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		panic(err)
	}
	for _, size := range []int{64, 256} {
		path := filepath.Join(*out, "icon_"+strconv.Itoa(size)+".png")
		if err := writePNG(path, resizeNRGBA(src, size, size)); err != nil {
			panic(err)
		}
	}
}

func decodeSource() (*image.NRGBA, error) {
	img, err := png.Decode(bytes.NewReader(sourcePNG))
	if err != nil {
		return nil, fmt.Errorf("decode source icon: %w", err)
	}
	return toNRGBA(img), nil
}

func toNRGBA(src image.Image) *image.NRGBA {
	if nrgba, ok := src.(*image.NRGBA); ok && nrgba.Rect.Min == image.Pt(0, 0) {
		return nrgba
	}
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func resizeNRGBA(src *image.NRGBA, width, height int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw == width && sh == height {
		copy(dst.Pix, src.Pix)
		return dst
	}
	for y := 0; y < height; y++ {
		sy := (float64(y)+0.5)*float64(sh)/float64(height) - 0.5
		for x := 0; x < width; x++ {
			sx := (float64(x)+0.5)*float64(sw)/float64(width) - 0.5
			dst.SetNRGBA(x, y, sampleBilinear(src, sx, sy))
		}
	}
	return dst
}

type premulRGBA struct{ r, g, b, a float64 }

func sampleBilinear(src *image.NRGBA, x, y float64) color.NRGBA {
	width, height := src.Bounds().Dx(), src.Bounds().Dy()
	x0 := math.Floor(x)
	y0 := math.Floor(y)
	ix0 := clampInt(int(x0), 0, width-1)
	iy0 := clampInt(int(y0), 0, height-1)
	ix1 := clampInt(int(x0)+1, 0, width-1)
	iy1 := clampInt(int(y0)+1, 0, height-1)
	fx := clampUnit(x - x0)
	fy := clampUnit(y - y0)
	top := lerp(premul(src.NRGBAAt(ix0, iy0)), premul(src.NRGBAAt(ix1, iy0)), fx)
	bottom := lerp(premul(src.NRGBAAt(ix0, iy1)), premul(src.NRGBAAt(ix1, iy1)), fx)
	return unpremul(lerp(top, bottom, fy))
}

func premul(c color.NRGBA) premulRGBA {
	a := float64(c.A) / 255
	return premulRGBA{
		r: float64(c.R) / 255 * a,
		g: float64(c.G) / 255 * a,
		b: float64(c.B) / 255 * a,
		a: a,
	}
}

func lerp(a, b premulRGBA, t float64) premulRGBA {
	return premulRGBA{
		r: a.r + (b.r-a.r)*t,
		g: a.g + (b.g-a.g)*t,
		b: a.b + (b.b-a.b)*t,
		a: a.a + (b.a-a.a)*t,
	}
}

func unpremul(c premulRGBA) color.NRGBA {
	if c.a <= 0 {
		return color.NRGBA{}
	}
	inv := 1 / c.a
	return color.NRGBA{
		R: clampU8(c.r * inv * 255),
		G: clampU8(c.g * inv * 255),
		B: clampU8(c.b * inv * 255),
		A: clampU8(c.a * 255),
	}
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func clampU8(value float64) uint8 {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return uint8(math.Round(value))
}
