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

// Package imageutil provides safeguards for image attachments before they are
// persisted and later sent to an LLM: aspect-preserving downscaling of images
// whose pixel dimensions (Clamp) or encoded size (Shrink) exceed a provider's
// limit.
//
// Providers reject images on both counts (e.g. Vertex/Claude 400s on an image
// whose long edge is too large, and on one over 10 MiB of base64). A tall-but-
// small-bytes screenshot passes a byte cap and fails the dimension limit, while a
// photo-like PNG within the dimension limit can still be too many bytes, so both
// are applied at the single point every attachment funnels through.
package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"

	// Register GIF as a decodable format for image.Decode (decode-only; a
	// resized GIF is re-encoded to PNG).
	_ "image/gif"

	"golang.org/x/image/draw"

	// Register WebP as a decodable format for image.Decode. Decode-only; we
	// re-encode a resized WebP to PNG (x/image has no WebP encoder).
	_ "golang.org/x/image/webp"
)

// DefaultMaxDim is the default long-edge clamp in pixels. 2000px sits safely
// under every provider's hard rejection threshold while leaving headroom above
// Anthropic's ~1568px internal downscale target.
const DefaultMaxDim = 2000

// DefaultMaxBytes is the default cap on an image's encoded size: 3.75 MB is 5 MB
// once base64-encoded, the strictest per-image limit among the providers (the
// Anthropic API documents 5 MB; Vertex allows 10 MiB).
const DefaultMaxBytes = 3_750_000

// jpegQuality is the re-encode quality for JPEG output. 90 is visually
// near-lossless while keeping the byte size well below the base64 caps.
const jpegQuality = 90

// Clamp downscales an image so neither dimension exceeds maxDim, preserving
// aspect ratio. It returns the (possibly new) bytes, the output MIME type, and
// whether a resize happened.
//
// Behavior:
//   - If the image already fits (both dimensions <= maxDim), the ORIGINAL bytes
//     and MIME are returned untouched (resized=false), so no re-encode artifacts
//     are introduced on images that don't need it.
//   - JPEG stays JPEG (q90); every other decodable format (PNG, GIF, WebP) is
//     re-encoded to PNG, since x/image ships no encoders for GIF-animation or
//     WebP and PNG is universally accepted and lossless.
//   - On any decode/encode failure the original bytes+MIME are returned with a
//     non-nil error; callers should keep the original and log, never drop the
//     image or fail the turn.
//
// maxDim <= 0 selects DefaultMaxDim.
func Clamp(data []byte, mime string, maxDim int) (out []byte, outMime string, resized bool, err error) {
	if maxDim <= 0 {
		maxDim = DefaultMaxDim
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, mime, false, fmt.Errorf("decode image config: %w", err)
	}
	// Already within bounds: return the original bytes untouched.
	if cfg.Width <= maxDim && cfg.Height <= maxDim {
		return data, mime, false, nil
	}

	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime, false, fmt.Errorf("decode image: %w", err)
	}
	nw, nh := scaledDims(cfg.Width, cfg.Height, maxDim)
	out, outMime, err = resize(src, format, nw, nh)
	if err != nil {
		return data, mime, false, err
	}
	return out, outMime, true, nil
}

// Shrink downscales an image whose encoding is larger than maxBytes until it
// fits, preserving aspect ratio, and otherwise behaves like Clamp: an image that
// already fits comes back untouched, JPEG stays JPEG and the rest become PNG, and
// on failure (including an image that cannot be shrunk enough) the original is
// returned with a non-nil error.
//
// maxBytes <= 0 selects DefaultMaxBytes.
func Shrink(data []byte, mime string, maxBytes int) (out []byte, outMime string, resized bool, err error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if len(data) <= maxBytes {
		return data, mime, false, nil
	}
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime, false, fmt.Errorf("decode image: %w", err)
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	size := len(data)
	// Encoded size grows roughly with pixel count, so scale both edges by the
	// square root of the overshoot, with a margin; repeat if it still misses.
	for range 5 {
		scale := math.Sqrt(float64(maxBytes)/float64(size)) * 0.9
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
		out, outMime, err = resize(src, format, w, h)
		if err != nil {
			return data, mime, false, err
		}
		if len(out) <= maxBytes {
			return out, outMime, true, nil
		}
		size = len(out)
	}
	return data, mime, false, fmt.Errorf("still %d bytes at %dx%d, over %d", size, w, h, maxBytes)
}

// resize resamples src to w×h and encodes it in Clamp's output format for the
// source format.
func resize(src image.Image, format string, w, h int) ([]byte, string, error) {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom is a high-quality resampling kernel — good for downscaling
	// screenshots/diagrams where sharp text should stay legible.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&buf, dst); err != nil {
		return nil, "", fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), "image/png", nil
}

// scaledDims returns the largest width/height that fit within a maxDim square
// while preserving the source aspect ratio. Each result is at least 1px.
func scaledDims(w, h, maxDim int) (int, int) {
	if w <= 0 || h <= 0 {
		return 1, 1
	}
	if w >= h {
		nh := h * maxDim / w
		if nh < 1 {
			nh = 1
		}
		return maxDim, nh
	}
	nw := w * maxDim / h
	if nw < 1 {
		nw = 1
	}
	return nw, maxDim
}
