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
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"slices"
	"testing"
	"time"

	"golang.org/x/image/webp"

	"amplio/internal/blob"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/imageutil"
	"amplio/internal/llm"
	"amplio/internal/tool"
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

// A PNG whose lossless WebP is smaller is stored as that WebP: the same picture
// in fewer bytes, resent every turn.
func TestStoreAttachmentStoresSmallerWebP(t *testing.T) {
	a := newT(testCfg{BlobStore: blob.NewStore(t.TempDir())})
	// Smooth gradients with a little noise, which WebP's predictors compress
	// better than PNG does, as they do screenshots.
	noise := make([]byte, 200*150)
	rand.New(rand.NewSource(1)).Read(noise) //nolint:gosec // test determinism
	img := image.NewRGBA(image.Rect(0, 0, 200, 150))
	for y := range 150 {
		for x := range 200 {
			n := noise[y*200+x] & 3
			img.Set(x, y, color.RGBA{uint8(x) + n, uint8(y) + n, uint8(x+y)/2 + n, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	orig := buf.Bytes()

	ref, ok := a.storeAttachment(event.Attachment{MimeType: "image/png", Data: orig, SourceHint: "shot.png"})
	if !ok {
		t.Fatal("storeAttachment returned ok=false")
	}
	if ref.MimeType != "image/webp" || ref.Size >= len(orig) {
		t.Fatalf("stored %d bytes of %s; want a webp smaller than the png's %d", ref.Size, ref.MimeType, len(orig))
	}
	atts := a.loadAttachments([]event.Attachment{ref})
	if len(atts) != 1 || atts[0].MimeType != "image/webp" {
		t.Fatalf("loaded %+v, want one image/webp", atts)
	}
	raw, err := base64.StdEncoding.DecodeString(atts[0].Base64Data)
	if err != nil {
		t.Fatal(err)
	}
	back, err := webp.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode stored webp: %v", err)
	}
	for y := range 150 {
		for x := range 200 {
			if g, w := color.RGBAModel.Convert(back.At(x, y)), img.At(x, y); g != w {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, g, w)
			}
		}
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
	_ = binary.Write(&chunk, binary.BigEndian, uint32(pad)) //nolint:gosec // G115: test padding, a few MB
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

// A fast tool's result is persisted while another tool's screenshot in the same
// step is still being encoded, not after it.
func TestToolResults_FastResultNotHeldBehindAnImageEncode(t *testing.T) {
	store, runID, _ := testSetup(t)
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "s", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	a := newT(testCfg{RunID: runID, SessionID: "s", Store: store, BlobStore: blob.NewStore(t.TempDir())})

	encoding, release := make(chan struct{}), make(chan struct{})
	old := toWebP
	toWebP = func(data []byte, mime string) ([]byte, string, bool, error) {
		close(encoding)
		<-release
		return data, mime, false, nil
	}
	t.Cleanup(func() { toWebP = old })

	img := pngBytes(t, 8, 8)
	shot := &tool.Tool{Name: "shot", ParamType: &struct{}{}, Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
		return &tool.Result{Content: "screenshot", Attachments: []event.Attachment{{MimeType: "image/png", Data: img}}}, nil
	}}
	fast := &tool.Tool{Name: "fast", ParamType: &struct{}{}, Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
		<-encoding // finish while the screenshot is being encoded
		return &tool.Result{Content: "done"}, nil
	}}
	calls := []llm.ToolCall{{ID: "c1", Name: "shot", Arguments: "{}"}, {ID: "c2", Name: "fast", Arguments: "{}"}}
	done := make(chan error, 1)
	go func() {
		done <- a.executeAndAppendToolResults(ctx, 1, calls, map[string]*tool.Tool{"shot": shot, "fast": fast})
	}()

	persisted := func(id string) bool {
		recs, err := store.GetEvents(ctx, runID, "s", db.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if tr, ok := r.Event.(*event.ToolResultEvent); ok && tr.ToolCallID == id {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); !persisted("c2"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the fast tool's result waited for the other tool's image encode")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !persisted("c1") {
		t.Error("the screenshot's result was not persisted")
	}
}
