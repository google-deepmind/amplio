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

package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"amplio/internal/event"
	"amplio/internal/llm"
)

const (
	rsModel   = "test/model"
	rsHi      = `{"type":"message","role":"user","content":"hi"}`
	rsNext    = `{"type":"message","role":"user","content":"next"}`
	rsMessage = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
	rsOK      = `{"id":"r","status":"completed","output":[` + rsMessage + `]}`
	rsPreview = `{"type":"response.output_text.delta","delta":"preview"}`
)

// Script the HTTP peer, not the provider. Each POST consumes one reply;
// missing and extra attempts are failures, not waits on a channel. Injecting
// at RoundTrip also lets framing tests choose actual Read boundaries.
type rsReply struct {
	code  int
	media string
	body  io.Reader
}

type rsPost struct {
	*http.Request
	json map[string]any
}

type rsPeer struct {
	t       *testing.T
	replies []rsReply
	posts   []rsPost
}

func rsBackend(t *testing.T, replies ...rsReply) *rsPeer {
	t.Helper()
	t.Setenv("OPENAI_BASE_URL", "https://gateway.test/env/v1///")
	s := &rsPeer{t: t, replies: replies}
	t.Cleanup(func() {
		if !t.Failed() && len(s.posts) != len(replies) {
			t.Errorf("got %d POSTs, want %d", len(s.posts), len(replies))
		}
	})
	return s
}

func rsDecode(t *testing.T, r io.Reader) any {
	t.Helper()
	d := json.NewDecoder(r)
	d.UseNumber() // opaque replay items can contain integers larger than 2^53
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if d.Decode(new(any)) != io.EOF {
		t.Fatal("trailing data after JSON value")
	}
	return v
}

func rsEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
	}
}

func (s *rsPeer) RoundTrip(r *http.Request) (*http.Response, error) {
	s.t.Helper()
	if r.Body == nil {
		s.t.Fatal("missing request body")
	}
	defer r.Body.Close()
	n := len(s.posts)
	if n == len(s.replies) {
		s.t.Fatalf("unexpected POST #%d to %s", n+1, r.URL)
	}
	body, ok := rsDecode(s.t, r.Body).(map[string]any)
	if !ok {
		s.t.Fatal("request is not a JSON object")
	}
	rsEqual(s.t, "method", r.Method, http.MethodPost)
	rsEqual(s.t, "Content-Type", r.Header.Get("Content-Type"), "application/json")
	if body["stream"] == true {
		rsEqual(s.t, "Accept", r.Header.Get("Accept"), "text/event-stream")
	}
	s.posts = append(s.posts, rsPost{r, body})
	reply := s.replies[n]
	return &http.Response{
		StatusCode: reply.code, Status: fmt.Sprintf("%d %s", reply.code, http.StatusText(reply.code)),
		Header: http.Header{"Content-Type": {reply.media}}, Body: io.NopCloser(reply.body), Request: r,
	}, nil
}

func (s *rsPeer) provider(limit int, client, query url.Values) llm.Provider {
	s.t.Helper()
	p, err := NewResponses(rsModel, limit, client, query)
	if err != nil {
		s.t.Fatal(err)
	}
	p.(*responsesProvider).http.Transport = s
	return p
}

// Compare the history-bearing portion of a particular POST, independently
// of formatting, object key order and unrelated request options.
func (s *rsPeer) history(n int, stored bool, previous string, input ...string) {
	s.t.Helper()
	if n >= len(s.posts) {
		s.t.Fatalf("missing POST #%d (got %d)", n+1, len(s.posts))
	}
	b := s.posts[n].json
	store := b["store"]
	if store == nil {
		store = false
	}
	var id any
	if previous != "" {
		id = previous
	}
	rsEqual(s.t, fmt.Sprintf("POST #%d [store, previous_response_id, input]", n+1),
		[]any{store, b["previous_response_id"], b["input"]},
		[]any{stored, id, rsDecode(s.t, strings.NewReader("["+strings.Join(input, ",")+"]"))})
}

func rsJSON(code int, doc string) rsReply {
	return rsReply{code, "application/json", strings.NewReader(doc)}
}

func rsSSE(events ...string) rsReply {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", strings.ReplaceAll(e, "\n", "\ndata: "))
	}
	return rsReply{200, "text/event-stream", strings.NewReader(b.String())}
}

func rsDoc(status string, items ...string) string {
	return `{"status":"` + status + `","output":[` + strings.Join(items, ",") + `]}`
}

func rsEnd(status, doc string) string {
	return `{"type":"response.` + status + `","response":` + doc + `}`
}

func rsItem(phase string, index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.%s","output_index":%d,"item":%s}`, phase, index, item)
}

type rsMode string

const (
	rsREST   rsMode = "Call"
	rsStream rsMode = "Stream"
	rsDrain  rsMode = "Call-via-SSE"
)

func (m rsMode) settings() url.Values {
	return url.Values{"call_stream": {strconv.FormatBool(m == rsDrain)}}
}

func (m rsMode) terminal(status, doc string, live ...string) rsReply {
	if m == rsREST {
		return rsJSON(200, doc)
	}
	return rsSSE(append(live, rsEnd(status, doc))...)
}

type rsResult struct {
	out    *llm.Response
	err    error
	events []llm.StreamEvent
}

func (m rsMode) run(t *testing.T, p llm.Provider, req llm.Request) rsResult {
	t.Helper()
	var r rsResult
	if m != rsStream {
		r.out, r.err = p.Call(t.Context(), req)
		return r
	}
	// These scripts promise an accepted stream. Handshake failures have
	// a different public contract and are tested at Stream itself.
	s, err := p.Stream(t.Context(), req)
	if err != nil {
		t.Fatalf("opening stream: %v", err)
	}
	defer s.Close()
	for s.Next() {
		r.events = append(r.events, s.Event())
	}
	r.out, r.err = s.Response(), s.Err()
	if s.Next() {
		t.Fatalf("event escaped after end of stream: %+v", s.Event())
	}
	return r
}

func (r rsResult) ok(t *testing.T) *llm.Response {
	t.Helper()
	if r.err != nil || r.out == nil {
		t.Fatalf("response = %+v, err = %v", r.out, r.err)
	}
	return r.out
}

func rsError(t *testing.T, err error, details ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, d := range details {
		if !strings.Contains(err.Error(), d) {
			t.Fatalf("error %q does not identify %q", err, d)
		}
	}
}

func (r rsResult) fails(t *testing.T, delivered []llm.StreamEvent, details ...string) {
	t.Helper()
	rsError(t, r.err, details...)
	if r.out != nil {
		t.Fatalf("failed attempt committed %+v", r.out)
	}
	rsEqual(t, "events before failure", r.events, delivered)
}

func rsPlay(t *testing.T, events ...string) rsResult {
	t.Helper()
	s := rsBackend(t, rsSSE(events...))
	return rsStream.run(t, s.provider(128, nil, nil), llm.Request{})
}

func rsUser(text string) llm.Message          { return llm.Message{Role: llm.RoleUser, Content: text} }
func rsHistory(ms ...llm.Message) llm.Request { return llm.Request{Messages: ms} }

// Configuration and the outbound protocol.

func TestResponsesInvalidConfiguration(t *testing.T) {
	for k, v := range map[string]string{
		"profile": "unknown", "webp": "sometimes", "call_stream": "sometimes", "tool_strict": "sometimes",
	} {
		t.Run("client/"+k, func(t *testing.T) {
			_, err := NewResponses(rsModel, 128, url.Values{k: {v}}, nil)
			rsError(t, err)
		})
	}
	for _, tc := range []struct{ key, value, why string }{
		{"input", "x", "managed by the provider"}, {"previous_response_id", "r", "managed by the provider"},
		{"conversation", "c", "incompatible"}, {"background", "true", "background execution"},
		{"strict", "false", "tool_strict"}, {"stream", "true", "call_stream"}, {"store", "sometimes", "boolean"},
		{"max_output_tokens", "16.5", "integer"}, {"max_output_tokens", "15", "minimum"}, {"max_output_tokens", "0", "minimum"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := NewResponses(rsModel, 128, nil, url.Values{tc.key: {tc.value}})
			rsError(t, err, tc.why)
		})
	}
	_, err := NewResponses(rsModel, 15, nil, nil)
	rsError(t, err, "minimum")
}

func TestResponsesAddressAndCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "default")
	t.Setenv("RS_OTHER_KEY", "other")
	t.Setenv("RS_EMPTY_KEY", "")
	for _, tc := range []struct {
		name, base, keyEnv, address, auth string
		stored                            bool
	}{
		{"environment", "", "", "https://gateway.test/env/v1/responses", "Bearer default", true},
		{"client overrides", "https://override.test/v1/", "RS_OTHER_KEY", "https://override.test/v1/responses", "Bearer other", true},
		{"no selected key", "", "RS_EMPTY_KEY", "https://gateway.test/env/v1/responses", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rsBackend(t, rsJSON(200, rsOK))
			p := s.provider(128, url.Values{"base_url": {tc.base}, "api_key_env": {tc.keyEnv}}, nil)
			rsREST.run(t, p, llm.Request{}).ok(t)
			rsEqual(t, "ModelID", p.ModelID(), rsModel)
			rsEqual(t, "URL", s.posts[0].URL.String(), tc.address)
			rsEqual(t, "Authorization", s.posts[0].Header.Get("Authorization"), tc.auth)
			s.history(0, tc.stored, "")
		})
	}
}

func TestResponsesBudget(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		base, query, request, cap, wire int
	}{
		{"default", 64, 0, 0, 64, 64},
		{"validate resolved limit", 8, 16, -1, 16, 16},
		{"request wins without changing provider", 256, 64, 16, 64, 16},
		{"no limit", 0, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rsBackend(t, rsJSON(200, rsOK))
			q := url.Values{}
			if tc.query != 0 {
				q.Set("max_output_tokens", strconv.Itoa(tc.query))
			}
			p := s.provider(tc.base, nil, q)
			rsREST.run(t, p, llm.Request{MaxTokens: tc.request}).ok(t)
			want := map[string]any{"model": rsModel, "store": true, "input": []any{}}
			if tc.wire != 0 {
				want["max_output_tokens"] = json.Number(strconv.Itoa(tc.wire))
			}
			rsEqual(t, "request", s.posts[0].json, want)
			rsEqual(t, "MaxTokens", p.MaxTokens(), tc.cap)
		})
	}
}

func TestResponsesRejectsBeforePOST(t *testing.T) {
	p := rsBackend(t).provider(128, nil, nil)
	for _, tc := range []struct {
		req llm.Request
		why string
	}{
		{llm.Request{MaxTokens: 15}, "max_output_tokens"},
		{rsHistory(llm.Message{Role: llm.RoleToolResult, Content: "orphan"}), "missing call_id"},
		{rsHistory(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "lookup"}}}), "missing call_id"},
	} {
		rsREST.run(t, p, tc.req).fails(t, nil, tc.why)
	}
}

func TestResponsesHistoryEncoding(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEUlEQVR4nAAEAPv/Av8AAAMAAwkBAvk/Y+MAAAAASUVORK5CYII="
	const webp = "UklGRhwAAABXRUJQVlA4TA8AAAAvAAAAAAcQ/Y/+ByKi/wEA"
	const image = `{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,` + png + `"}`
	const both = image + `,{"type":"input_image","detail":"auto","image_url":"data:image/webp;base64,` + webp + `"}`
	const bridged = `{"type":"function_call_output","call_id":"c","output":"captured"},{"type":"message","role":"user","content":[` + image + `]}`
	const native = `{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"captured"},` + image + `]}`
	picture := llm.Attachment{MimeType: "image/png", Base64Data: png}
	req := llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Use camera."},
			{Role: llm.RoleUser, Content: "Compare.", Attachments: []llm.Attachment{picture, {MimeType: "image/webp", Base64Data: webp}}},
			{Role: llm.RoleAssistant, Content: "Ready."},
			rsUser("Capture."),
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "capture", Arguments: " "}}},
			{Role: llm.RoleToolResult, ToolCallID: "c", Content: "captured", Attachments: []llm.Attachment{picture}},
		},
		Tools:            []llm.ToolDef{{Name: "capture", Description: "Use camera", Schema: json.RawMessage(`{"type":"object"}`)}},
		DisableToolCalls: true,
	}
	for _, tc := range []struct {
		name, profile, override, images, result string
		strict                                  bool
	}{
		{"default generic", "", "", image + "," + image, bridged, true},
		{"hosted", "openai", "", both, native, false},
		{"vllm", "vllm", "", both, bridged, false},
		{"allow WebP", "generic", "true", both, bridged, false},
		{"disable WebP", "openai", "false", image + "," + image, native, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rsBackend(t, rsJSON(200, rsOK))
			client := url.Values{"profile": {tc.profile}, "webp": {tc.override}}
			if tc.strict {
				client.Set("tool_strict", "true")
			}
			p := s.provider(128, client, url.Values{"tool_choice": {"auto"}})
			rsREST.run(t, p, req).ok(t)
			want := `{"model":"test/model","max_output_tokens":128,"store":true,"tool_choice":"none",
				"tools":[{"type":"function","name":"capture","description":"Use camera","parameters":{"type":"object"},"strict":` + strconv.FormatBool(tc.strict) + `}],
				"input":[{"type":"message","role":"system","content":"Use camera."},
					{"type":"message","role":"user","content":[{"type":"input_text","text":"Compare."},` + tc.images + `]},
					{"type":"message","role":"assistant","content":"Ready."},{"type":"message","role":"user","content":"Capture."},
					{"type":"function_call","call_id":"c","name":"capture","arguments":"{}"},` + tc.result + `]}`
			rsEqual(t, "wire request", s.posts[0].json, rsDecode(t, strings.NewReader(want)))
		})
	}
}

func TestResponsesDeliveryMode(t *testing.T) {
	const refusal = `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot inspect."}]}]}`
	for _, m := range []rsMode{rsREST, rsStream, rsDrain} {
		t.Run(string(m), func(t *testing.T) {
			s := rsBackend(t, m.terminal("completed", refusal, rsPreview))
			p := s.provider(128, m.settings(), url.Values{
				"reasoning.effort": {"high"}, "store": {"true"}, "stream_options.include_obfuscation": {"false"},
			})
			temp := 0.25
			run := m.run(t, p, llm.Request{SystemPrompt: "Be brief.", Temperature: &temp, DisableToolCalls: true})
			r := run.ok(t)
			want := `{"model":"test/model","input":[],"max_output_tokens":128,"store":true,
				"instructions":"Be brief.","temperature":0.25,"reasoning":{"effort":"high"}`
			if m != rsREST {
				want += `,"stream":true,"stream_options":{"include_obfuscation":false}`
			}
			rsEqual(t, "request", s.posts[0].json, rsDecode(t, strings.NewReader(want+"}")))
			if m == rsStream {
				rsEqual(t, "preview", run.events, []llm.StreamEvent{{DeltaText: "preview"}})
			}
			rsEqual(t, "terminal content", r.Content, "")
			rsEqual(t, "refusal", r.Refusal, &llm.Refusal{Category: "refusal", Explanation: "Cannot inspect."})
			rsEqual(t, "stop", r.StopReason, "completed")
		})
	}
}

// Terminal meaning is independent of how the snapshot arrived.

func TestResponsesTerminalProjection(t *testing.T) {
	for _, tc := range []struct {
		name, status, fields string
		want                 llm.Response
	}{
		{"ordered heterogeneous output", "completed", `"output":[
			{"type":"reasoning","content":[{"type":"reasoning_text","text":"First."}],"summary":[{"type":"summary_text","text":"Not additional thoughts."}]},
			{"type":"message","content":[{"type":"output_text","text":"A"},{"type":"refusal","refusal":"Not "}]},
			{"type":"function_call","id":"fc_z","call_id":"z","name":"first","arguments":"{\"k\": 3}","status":"completed"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"Second."}]},
			{"type":"message","content":[{"type":"refusal","refusal":"allowed."},{"type":"output_text","text":"B"}]},
			{"type":"function_call","call_id":"unfinished","status":"incomplete"},
			{"type":"function_call","call_id":"a","name":"second","arguments":"{}"}],
			"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":2}}`,
			llm.Response{
				Content: "AB", Thoughts: "First.Second.", Refusal: &llm.Refusal{Category: "refusal", Explanation: "Not allowed."},
				StopReason: "completed", ToolCalls: []llm.ToolCall{{ID: "z", Name: "first", Arguments: `{"k": 3}`}, {ID: "a", Name: "second", Arguments: "{}"}},
				Usage: llm.Usage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13, CacheReadTokens: 5, CacheWriteTokens: 2},
			}},
		{"budget exhausted", "incomplete", `"incomplete_details":{"reason":"max_output_tokens"},
			"output":[{"type":"function_call","call_id":"partial","name":"lookup","arguments":"{","status":"incomplete"}]`,
			llm.Response{StopReason: "max_output_tokens"}},
		{"only explicitly finished calls", "incomplete", `"incomplete_details":{"reason":"content_filter"},
			"output":[{"type":"function_call","call_id":"pending","name":"lookup","arguments":"{}"},
				{"type":"function_call","call_id":"done","name":"lookup","arguments":"{}","status":"completed"}]`,
			llm.Response{StopReason: "content_filter", ToolCalls: []llm.ToolCall{{ID: "done", Name: "lookup", Arguments: "{}"}}}},
		{"unspecified reason", "incomplete", `"output":[]`, llm.Response{StopReason: "incomplete"}},
	} {
		for _, m := range []rsMode{rsREST, rsStream, rsDrain} {
			t.Run(tc.name+"/"+string(m), func(t *testing.T) {
				doc := `{"status":"` + tc.status + `",` + tc.fields + `}`
				s := rsBackend(t, m.terminal(tc.status, doc))
				got := *m.run(t, s.provider(128, m.settings(), nil), llm.Request{}).ok(t)
				got.ProviderExtra = nil // exercised by subsequent POSTs, not by inspecting receipt internals
				got.ToolCalls = append([]llm.ToolCall(nil), got.ToolCalls...)
				rsEqual(t, "local projection", got, tc.want)
			})
		}
	}
}

func TestResponsesInvalidTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, status, doc string
		why               []string
	}{
		{"failed", "failed", `{"status":"failed","error":{"code":429,"message":"try later"}}`, []string{"429", "try later"}},
		{"cancelled", "cancelled", rsDoc("cancelled"), []string{"cancelled"}},
		{"malformed item", "completed", rsDoc("completed", `"not an item"`), []string{"decode output item"}},
		{"no canonical ID", "incomplete", rsDoc("incomplete", `{"type":"function_call","id":"fc_only"}`), []string{"fc_only", "missing call_id"}},
		{"no function name", "completed", rsDoc("completed", `{"type":"function_call","call_id":"c"}`), []string{"c", "name"}},
		{"duplicate even if unfinished", "completed", rsDoc("completed", `{"type":"function_call","call_id":"same","name":"lookup"}`,
			`{"type":"function_call","call_id":"same","status":"incomplete"}`), []string{"duplicate call_id", "same"}},
	} {
		for _, m := range []rsMode{rsREST, rsStream} {
			t.Run(tc.name+"/"+string(m), func(t *testing.T) {
				s := rsBackend(t, m.terminal(tc.status, tc.doc))
				m.run(t, s.provider(128, nil, nil), llm.Request{}).fails(t, nil, tc.why...)
			})
		}
	}
	for _, status := range []string{"queued", "in_progress", "unknown"} {
		s := rsBackend(t, rsJSON(200, rsDoc(status)))
		rsREST.run(t, s.provider(128, nil, nil), llm.Request{}).fails(t, nil, status)
	}
}

func TestResponsesHandshake(t *testing.T) {
	// A success-shaped body doesn't override the HTTP status. Conversely,
	// HTTP success with a JSON body isn't an accepted SSE stream.
	s := rsBackend(t, rsJSON(400, rsOK), rsJSON(400, rsOK), rsJSON(200, rsOK))
	p := s.provider(128, nil, nil)
	rsREST.run(t, p, llm.Request{}).fails(t, nil)
	for range 2 {
		stream, err := p.Stream(t.Context(), llm.Request{})
		if stream != nil {
			stream.Close()
			t.Fatal("rejected handshake returned a stream")
		}
		rsError(t, err)
	}
}

// SSE framing and the boundary between provisional and committed output.

func TestResponsesSSEFraming(t *testing.T) {
	text := strings.Repeat("x", 96<<10)
	doc := rsDoc("completed", `{"type":"message","content":[{"type":"output_text","text":"`+text+`"}]}`)
	short := "\uFEFFdata: {\"type\":\"response.reasoning_summary_text.delta\",\r\ndata: \"delta\":\"thought\"}\r\n\r\n" +
		": keepalive\rid: 7\r\rdata:\n\nevent: ignored-name\ndata: " + rsPreview + "\n\n"
	body := io.MultiReader(iotest.OneByteReader(strings.NewReader(short)), strings.NewReader("data: "+rsEnd("completed", doc)+"\r\r"))
	s := rsBackend(t, rsReply{200, "text/event-stream; charset=utf-8", body})
	r := rsStream.run(t, s.provider(128, nil, nil), llm.Request{})
	rsEqual(t, "terminal text", r.ok(t).Content, text)
	rsEqual(t, "dispatched events", r.events, []llm.StreamEvent{{DeltaThoughts: "thought"}, {DeltaText: "preview"}})
}

func TestResponsesSSEPhysicalLineBound(t *testing.T) {
	// A long but otherwise ignorable comment, followed by a valid terminal.
	// Neither a JSON error nor ordinary EOF should satisfy this test.
	parts := []io.Reader{strings.NewReader(":")}
	block := strings.Repeat("x", 64<<10)
	for n := 0; n < 32<<20; n += len(block) {
		parts = append(parts, strings.NewReader(block))
	}
	parts = append(parts, strings.NewReader("\n\ndata: "+rsEnd("completed", rsOK)+"\n\n"))
	s := rsBackend(t, rsReply{200, "text/event-stream", io.MultiReader(parts...)})
	rsStream.run(t, s.provider(128, nil, nil), llm.Request{}).fails(t, nil, "stream read")
}

func TestResponsesTruncatedStream(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"EOF", ""}, {"DONE", "data: [DONE]\n\n"}, {"undispatched terminal", "data: " + rsEnd("completed", rsOK) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rsBackend(t, rsReply{200, "text/event-stream", strings.NewReader("data: " + rsPreview + "\n\n" + tc.tail)})
			rsStream.run(t, s.provider(128, nil, nil), llm.Request{}).
				fails(t, []llm.StreamEvent{{DeltaText: "preview"}}, "ended before terminal response event")
		})
	}
}

func TestResponsesInvalidFrame(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		why         []string
	}{
		{"malformed JSON", `{`, []string{"decode SSE event"}},
		{"no snapshot", `{"type":"response.completed"}`, []string{"completed"}},
		{"inconsistent status", rsEnd("completed", rsDoc("incomplete")), []string{"completed", "incomplete"}},
		{"plain error", `{"type":"error","code":400,"param":"model","message":"unavailable"}`, []string{"400", "model", "unavailable"}},
		{"absent index", `{"type":"response.function_call_arguments.delta","item_id":"fc","delta":"{"}`, []string{"output_index"}},
		{"negative index", rsItem("added", -1, `{"type":"function_call"}`), []string{"output_index"}},
	} {
		t.Run(tc.name, func(t *testing.T) { rsPlay(t, tc.frame).fails(t, nil, tc.why...) })
	}
}

func rsStart(id, name string) llm.StreamEvent {
	return llm.StreamEvent{ToolCallStart: &llm.ToolCallStart{ID: id, Name: name}}
}

func rsArgs(id, fragment string) llm.StreamEvent {
	return llm.StreamEvent{ToolCallDelta: &llm.ToolCallDelta{ID: id, ArgumentsDelta: fragment}}
}

func TestResponsesInterleavedCalls(t *testing.T) {
	const a = `{"type":"function_call","id":"fa","call_id":"a","name":"A"}`
	const b = `{"type":"function_call","id":"fb","call_id":"b","name":"B","arguments":"{}"}`
	doc := rsDoc("completed", `{"type":"function_call","id":"fa","call_id":"a","name":"A","arguments":"{\"n\":1}"}`, b,
		`{"type":"function_call","call_id":"c","name":"C","arguments":"{}"}`)
	r := rsPlay(t,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fb","delta":"{"}`,
		rsItem("added", 1, `{"type":"function_call","id":"fb","call_id":"b","name":"B"}`),
		rsItem("added", 0, a),
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fa","delta":"{"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fb","delta":"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fa","arguments":"{\"n\":1}"}`,
		rsItem("done", 1, b),
		rsEnd("completed", doc),
	)
	rsEqual(t, "executable calls in output order", r.ok(t).ToolCalls, []llm.ToolCall{
		{ID: "a", Name: "A", Arguments: `{"n":1}`}, {ID: "b", Name: "B", Arguments: "{}"}, {ID: "c", Name: "C", Arguments: "{}"},
	})
	rsEqual(t, "live events in identification order, no repeated snapshot bytes", r.events, []llm.StreamEvent{
		rsStart("b", "B"), rsArgs("b", "{"), rsStart("a", "A"), rsArgs("a", "{"),
		rsArgs("b", "}"), rsArgs("a", `"n":1}`), rsStart("c", "C"), rsArgs("c", "{}"),
	})
}

func TestResponsesCallReconciliation(t *testing.T) {
	start := rsItem("added", 1, `{"type":"function_call","id":"fc","call_id":"c","name":"lookup"}`)
	delivered := []llm.StreamEvent{rsStart("c", "lookup")}
	// This first terminal item could generate two events. Neither is
	// publishable if a later item invalidates the frame.
	const candidate = `{"type":"function_call","call_id":"new","name":"lookup","arguments":"{}","status":"completed"}`
	const conflict = `{"type":"function_call","id":"fc","call_id":"changed","status":"incomplete"}`
	for _, tc := range []struct {
		name, frame string
		why         []string
	}{
		{"changed item", `{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"other","delta":"x"}`, []string{"changed item ID"}},
		{"renamed", rsItem("done", 1, `{"type":"function_call","id":"fc","call_id":"c","name":"other"}`), []string{"changed function name"}},
		{"ID claimed twice", rsItem("added", 0, `{"type":"function_call","call_id":"c","name":"lookup"}`), []string{"duplicate call_id"}},
		{"omitted on completion", rsEnd("completed", rsDoc("completed")), []string{"missing from terminal output"}},
		{"unfinished identity still matters", rsEnd("incomplete", rsDoc("incomplete", candidate, conflict)), []string{"fc", "changed call_id"}},
		{"validate whole snapshot before reconciliation", rsEnd("incomplete", rsDoc("incomplete", candidate, conflict,
			`{"type":"function_call","id":"bad"}`)), []string{"bad", "missing call_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) { rsPlay(t, start, tc.frame).fails(t, delivered, tc.why...) })
	}
	r := rsPlay(t, start, rsEnd("incomplete", rsDoc("incomplete")))
	rsEqual(t, "omitted on incomplete: no executable call", len(r.ok(t).ToolCalls), 0)
	rsEqual(t, "already delivered", r.events, delivered)
}

func TestResponsesReasoningPositions(t *testing.T) {
	done := func(n int, id string) string { return rsItem("done", n, `{"type":"reasoning","id":"`+id+`"}`) }
	end := rsEnd("completed", rsDoc("completed"))
	for _, tc := range []struct {
		name, why string
		frames    []string
	}{
		{"no identity", "reasoning ID", []string{done(0, "")}},
		{"two IDs at one slot", "changed item ID", []string{done(0, "r"), done(0, "s")}},
		{"one ID at two slots", "duplicate", []string{done(0, "r"), done(1, "r"), end}},
		{"unfillable gap", "missing output item", []string{done(1, "r"), end}},
		{"terminal ID at wrong index", "output index", []string{done(1, "r"), rsEnd("completed", rsDoc("completed", `{"type":"reasoning","id":"r"}`))}},
	} {
		t.Run(tc.name, func(t *testing.T) { rsPlay(t, tc.frames...).fails(t, nil, tc.why) })
	}
}

// History evidence must survive the application's event store. Don't
// construct a receipt in a test: obtain one from an actual generation.
func rsSaved(t *testing.T, r *llm.Response) llm.Message {
	t.Helper()
	data, err := event.Marshal(&event.AssistantEvent{ProviderExtra: r.ProviderExtra})
	if err != nil {
		t.Fatal(err)
	}
	e, err := event.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return llm.Message{Role: llm.RoleAssistant, Content: r.Content, ToolCalls: r.ToolCalls,
		ProviderExtra: e.(*event.AssistantEvent).ProviderExtra}
}

func TestResponsesPersistedContinuation(t *testing.T) {
	const first = `{"type":"reasoning","id":"r0","content":[{"type":"reasoning_text","text":"Think."}]}`
	const second = `{"type":"reasoning","id":"r2","summary":[{"type":"summary_text","text":"Next."}]}`
	const call = `{"type":"function_call","id":"fc","call_id":"c","name":"lookup","arguments":"{}"}`
	const doc = `{"status":"completed","id":"stored","output":[` + rsMessage + `,{"type":"reasoning","id":"r2"},` + call + `]}`
	s := rsBackend(t, rsSSE(
		`{"type":"response.reasoning_text.delta","delta":"Think."}`,
		rsItem("done", 0, first), rsItem("done", 2, second),
		rsItem("added", 3, `{"type":"function_call","id":"fc","call_id":"c","name":"lookup"}`),
		rsEnd("completed", doc),
	), rsJSON(200, rsOK), rsJSON(200, rsOK))

	run := rsStream.run(t, s.provider(128, nil, nil), rsHistory(rsUser("hi")))
	out := run.ok(t)
	rsEqual(t, "omitted and stripped reasoning in position order", out.Thoughts, "Think.Next.")
	rsEqual(t, "live events", run.events, []llm.StreamEvent{{DeltaThoughts: "Think."}, rsStart("c", "lookup"), rsArgs("c", "{}")})
	rsEqual(t, "call for execution", out.ToolCalls, []llm.ToolCall{{ID: "c", Name: "lookup", Arguments: "{}"}})

	p := s.provider(128, nil, nil) // no provider-local memory of the first turn
	req := rsHistory(rsUser("hi"), rsSaved(t, out), llm.Message{Role: llm.RoleToolResult, ToolCallID: "c", Content: "42"})
	req.SystemPrompt = "Use the result."
	rsREST.run(t, p, req).ok(t)
	const result = `{"type":"function_call_output","call_id":"c","output":"42"}`
	s.history(0, true, "", rsHi)
	s.history(1, true, "stored", result)
	rsEqual(t, "continuation instructions", s.posts[1].json["instructions"], req.SystemPrompt)

	req.Messages[0] = rsUser("edited")
	rsREST.run(t, p, req).ok(t)
	s.history(2, true, "", `{"type":"message","role":"user","content":"edited"}`,
		`{"type":"message","role":"assistant","content":"ok"}`,
		`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}`, result)
}

func TestResponsesContinuationBelongsToItsBranch(t *testing.T) {
	// Identical local messages, different protocol histories. A child of L
	// spliced after R can supply local text but cannot replace R's prefix.
	const left = `{"id":"L","status":"completed","output":[{"type":"reasoning","encrypted_content":"left"}]}`
	const right = `{"id":"R","status":"completed","output":[{"type":"reasoning","encrypted_content":"right"}]}`
	s := rsBackend(t, rsJSON(200, left), rsJSON(200, rsOK), rsJSON(200, right), rsJSON(200, rsOK))
	p := s.provider(128, nil, nil)
	hi, next := rsUser("hi"), rsUser("next")
	l := rsSaved(t, rsREST.run(t, p, rsHistory(hi)).ok(t))
	child := rsSaved(t, rsREST.run(t, p, rsHistory(hi, l, next)).ok(t))
	r := rsSaved(t, rsREST.run(t, p, rsHistory(hi)).ok(t))
	rsREST.run(t, p, rsHistory(hi, r, next, child, next)).ok(t)
	s.history(1, true, "L", rsNext)
	s.history(2, true, "", rsHi)
	s.history(3, true, "R", rsNext, `{"type":"message","role":"assistant","content":"ok"}`, rsNext)
}

// Recovery changes how a prefix is sent, never what conversation is sent.

func TestResponsesFallbackSequence(t *testing.T) {
	s := rsBackend(t,
		rsJSON(200, rsOK),
		rsJSON(404, `{"error":{"code":"previous_response_not_found"}}`),
		rsJSON(400, `{"error":{"param":"store","code":"unsupported_parameter"}}`),
		rsJSON(200, rsOK), rsJSON(200, rsOK),
	)
	p := s.provider(128, nil, nil)
	a := rsSaved(t, rsREST.run(t, p, rsHistory(rsUser("hi"))).ok(t))
	req := rsHistory(rsUser("hi"), a, rsUser("next"))
	rsREST.run(t, p, req).ok(t)
	rsREST.run(t, p, req).ok(t)
	s.history(0, true, "", rsHi)
	s.history(1, true, "r", rsNext)
	s.history(2, true, "", rsHi, rsMessage, rsNext)  // missing ID: still prefer storage
	s.history(3, false, "", rsHi, rsMessage, rsNext) // rejected storage: replace once
	s.history(4, false, "", rsHi, rsMessage, rsNext) // same evidence on a later turn
}

func TestResponsesPublicationBoundary(t *testing.T) {
	full := []string{rsHi, rsMessage, rsNext}
	for _, cause := range []struct {
		name, seed, frame, diagnostic string
		previous                      string
		input                         []string
		keepStorage                   bool
	}{
		{"missing ID", rsOK, `{"type":"error","param":"previous_response_id","code":"not_found"}`, "previous_response_id", "r", []string{rsNext}, true},
		{"unsupported storage", rsDoc("completed", rsMessage), rsEnd("failed",
			`{"status":"failed","error":{"param":"store","code":"unsupported_parameter"}}`), "store", "", full, false},
	} {
		for _, delivery := range []struct {
			name      string
			mode      rsMode
			before    []string
			published bool
		}{
			{"before delivery", rsStream, nil, false},
			{"after delivery", rsStream, []string{rsPreview}, true},
			{"internally drained", rsDrain, []string{rsPreview}, false},
		} {
			t.Run(cause.name+"/"+delivery.name, func(t *testing.T) {
				m := delivery.mode
				s := rsBackend(t, m.terminal("completed", cause.seed),
					rsSSE(append(delivery.before, cause.frame)...), m.terminal("completed", rsOK))
				p := s.provider(128, m.settings(), nil)
				a := rsSaved(t, m.run(t, p, rsHistory(rsUser("hi"))).ok(t))
				req := rsHistory(rsUser("hi"), a, rsUser("next"))
				r := m.run(t, p, req)
				if delivery.published {
					r.fails(t, []llm.StreamEvent{{DeltaText: "preview"}}, cause.diagnostic)
					rsEqual(t, "no replacement after publication", len(s.posts), 2)
					r = m.run(t, p, req) // new operation must still honor the diagnosis
				}
				rsEqual(t, "reply", r.ok(t).Content, "ok")
				rsEqual(t, "no stale preview", len(r.events), 0)
				s.history(1, true, cause.previous, cause.input...)
				s.history(2, cause.keepStorage, "", full...)
			})
		}
	}
}

func TestResponsesUnrelatedRejectionsAreNotEvidence(t *testing.T) {
	s := rsBackend(t, rsJSON(200, rsOK),
		rsJSON(404, `{"error":{"param":"model","code":"not_found","message":"model not found"}}`),
		rsJSON(400, `{"error":{"message":"tool not supported when store is true"}}`),
		rsJSON(401, `{"error":{"param":"store","code":"unsupported_parameter"}}`),
		rsJSON(200, rsOK))
	p := s.provider(128, nil, nil)
	a := rsSaved(t, rsREST.run(t, p, rsHistory(rsUser("hi"))).ok(t))
	req := rsHistory(rsUser("hi"), a, rsUser("next"))
	for n := 1; n <= 3; n++ {
		rsREST.run(t, p, req).fails(t, nil)
		rsEqual(t, "no transparent retry", len(s.posts), n+1)
		s.history(n, true, "r", rsNext)
	}
	rsREST.run(t, p, req).ok(t)
	s.history(4, true, "r", rsNext)
}
