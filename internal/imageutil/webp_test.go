// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"testing"

	"golang.org/x/image/webp"
)

// photoLike builds a w×h image of smooth gradients with a little noise: the
// kind of content lossless WebP's predictors compress better than PNG does,
// as they do screenshots, at a size small enough for -race. alpha, when not
// 255, makes every pixel that translucent, with some variation.
func photoLike(w, h int, alpha uint8) *image.NRGBA {
	noise := make([]byte, w*h)
	rand.New(rand.NewSource(1)).Read(noise) //nolint:gosec // test determinism
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			n := noise[y*w+x] & 3
			a := alpha
			if a != 255 {
				a -= noise[y*w+x] % (a / 2)
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(x) + n, uint8(y) + n, uint8(x+y)/2 + n, a})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func decodeAny(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func TestToWebP_ConvertsAPNGLosslessly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alpha uint8
	}{{"opaque", 255}, {"translucent", 160}} {
		t.Run(tc.name, func(t *testing.T) {
			src := photoLike(200, 150, tc.alpha)
			in := encodePNG(t, src)
			out, mime, converted, err := ToWebP(in, "image/png")
			if err != nil {
				t.Fatalf("ToWebP: %v", err)
			}
			if !converted || mime != "image/webp" {
				t.Fatalf("converted=%v mime=%q, want true, image/webp", converted, mime)
			}
			if len(out) >= len(in) {
				t.Errorf("webp is %d bytes, not smaller than the png's %d", len(out), len(in))
			}
			back, err := webp.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("decode webp: %v", err)
			}
			if !samePixels(src, back) {
				t.Error("the webp does not decode to the png's pixels")
			}

			// And back to PNG, for a server that takes no WebP.
			again, err := ToPNG(out)
			if err != nil {
				t.Fatalf("ToPNG: %v", err)
			}
			if !samePixels(src, decodeAny(t, again)) {
				t.Error("ToPNG of the webp does not decode to the original pixels")
			}
		})
	}
}

func TestToWebP_KeepsAPNGWhoseWebPIsNotSmaller(t *testing.T) {
	// A plain gradient is what PNG's filters are best at: its WebP is larger.
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := range 60 {
		for x := range 80 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	in := encodePNG(t, img)
	out, mime, converted, err := ToWebP(in, "image/png")
	if err != nil {
		t.Fatalf("ToWebP: %v", err)
	}
	if converted || mime != "image/png" || !bytes.Equal(out, in) {
		t.Errorf("converted=%v mime=%q same bytes=%v; want the png back untouched", converted, mime, bytes.Equal(out, in))
	}
}

func TestToWebP_LeavesOtherFormatsAlone(t *testing.T) {
	jpg := makeImage(t, 200, 150, "jpeg")
	for _, mime := range []string{"image/jpeg", "image/gif", "image/webp", "application/pdf"} {
		out, outMime, converted, err := ToWebP(jpg, mime)
		if err != nil || converted || outMime != mime || !bytes.Equal(out, jpg) {
			t.Errorf("%s: converted=%v mime=%q err=%v; want it back untouched", mime, converted, outMime, err)
		}
	}
}

func TestToWebP_KeepsTheOriginalWhenItCannotDecode(t *testing.T) {
	in := []byte("not a png")
	out, mime, converted, err := ToWebP(in, "image/png")
	if err == nil {
		t.Fatal("want an error for bytes that are not a PNG")
	}
	if converted || mime != "image/png" || !bytes.Equal(out, in) {
		t.Errorf("converted=%v mime=%q; want the original back", converted, mime)
	}
}

func TestToPNG_RejectsWhatIsNotWebP(t *testing.T) {
	if _, err := ToPNG(makeImage(t, 8, 8, "png")); err == nil {
		t.Error("want an error for a PNG")
	}
}

func TestSamePixels(t *testing.T) {
	a := photoLike(20, 10, 200)
	b := image.NewNRGBA(a.Bounds())
	copy(b.Pix, a.Pix)
	if !samePixels(a, b) {
		t.Fatal("a copy is not the same picture")
	}

	// A fully transparent pixel shows nothing, whatever its colour.
	a.SetNRGBA(3, 3, color.NRGBA{1, 2, 3, 0})
	b.SetNRGBA(3, 3, color.NRGBA{200, 100, 50, 0})
	if !samePixels(a, b) {
		t.Error("pixels differing only in an invisible colour count as different")
	}

	b.SetNRGBA(5, 5, color.NRGBA{0, 0, 0, 255})
	if samePixels(a, b) {
		t.Error("a changed pixel goes unnoticed")
	}
	if samePixels(a, photoLike(20, 11, 200)) {
		t.Error("images of different sizes count as the same")
	}
}
