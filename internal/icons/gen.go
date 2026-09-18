//go:build ignore

// Command gen draws minitail's menu bar icons.
//
// They are macOS template images: pure black with an alpha channel, which lets
// AppKit recolour them for the light and dark menu bar automatically. Run with
// `go generate ./internal/icons`.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

// size is 44px so the icon stays crisp on a Retina menu bar (22pt @2x).
const size = 44

type canvas struct{ img *image.NRGBA }

func newCanvas() *canvas {
	return &canvas{img: image.NewNRGBA(image.Rect(0, 0, size, size))}
}

// set blends black at the given coverage (0..1) into the pixel.
func (c *canvas) set(x, y int, cov float64) {
	if cov <= 0 || x < 0 || y < 0 || x >= size || y >= size {
		return
	}
	if cov > 1 {
		cov = 1
	}
	prev := c.img.NRGBAAt(x, y)
	a := uint8(math.Round(cov * 255))
	if a <= prev.A {
		return
	}
	c.img.SetNRGBA(x, y, color.NRGBA{A: a})
}

// ring draws an antialiased annulus centred on the canvas.
func (c *canvas) ring(rOuter, rInner float64) {
	cx, cy := float64(size)/2, float64(size)/2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			// Coverage falls off over one pixel at each edge.
			cov := math.Min(rOuter-d, d-rInner)
			c.set(x, y, cov+0.5)
		}
	}
}

// disc draws a filled antialiased circle.
func (c *canvas) disc(r float64) { c.ring(r, -1) }

// bar draws a filled rounded-ish rectangle in canvas coordinates.
func (c *canvas) bar(x0, y0, x1, y1 float64) {
	for y := int(y0) - 1; y <= int(y1)+1; y++ {
		for x := int(x0) - 1; x <= int(x1)+1; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			cov := math.Min(math.Min(fx-x0, x1-fx), math.Min(fy-y0, y1-fy))
			c.set(x, y, cov+0.5)
		}
	}
}

// wedge fills the left half of a disc, for the "partly there" states.
func (c *canvas) leftHalfDisc(r float64) {
	cx, cy := float64(size)/2, float64(size)/2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if fx > cx {
				continue
			}
			c.set(x, y, r-math.Hypot(fx-cx, fy-cy)+0.5)
		}
	}
}

func (c *canvas) save(name string) {
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, c.img); err != nil {
		log.Fatal(err)
	}
}

func main() {
	const rOuter, rInner = 17, 12

	// stopped: an empty ring.
	c := newCanvas()
	c.ring(rOuter, rInner)
	c.save("stopped.png")

	// starting: a ring with a small centre dot.
	c = newCanvas()
	c.ring(rOuter, rInner)
	c.disc(4)
	c.save("starting.png")

	// attention: a ring around an exclamation mark, for login and errors.
	c = newCanvas()
	c.ring(rOuter, rInner)
	c.bar(20, 12, 24, 25)
	c.bar(20, 28, 24, 32)
	c.save("attention.png")

	// partial: a half-filled ring, for advertised-but-not-approved.
	c = newCanvas()
	c.ring(rOuter, rInner)
	c.leftHalfDisc(rInner - 2)
	c.save("partial.png")

	// active: a solid disc.
	c = newCanvas()
	c.disc(rOuter)
	c.save("active.png")
}
