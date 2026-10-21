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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/llm"
)

// The far-side messages of the transient failures that crashed sessions in a
// real amplio DB (Aug-Sep 2026), relayed verbatim by the bridge.
var observedTransient = []string{
	"StreamGenerateContent RPC failed with status: RPC::DEADLINE_EXCEEDED: ",
	"UNAVAILABLE (code 503): No capacity available for model fable-5.1-bon-le on the server",
	"INTERNAL (code 500): Internal error encountered.",
	"no first token within 25s (the backend accepted the request and then went silent)",
}

// scripted is a transport that answers each /generate with the next canned
// NDJSON body (the last one repeats), counting posts.
type scripted struct {
	mu     sync.Mutex
	bodies []string
	status int // non-200 answers every post with this status instead
	posts  int
}

func (s *scripted) post(_ context.Context, _ string, _ []byte) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts++
	if s.status != 0 {
		return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("nope"))}, nil
	}
	body := s.bodies[min(s.posts, len(s.bodies))-1]
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (s *scripted) describe() string { return "scripted" }

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func errLine(msg string) string {
	b, _ := json.Marshal(map[string]string{"type": "error", "error": msg})
	return string(b) + "\n"
}

const (
	deltaHel = `{"type":"delta","text":"hel"}` + "\n"
	deltaLo  = `{"type":"delta","text":"lo"}` + "\n"
	finalOK  = `{"type":"final","response":{"content":"hello"}}` + "\n"
	ping     = `{"type":"ping"}` + "\n"
)

func noBackoff(t *testing.T) {
	t.Helper()
	old := baseRetryDelay
	baseRetryDelay = 0
	t.Cleanup(func() { baseRetryDelay = old })
}

func drain(t *testing.T, s llm.Stream) (text string, err error) {
	t.Helper()
	defer s.Close()
	for s.Next() {
		text += s.Event().DeltaText
	}
	return text, s.Err()
}

func TestIsTransient(t *testing.T) {
	for _, msg := range observedTransient {
		if !isTransient(&Error{Code: CodeProvider, Message: "bridge error: " + msg}) {
			t.Errorf("observed crash cause not transient: %q", msg)
		}
	}
	for _, err := range []error{
		// Context-window overflow: compaction has to see it, so never retried.
		&Error{Code: CodeProvider, Message: "bridge error: INVALID_ARGUMENT (code 400): The input token count (1200000) exceeds the maximum number of tokens allowed (1048576)."},
		&Error{Code: CodeProvider, Message: "bridge error: prompt is too long: 250000 tokens > 200000 maximum"},
		&Error{Code: CodeProvider, Message: "bridge error: RESOURCE_EXHAUSTED (code 429): request exceeds the context window"},
		// Other provider failures that a retry cannot fix.
		&Error{Code: CodeProvider, Message: "bridge error: INVALID_ARGUMENT (code 400): bad tool schema"},
		// Wrong class: the spec, the token or the build has to change.
		&Error{Code: CodeNotAllowed, Message: "bridge scripted: HTTP 403: UNAVAILABLE"},
		&Error{Code: CodeUnauthorized, Message: "bridge scripted: HTTP 401"},
		&Error{Code: CodeProtocol, Message: "bridge: bad protocol line"},
		// Not from a bridge at all.
		errors.New("bridge: read stream: unexpected EOF (code 503)"),
		nil,
	} {
		if isTransient(err) {
			t.Errorf("isTransient(%v) = true, want false", err)
		}
	}
}

func TestCall_RetriesTransientThenSucceeds(t *testing.T) {
	noBackoff(t)
	tr := &scripted{bodies: []string{errLine(observedTransient[1]), ping + errLine(observedTransient[3]), deltaHel + finalOK}}
	p := &provider{tr: tr, model: "m"}
	resp, err := p.Call(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Content != "hello" || tr.count() != 3 {
		t.Errorf("content %q after %d posts, want %q after 3", resp.Content, tr.count(), "hello")
	}
}

func TestCall_GivesUpAfterMaxAttemptsWithTheFarSideError(t *testing.T) {
	noBackoff(t)
	tr := &scripted{bodies: []string{errLine(observedTransient[1])}}
	p := &provider{tr: tr, model: "m"}
	_, err := p.Call(context.Background(), llm.Request{})
	if tr.count() != maxAttempts {
		t.Errorf("posts = %d, want %d", tr.count(), maxAttempts)
	}
	if CodeOf(err) != CodeProvider || !strings.Contains(err.Error(), "No capacity") {
		t.Errorf("err = %v (code %q), want the far side's provider error", err, CodeOf(err))
	}
}

func TestCall_DoesNotRetryNonTransient(t *testing.T) {
	noBackoff(t)
	for name, tr := range map[string]*scripted{
		"overflow":    {bodies: []string{errLine("prompt is too long: 250000 tokens > 200000 maximum"), finalOK}},
		"bad request": {bodies: []string{errLine("INVALID_ARGUMENT (code 400): bad tool schema"), finalOK}},
		"not allowed": {status: http.StatusForbidden},
	} {
		p := &provider{tr: tr, model: "m"}
		if _, err := p.Call(context.Background(), llm.Request{}); err == nil {
			t.Errorf("%s: Call succeeded, want the error", name)
		}
		if tr.count() != 1 {
			t.Errorf("%s: posts = %d, want 1 (no retry)", name, tr.count())
		}
	}
}

func TestStream_RetriesTransientBeforeFirstDelta(t *testing.T) {
	noBackoff(t)
	tr := &scripted{bodies: []string{ping + errLine(observedTransient[0]), ping + deltaHel + deltaLo + finalOK}}
	p := &provider{tr: tr, model: "m"}
	s, err := p.Stream(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, err := drain(t, s)
	if err != nil || text != "hello" || s.Response().Content != "hello" {
		t.Errorf("deltas %q, final %q, err %v; want hello/hello/nil", text, s.Response().Content, err)
	}
	if tr.count() != 2 {
		t.Errorf("posts = %d, want 2", tr.count())
	}
}

// Once a delta has reached the caller, a retry would show the text twice: the
// error surfaces through the stream as before.
func TestStream_NoRetryAfterFirstDelta(t *testing.T) {
	noBackoff(t)
	tr := &scripted{bodies: []string{deltaHel + errLine(observedTransient[0]), deltaHel + deltaLo + finalOK}}
	p := &provider{tr: tr, model: "m"}
	s, err := p.Stream(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, err := drain(t, s)
	if text != "hel" || !strings.Contains(fmtErr(err), "DEADLINE_EXCEEDED") {
		t.Errorf("deltas %q, err %v; want hel + the deadline error", text, err)
	}
	if tr.count() != 1 {
		t.Errorf("posts = %d, want 1", tr.count())
	}
}

// A response with no deltas (e.g. tool calls only) still reaches the caller
// whole after the read-ahead, and Next stays false past the end.
func TestStream_FinalWithoutDeltas(t *testing.T) {
	tr := &scripted{bodies: []string{finalOK + deltaHel}}
	p := &provider{tr: tr, model: "m"}
	s, err := p.Stream(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, err := drain(t, s)
	if text != "" || err != nil || s.Response().Content != "hello" {
		t.Errorf("deltas %q, final %q, err %v; want none/hello/nil", text, s.Response().Content, err)
	}
}

// Exhausted retries before any delta: the far side's error comes back from
// Stream itself; a non-transient one comes back through the stream, as before.
func TestStream_ErrorsAfterRetriesAndNonTransient(t *testing.T) {
	noBackoff(t)
	tr := &scripted{bodies: []string{errLine(observedTransient[2])}}
	p := &provider{tr: tr, model: "m"}
	if _, err := p.Stream(context.Background(), llm.Request{}); !strings.Contains(fmtErr(err), "INTERNAL (code 500)") {
		t.Errorf("Stream err = %v, want the far side's 500", err)
	}
	if tr.count() != maxAttempts {
		t.Errorf("posts = %d, want %d", tr.count(), maxAttempts)
	}

	tr = &scripted{bodies: []string{errLine("INVALID_ARGUMENT (code 400): bad tool schema")}}
	p = &provider{tr: tr, model: "m"}
	s, err := p.Stream(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, err := drain(t, s); !strings.Contains(fmtErr(err), "bad tool schema") || tr.count() != 1 {
		t.Errorf("err %v after %d posts; want the 400 after 1", err, tr.count())
	}
}

func TestRetry_StopsWhenContextEnds(t *testing.T) {
	old := baseRetryDelay
	baseRetryDelay = time.Hour
	t.Cleanup(func() { baseRetryDelay = old })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	tr := &scripted{bodies: []string{errLine(observedTransient[1])}}
	p := &provider{tr: tr, model: "m"}
	start := time.Now()
	_, err := p.Call(ctx, llm.Request{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second || tr.count() != 1 {
		t.Errorf("err %v after %s and %d posts; want the context's deadline, promptly, after 1", err, time.Since(start), tr.count())
	}
}

func fmtErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
