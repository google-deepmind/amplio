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

package eventloop

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"slices"
	"testing"

	"amplio/internal/blob"
	"amplio/internal/event"
	"amplio/internal/imageutil"
)

// pngBytes builds a w×h PNG for attachment-clamp tests.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestAttachmentBlobRoundTrip(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	data := []byte("\x89PNG\r\n\x1a\nfake image bytes")

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: data, SourceHint: "x.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false")
	}
	if !blob.ValidKey(ref.BlobKey) || ref.Size != len(data) || ref.MimeType != "image/png" {
		t.Fatalf("bad ref: %+v", ref)
	}
	if len(ref.Data) != 0 {
		t.Error("persistable ref should not carry raw Data")
	}

	atts := a.loadAttachments([]event.Attachment{ref})
	if len(atts) != 1 {
		t.Fatalf("loadAttachments len = %d", len(atts))
	}
	if atts[0].MimeType != "image/png" {
		t.Errorf("mime: %q", atts[0].MimeType)
	}
	if want := base64.StdEncoding.EncodeToString(data); atts[0].Base64Data != want {
		t.Error("round-tripped bytes differ from original")
	}
}

func TestAttachmentNilStoreDegrades(t *testing.T) {
	a := newT(testCfg{}) // no BlobStore configured
	if _, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: []byte("x")}); ok {
		t.Error("expected storeAttachment to drop when no blob store")
	}
	if got := a.loadAttachments([]event.Attachment{{BlobKey: "deadbeef", MimeType: "image/png"}}); got != nil {
		t.Errorf("expected nil attachments with no store, got %v", got)
	}
}

// An oversized image is downscaled before it's persisted, so the stored blob
// (and the ref) reflect the clamped dimensions — not the original.
func TestStoreAttachmentClampsOversized(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	// A sliver: storeAttachment's cap is imageutil.DefaultMaxDim (2000), so the
	// input must really exceed 2000px, but only on one edge. 4000×16 is 250x
	// cheaper to generate and resample than 4000×1000 and clamps the same way.
	orig := pngBytes(t, 4000, 16) // long edge 4000 > 2000 cap

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: orig, SourceHint: "big.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false")
	}
	if ref.Size >= len(orig) {
		t.Errorf("stored size %d not smaller than original %d; expected downscale", ref.Size, len(orig))
	}
	// Read the stored bytes back and confirm the pixel dimensions were clamped.
	atts := a.loadAttachments([]event.Attachment{ref})
	if len(atts) != 1 {
		t.Fatalf("loadAttachments len = %d", len(atts))
	}
	raw, err := base64.StdEncoding.DecodeString(atts[0].Base64Data)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode stored image: %v", err)
	}
	if cfg.Width != 2000 || cfg.Height != 8 {
		t.Errorf("stored dims = %dx%d, want 2000x8 (clamped, aspect preserved)", cfg.Width, cfg.Height)
	}
}

// An in-bounds image is stored byte-for-byte (no re-encode).
func TestStoreAttachmentKeepsInBounds(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	orig := pngBytes(t, 80, 60)

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: orig, SourceHint: "ok.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false")
	}
	if ref.Size != len(orig) {
		t.Errorf("stored size %d != original %d; in-bounds image should not be re-encoded", ref.Size, len(orig))
	}
	atts := a.loadAttachments([]event.Attachment{ref})
	if want := base64.StdEncoding.EncodeToString(orig); atts[0].Base64Data != want {
		t.Error("in-bounds image bytes changed; want original preserved")
	}
}

// A non-decodable "image" (or non-image mime) is stored unchanged rather than
// dropped — a clamp failure must never lose the attachment.
func TestStoreAttachmentUndecodableFallsBack(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	data := []byte("\x89PNG\r\n\x1a\nnot a real png")

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: data, SourceHint: "x.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false; undecodable image must fall back, not drop")
	}
	if ref.Size != len(data) || ref.MimeType != "image/png" {
		t.Errorf("fallback ref = %+v, want original size %d / image/png", ref, len(data))
	}
	atts := a.loadAttachments([]event.Attachment{ref})
	if want := base64.StdEncoding.EncodeToString(data); atts[0].Base64Data != want {
		t.Error("undecodable image bytes changed; want original preserved")
	}
}

// paddedPNG is a w×h PNG carrying pad bytes of private ancillary chunk, the
// kind of bulk (embedded metadata, say) decoders skip: an image over the byte cap
// that is cheap to decode and resample.
func paddedPNG(t *testing.T, w, h, pad int) []byte {
	t.Helper()
	img := pngBytes(t, w, h)
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(pad))
	chunk.WriteString("prVt")
	chunk.Write(make([]byte, pad))
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(chunk.Bytes()[4:]))
	iend := len(img) - 12 // the IEND chunk closes every PNG
	return slices.Concat(img[:iend], chunk.Bytes(), img[iend:])
}

// An image within the dimension cap but over the byte cap is shrunk before
// it's persisted, rather than stored as is to fail at the API on every turn.
func TestStoreAttachmentShrinksOverByteCap(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	orig := paddedPNG(t, 64, 32, imageutil.DefaultMaxBytes)

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: orig, SourceHint: "photo.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false")
	}
	if ref.Size > imageutil.DefaultMaxBytes || ref.MimeType != "image/png" {
		t.Errorf("stored %d bytes of %s, want a PNG within %d", ref.Size, ref.MimeType, imageutil.DefaultMaxBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(a.loadAttachments([]event.Attachment{ref})[0].Base64Data)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := imageDims(t, raw); w > 64 || w < 2*h-1 || w > 2*h+1 {
		t.Errorf("stored %dx%d, want at most 64x32 with its 2:1 aspect", w, h)
	}
}

func imageDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode stored image: %v", err)
	}
	return cfg.Width, cfg.Height
}
