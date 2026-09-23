// SPDX-License-Identifier: GPL-3.0-or-later

package feed

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sync"
)

//go:embed sw.js
var serviceWorker []byte

//go:embed manifest.webmanifest
var manifest []byte

// AppFiles serves the service worker, the web app manifest and the icon.
// Browsers fetch the manifest and its icon without cookies, and none of
// the three holds anything private, so they are served without login.
func AppFiles() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(serviceWorker)
	})
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(manifest)
	})
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(icon())
	})
	return mux
}

// AppPaths are the paths AppFiles serves.
var AppPaths = []string{"/sw.js", "/manifest.webmanifest", "/icon.png"}

// icon draws a terminal prompt, ">_", in white on the accent color.
var icon = sync.OnceValue(func() []byte {
	const size = 512
	const halfWidth = 28.0
	type segment struct{ ax, ay, bx, by float64 }
	strokes := []segment{
		{150, 160, 262, 256},
		{262, 256, 150, 352},
		{292, 352, 382, 352},
	}
	background := color.NRGBA{0xb4, 0x55, 0x2d, 0xff}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			px, py := float64(x)+0.5, float64(y)+0.5
			distance := math.Inf(1)
			for _, s := range strokes {
				distance = min(distance, distanceToSegment(px, py, s.ax, s.ay, s.bx, s.by))
			}
			coverage := max(0, min(1, halfWidth+0.5-distance))
			img.SetNRGBA(x, y, color.NRGBA{
				R: blend(background.R, coverage),
				G: blend(background.G, coverage),
				B: blend(background.B, coverage),
				A: 0xff,
			})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
})

func blend(channel uint8, white float64) uint8 {
	return uint8(math.Round(float64(channel) + (255-float64(channel))*white))
}

func distanceToSegment(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := max(0, min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
