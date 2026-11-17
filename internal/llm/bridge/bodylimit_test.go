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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"amplio/internal/llm"
)

// imageRequest is a request whose history holds n images of size base64 bytes
// each, plus a final user turn.
func imageRequest(n, size int) llm.Request {
	img := strings.Repeat("A", size)
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "look at these"}}
	for range n {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "img",
			Attachments: []llm.Attachment{{MimeType: "image/png", Base64Data: img}}})
	}
	return llm.Request{Messages: msgs}
}

// A request body over 16 MiB, the size of one response line, is served whole.
// The server used to read it through a reader capped at that size, so it was
// cut off and failed with "bad request: unexpected EOF": image-heavy sessions
// crashed at the call whose history first passed 16 MiB, and again at every
// resume. 17 images of 1 MB is 17 MB of body.
func TestGenerate_BodyOverOneLineIsServed(t *testing.T) {
	fake := &fakeProvider{final: &llm.Response{Content: "ok", StopReason: "end_turn"}}
	p := serveBridge(t, fake)
	if _, err := p.Call(context.Background(), imageRequest(17, 1_000_000)); err != nil {
		t.Fatalf("Call with a 17 MB body: %v", err)
	}
	n := 0
	for _, m := range fake.got.Messages {
		n += len(m.Attachments)
	}
	if n != 17 {
		t.Errorf("the far side got %d images, want all 17", n)
	}
}

// The same for an embedding request.
func TestEmbed_BodyOverOneLineIsServed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /embed", EmbedHandler(&fakeEmbedder{model: "m"}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e, err := NewEmbedder("", url.Values{"url": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 4)
	for i := range texts {
		texts[i] = strings.Repeat("x", 5_000_000) // 20 MB in one batch
	}
	if v, err := e.Embed(context.Background(), texts); err != nil || len(v) != len(texts) {
		t.Fatalf("Embed with a 20 MB body = %d vectors, %v", len(v), err)
	}
}

// A body over the limit gets 413 with its size in words, not the decoder's
// "unexpected EOF". The client keeps the words in its error, which is no
// transient fault, and they match an overflow signature, so the event loop's
// compaction can take it up rather than the session crashing.
func TestBadRequest_OverTheLimitIs413(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /generate", func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest // the handler's decode, with a small limit
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		t.Error("an over-limit body was decoded")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p, err := NewBridge("m", 4096, url.Values{"url": {srv.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Call(context.Background(), imageRequest(1, 4096))
	var be *Error
	if !errors.As(err, &be) || !strings.Contains(be.Message, "HTTP 413: request too large: body exceeds the maximum of 1024 bytes") {
		t.Fatalf("error = %v, want the 413 with its size", err)
	}
	if isTransient(err) {
		t.Error("an over-limit body counted as transient: it would be retried, the same size")
	}
	overflow := false
	for _, sig := range overflowSignatures {
		overflow = overflow || strings.Contains(strings.ToLower(be.Message), sig)
	}
	if !overflow {
		t.Errorf("%q matches no overflow signature", be.Message)
	}

	// An undecodable body that isn't too big stays a 400.
	rec := httptest.NewRecorder()
	badRequest(rec, errors.New("invalid character"))
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte("bad request: invalid character")) {
		t.Errorf("other decode error = %d %q", rec.Code, rec.Body.String())
	}
}
