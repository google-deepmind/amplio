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

package llm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"strings"
	"testing"

	"github.com/HugoSmits86/nativewebp"
)

// webpB64 returns a small lossless WebP, base64-encoded, and its picture.
func webpB64(t *testing.T, seed uint8) (string, image.Image) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 6, 4))
	for i := range img.Pix {
		img.Pix[i] = seed + uint8(i)
	}
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode webp: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), img
}

// resetPNGCache empties the conversion cache for one test, and counts the
// conversions it makes.
func resetPNGCache(t *testing.T) *int {
	t.Helper()
	old, oldConv := pngs, toPNG
	pngs = &pngCache{max: pngCacheMaxBytes}
	n := new(int)
	toPNG = func(b []byte) ([]byte, error) { *n++; return oldConv(b) }
	t.Cleanup(func() { pngs, toPNG = old, oldConv })
	return n
}

func TestWithoutWebP_ConvertsWebPToTheSamePicture(t *testing.T) {
	conversions := resetPNGCache(t)
	w, pic := webpB64(t, 7)
	jpg := Attachment{MimeType: "image/jpeg", Base64Data: "/9j/"}
	msgs := []Message{
		{Role: RoleUser, Content: "task"},
		{Role: RoleToolResult, Content: "two images", Attachments: []Attachment{{MimeType: "image/webp", Base64Data: w}, jpg}},
	}

	out := WithoutWebP(msgs)
	if got := msgs[1].Attachments[0].MimeType; got != "image/webp" {
		t.Fatalf("the input was modified: mime %q", got)
	}
	atts := out[1].Attachments
	if len(atts) != 2 || atts[0].MimeType != "image/png" || atts[1] != jpg {
		t.Fatalf("attachments = %+v; want the webp as png, then the jpeg as it was", atts)
	}
	data, err := base64.StdEncoding.DecodeString(atts[0].Base64Data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	for y := range 4 {
		for x := range 6 {
			if g, w := color.NRGBAModel.Convert(got.At(x, y)), color.NRGBAModel.Convert(pic.At(x, y)); g != w {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, g, w)
			}
		}
	}
	if out[1].Content != "two images" {
		t.Errorf("content = %q, want it unchanged", out[1].Content)
	}

	// The next turn resends the same image: it is not converted again.
	again := WithoutWebP(msgs)
	if again[1].Attachments[0].Base64Data != atts[0].Base64Data {
		t.Error("the second conversion differs from the first")
	}
	if *conversions != 1 {
		t.Errorf("%d conversions for one image sent twice, want 1", *conversions)
	}
}

func TestWithoutWebP_ReturnsAHistoryWithoutWebPAsItIs(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "task", Attachments: []Attachment{{MimeType: "image/png", Base64Data: "iVBO"}}}}
	if out := WithoutWebP(msgs); &out[0] != &msgs[0] {
		t.Error("a history without WebP was copied")
	}
}

func TestWithoutWebP_NotesAnImageItCannotConvert(t *testing.T) {
	resetPNGCache(t)
	toPNG = func([]byte) ([]byte, error) { return nil, errors.New("decode webp: bad header") }
	msgs := []Message{{Role: RoleToolResult, Content: "Image: a.webp",
		Attachments: []Attachment{{MimeType: "image/webp", Base64Data: "UklG"}}}}
	out := WithoutWebP(msgs)
	if len(out[0].Attachments) != 0 {
		t.Errorf("attachments = %+v, want the image dropped", out[0].Attachments)
	}
	if want := "Image: a.webp\n[image omitted from this request: it is WebP, which this model's server does not accept, and converting it to PNG failed: decode webp: bad header]"; out[0].Content != want {
		t.Errorf("content = %q\nwant %q", out[0].Content, want)
	}
}

func TestPNGCache_EvictsTheLeastRecentlyUsed(t *testing.T) {
	c := &pngCache{max: 10}
	key := func(b byte) (k [32]byte) { k[0] = b; return k }
	c.put(key(1), "aaaa")
	c.put(key(2), "bbbb")
	if _, ok := c.get(key(1)); !ok { // 1 is now the most recently used
		t.Fatal("1 missing")
	}
	c.put(key(3), "cccc") // 12 bytes: 2 goes
	if _, ok := c.get(key(2)); ok {
		t.Error("2 was kept; want it evicted as the least recently used")
	}
	for _, k := range []byte{1, 3} {
		if _, ok := c.get(key(k)); !ok {
			t.Errorf("%d evicted", k)
		}
	}
	if c.size != 8 {
		t.Errorf("size = %d, want 8", c.size)
	}
	c.put(key(4), strings.Repeat("d", 11)) // larger than the whole cache
	if _, ok := c.get(key(4)); ok || c.size != 8 {
		t.Errorf("an entry over the cache's size was kept (size %d)", c.size)
	}
}

func TestWebPArg(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		def     bool
		want    bool
		wantErr bool
	}{
		{"", true, true, false},
		{"", false, false, false},
		{"true", false, true, false},
		{"false", true, false, false},
		{"maybe", true, false, true},
	} {
		v := url.Values{}
		if tc.arg != "" {
			v.Set("webp", tc.arg)
		}
		got, err := WebPArg(v, tc.def)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("WebPArg(webp=%q, def %v) = %v, %v; want %v, error %v", tc.arg, tc.def, got, err, tc.want, tc.wantErr)
		}
	}
}
