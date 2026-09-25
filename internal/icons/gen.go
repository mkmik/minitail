//go:build ignore

// Command gen draws minitail's menu bar icons: a small tail.
//
// They are macOS template images: pure black with an alpha channel, which lets
// AppKit recolour them for the light and dark menu bar automatically. Run with
// `go generate ./internal/icons` on the Go release go.mod names (for example
// GOTOOLCHAIN=go1.24.6): CI regenerates them and compares the bytes, and
// other releases compress PNGs differently.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

// size is 32px because systray shows the icon at 16pt, so this is exactly
// one pixel per pixel on a Retina menu bar (16pt @2x).
const size = 32

// dim is the opacity of a tail, or the part of one, that is not working.
const dim = 0.35

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

// point is a point on the tail's centre line, with t its distance along the
// tail: 0 at the base, 1 at the tip.
type point struct{ x, y, t float64 }

// spine is the tail's centre line: two cubic Bézier curves that rise from the
// bottom edge in an S and end in a flick to the left.
var spine = func() []point {
	curves := [][4][2]float64{
		{{11, 32}, {4, 25}, {5, 17}, {13, 14}},
		{{13, 14}, {21, 11}, {25, 5}, {19, 2}},
	}
	const steps = 1000
	var pts []point
	for i, p := range curves {
		for j := 0; j <= steps; j++ {
			s := float64(j) / steps
			a, b, c, d := (1-s)*(1-s)*(1-s), 3*(1-s)*(1-s)*s, 3*(1-s)*s*s, s*s*s
			pts = append(pts, point{
				x: a*p[0][0] + b*p[1][0] + c*p[2][0] + d*p[3][0],
				y: a*p[0][1] + b*p[1][1] + c*p[2][1] + d*p[3][1],
				t: (float64(i) + s) / float64(len(curves)),
			})
		}
	}
	return pts
}()

// tail draws a stroke along spine that is radius(t) wide and alpha(t) opaque
// at each point.
func (c *canvas) tail(radius, alpha func(t float64) float64) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			// The stroke is a union of discs, so the distance to its edge is
			// the smallest distance to the edge of any one of them.
			edge, at := math.Inf(1), 0.0
			for _, p := range spine {
				if d := math.Hypot(fx-p.x, fy-p.y) - radius(p.t); d < edge {
					edge, at = d, p.t
				}
			}
			// Coverage falls off over one pixel at the edge.
			c.set(x, y, alpha(at)*math.Min(0.5-edge, 1))
		}
	}
}

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
	// The tail tapers from 3.6px at the base to 1px at the tip.
	body := func(t float64) float64 { return 1 + 2.6*math.Pow(1-t, 0.85) }
	line := func(float64) float64 { return 0.8 }
	solid := func(float64) float64 { return 1 }
	faint := func(float64) float64 { return dim }

	// stopped: a faint tail.
	c := newCanvas()
	c.tail(body, faint)
	c.save("stopped.png")

	// starting: a faint tail with a line drawn down its middle.
	c = newCanvas()
	c.tail(body, faint)
	c.tail(line, solid)
	c.save("starting.png")

	// attention: a faint tail beside an exclamation mark, for login and errors.
	c = newCanvas()
	c.tail(body, faint)
	c.bar(23.6, 20, 26.4, 26)
	c.bar(23.6, 27.6, 26.4, 30.4)
	c.save("attention.png")

	// partial: a tail solid only from the base to halfway, for
	// advertised-but-not-approved.
	c = newCanvas()
	c.tail(body, func(t float64) float64 {
		if t < 0.5 {
			return 1
		}
		return dim
	})
	c.save("partial.png")

	// active: a solid tail.
	c = newCanvas()
	c.tail(body, solid)
	c.save("active.png")
}
