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
	"fmt"
	"image"
	"image/png"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// ToWebP re-encodes a PNG as lossless WebP, and returns that when it is smaller
// and decodes to the same picture. Screenshots, the usual image, come out about
// a third smaller, and every image in a session's context is resent every turn.
//
// Only PNG is converted. A JPEG is already lossy, so a lossless WebP of it is
// larger, and any other format passes through untouched. So does a PNG whose
// WebP would not be smaller, or would not decode to the same picture (see
// samePixels), so the conversion never changes what the model sees. On a decode
// or encode failure the original comes back with the error.
func ToWebP(data []byte, mime string) (out []byte, outMime string, converted bool, err error) {
	if mime != "image/png" {
		return data, mime, false, nil
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime, false, fmt.Errorf("decode png: %w", err)
	}
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, src, nil); err != nil {
		return data, mime, false, fmt.Errorf("encode webp: %w", err)
	}
	if buf.Len() >= len(data) {
		return data, mime, false, nil
	}
	back, err := webp.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return data, mime, false, fmt.Errorf("decode the encoded webp: %w", err)
	}
	if !samePixels(src, back) {
		return data, mime, false, nil
	}
	return buf.Bytes(), "image/webp", true, nil
}

// ToPNG re-encodes a WebP as PNG, for a server that does not accept WebP. A
// WebP from ToWebP comes back as exactly the picture it was made from.
func ToPNG(data []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode webp: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// samePixels reports whether a and b are the same picture: the same size, and
// the same 8-bit premultiplied colour at every pixel, which is what is shown.
// (A fully transparent pixel may differ in its invisible colour.)
func samePixels(a, b image.Image) bool {
	if a.Bounds().Size() != b.Bounds().Size() {
		return false
	}
	return bytes.Equal(rgbaPix(a), rgbaPix(b))
}

func rgbaPix(img image.Image) []byte {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst.Pix
}
