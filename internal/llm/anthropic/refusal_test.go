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

package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"amplio/internal/llm"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// sseTurn renders a minimal Anthropic SSE stream for one assistant turn: an
// optional text block, then message_delta carrying stopReason and the raw
// stopDetails JSON ("null" for none), as the Messages API sends it.
func sseTurn(text, stopReason, stopDetails string) string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"stop_details":null,"usage":{"input_tokens":25,"output_tokens":0}}}`)
	if text != "" {
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	}
	ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null,"stop_details":%s},"usage":{"output_tokens":0}}`, stopReason, stopDetails))
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// fakeProvider serves body as the SSE response to every request.
func fakeProvider(t *testing.T, body string) *provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	return &provider{client: client, model: "claude-opus-5-5", maxTokens: 1024}
}

// callBothPaths runs the blocking Call and the Stream path against the same
// fake stream, so each assertion covers both (both accumulate the stream).
func callBothPaths(t *testing.T, p *provider) map[string]*llm.Response {
	t.Helper()
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
	callResp, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	st, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer st.Close()
	for st.Next() {
	}
	if err := st.Err(); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return map[string]*llm.Response{"Call": callResp, "Stream": st.Response()}
}

// TestRefusal_StopDetailsCaptured guards the stop_details capture on the
// streaming path we always use. It relies on the SDK's Message.Accumulate
// copying message_delta.stop_details, which versions before anthropic-sdk-go
// v1.47 did not: on those, a refused turn carries no category or explanation.
func TestRefusal_StopDetailsCaptured(t *testing.T) {
	p := fakeProvider(t, sseTurn("", "refusal",
		`{"type":"refusal","category":"cyber","explanation":"This request was declined because it could enable cyber harm."}`))

	for path, resp := range callBothPaths(t, p) {
		if resp.StopReason != "refusal" {
			t.Errorf("%s: StopReason = %q, want refusal", path, resp.StopReason)
		}
		r := resp.Refusal
		if r == nil {
			t.Errorf("%s: Refusal = nil, want set", path)
			continue
		}
		if r.Category != "cyber" || r.Explanation != "This request was declined because it could enable cyber harm." {
			t.Errorf("%s: Refusal = %+v, want category/explanation from stop_details", path, r)
		}
		// Diagnostic only: nothing leaks into the replayed cargo.
		if resp.ProviderExtra != nil {
			t.Errorf("%s: ProviderExtra = %#v, want nil", path, resp.ProviderExtra)
		}
	}
}

// TestRefusal_NullCategory: the API documents category/explanation as null when
// a refusal maps to no named category. The turn is still a refusal, with both
// fields empty.
func TestRefusal_NullCategory(t *testing.T) {
	p := fakeProvider(t, sseTurn("", "refusal", `{"type":"refusal","category":null,"explanation":null}`))

	for path, resp := range callBothPaths(t, p) {
		r := resp.Refusal
		if r == nil {
			t.Errorf("%s: Refusal = nil, want set", path)
			continue
		}
		if r.Category != "" || r.Explanation != "" {
			t.Errorf("%s: Refusal = %+v, want empty category/explanation", path, r)
		}
	}
}

// TestRefusal_WithoutStopDetails: a refusal stop reason alone (no stop_details
// on the wire) still marks the turn as refused.
func TestRefusal_WithoutStopDetails(t *testing.T) {
	p := fakeProvider(t, sseTurn("", "refusal", "null"))

	for path, resp := range callBothPaths(t, p) {
		if resp.Refusal == nil {
			t.Fatalf("%s: Refusal = nil, want set from stop_reason alone", path)
		}
		if resp.Refusal.Category != "" || resp.Refusal.Explanation != "" {
			t.Errorf("%s: Refusal = %+v, want empty category/explanation", path, resp.Refusal)
		}
	}
}

// TestRefusal_NormalTurnUntouched: an ordinary turn has no Refusal and no
// ProviderExtra, exactly as before.
func TestRefusal_NormalTurnUntouched(t *testing.T) {
	p := fakeProvider(t, sseTurn("391", "end_turn", "null"))

	for path, resp := range callBothPaths(t, p) {
		if resp.Content != "391" || resp.StopReason != "end_turn" {
			t.Errorf("%s: got (%q, %q), want (\"391\", end_turn)", path, resp.Content, resp.StopReason)
		}
		if resp.Refusal != nil || resp.ProviderExtra != nil {
			t.Errorf("%s: Refusal = %+v, ProviderExtra = %#v, want both nil", path, resp.Refusal, resp.ProviderExtra)
		}
	}
}
