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

package bridge

import (
	"bytes"
	"encoding/base64"
	"image"
	"net/url"
	"strings"
	"testing"

	"github.com/HugoSmits86/nativewebp"

	"amplio/internal/llm"
)

func webpRequest(t *testing.T) llm.Request {
	t.Helper()
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatalf("encode webp: %v", err)
	}
	return llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "look",
		Attachments: []llm.Attachment{{MimeType: "image/webp", Base64Data: base64.StdEncoding.EncodeToString(buf.Bytes())}}}}}
}

// A bridge gets WebP as it is: an amplio at the far end converts for its own
// backend if it must. webp=false is for a bridge that cannot take it.
func TestBridge_SendsWebPUnlessToldNotTo(t *testing.T) {
	req := webpRequest(t)
	for _, tc := range []struct {
		name     string
		webp     string
		wantMime string
	}{
		{"default", "", "image/webp"},
		{"webp=true", "true", "image/webp"},
		{"webp=false", "false", "image/png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := url.Values{"url": {"http://127.0.0.1:1"}}
			sub := url.Values{"bin": {"/opt/bridges/corp"}}
			if tc.webp != "" {
				args.Set("webp", tc.webp)
				sub.Set("webp", tc.webp)
			}
			dialled, err := NewBridge("m", 10, args, nil)
			if err != nil {
				t.Fatalf("NewBridge: %v", err)
			}
			spawned, err := NewSubprocess("m", 10, sub, nil)
			if err != nil {
				t.Fatalf("NewSubprocess: %v", err)
			}
			for _, p := range []llm.Provider{dialled, spawned} {
				w := p.(*provider).toWire(req)
				if got := w.Messages[0].Attachments[0].MimeType; got != tc.wantMime {
					t.Errorf("%T: attachment sent as %s, want %s", p.(*provider).tr, got, tc.wantMime)
				}
			}
			// webp= is ours: a spawned bridge is not told about it.
			if q := spawned.(*provider).tr.(*spawnTransport).specQuery; strings.Contains(q, "webp") {
				t.Errorf("AMPLIO_BRIDGE_SPEC = %q, must not carry webp=", q)
			}
		})
	}
}

func TestBridge_RejectsANonBooleanWebP(t *testing.T) {
	if _, err := NewBridge("m", 10, url.Values{"url": {"http://x"}, "webp": {"png"}}, nil); err == nil {
		t.Error("NewBridge: want an error for webp=png")
	}
	if _, err := NewSubprocess("m", 10, url.Values{"bin": {"/b"}, "webp": {"png"}}, nil); err == nil {
		t.Error("NewSubprocess: want an error for webp=png")
	}
}
