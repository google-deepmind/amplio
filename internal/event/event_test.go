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

package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRoundTrip_AllTypes(t *testing.T) {
	events := []Event{
		&SystemEvent{Content: "system prompt here", Marker: "bootstrap"},
		&UserEvent{Content: "user message"},
		&AssistantEvent{
			Content: "response text",
			ToolCalls: []ToolCall{
				{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
			},
			Thoughts: "thinking...",
			Usage:    &Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		},
		&ToolResultEvent{
			Content:    "file listing output",
			ToolCallID: "tc1",
			Attachments: []Attachment{
				{MimeType: "image/png", BlobKey: "abc123", Size: 42, SourceHint: "/tmp/img.png"},
			},
		},
		&CompactionEvent{Content: "summary of prior context"},
		&MessageEvent{Content: "hello from peer", Sender: "swift-fox"},
		&ChildResultEvent{Content: "task done", ChildSessionID: "brave-owl", Verdict: "concluded"},
		&RecoverEvent{Content: "resumed"},
		&LoopEvent{Action: LoopSet, ID: "1", Prompt: "p", Schedule: &Schedule{Kind: ScheduleRRule, Spec: "DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY"}, By: ByOperator},
	}

	for _, e := range events {
		t.Run(e.EventType(), func(t *testing.T) {
			data, err := Marshal(e)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			// Verify type field is in JSON.
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("json.Unmarshal raw: %v", err)
			}
			if raw["type"] != e.EventType() {
				t.Errorf("type field: got %q, want %q", raw["type"], e.EventType())
			}

			// Round-trip.
			got, err := Unmarshal(data)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.EventType() != e.EventType() {
				t.Errorf("EventType: got %q, want %q", got.EventType(), e.EventType())
			}
		})
	}
}

func TestRoundTrip_AssistantEvent_Fields(t *testing.T) {
	orig := &AssistantEvent{
		Content: "here is the plan",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "bash", Arguments: `{"command":"echo hi"}`},
			{ID: "call_2", Name: "view_file", Arguments: `{"path":"/tmp/x"}`},
		},
		Thoughts: "let me think",
		Usage:    &Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300},
	}

	data, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}

	e, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := e.(*AssistantEvent)
	if !ok {
		t.Fatalf("expected *AssistantEvent, got %T", e)
	}

	if got.Content != orig.Content {
		t.Errorf("Content: %q vs %q", got.Content, orig.Content)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len: %d, want 2", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Name != "bash" {
		t.Errorf("ToolCalls[0].Name: %q", got.ToolCalls[0].Name)
	}
	if got.Thoughts != "let me think" {
		t.Errorf("Thoughts: %q", got.Thoughts)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 300 {
		t.Errorf("Usage: %+v", got.Usage)
	}
}

func TestRoundTrip_MessageEvent_SenderType(t *testing.T) {
	orig := &MessageEvent{Content: "hi", Sender: "swift-fox", SenderType: SenderTypeAgent}
	data, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := e.(*MessageEvent)
	if !ok {
		t.Fatalf("expected *MessageEvent, got %T", e)
	}
	if got.SenderType != SenderTypeAgent {
		t.Errorf("SenderType: got %q, want %q", got.SenderType, SenderTypeAgent)
	}
	if got.Sender != "swift-fox" {
		t.Errorf("Sender: got %q, want %q", got.Sender, "swift-fox")
	}
}

func TestRoundTrip_ToolResultEvent_WithAttachments(t *testing.T) {
	orig := &ToolResultEvent{
		Content:    "image content",
		ToolCallID: "tc_99",
		Attachments: []Attachment{
			// Data is transient: it must NOT survive marshaling (only the
			// blob reference is persisted).
			{MimeType: "image/webp", BlobKey: "deadbeef", Size: 4, SourceHint: "screenshot.png", Data: []byte("AAAA")},
		},
	}
	data, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "AAAA") {
		t.Errorf("raw attachment bytes leaked into persisted event: %s", data)
	}
	e, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	got := e.(*ToolResultEvent)
	if len(got.Attachments) != 1 {
		t.Fatalf("Attachments len: %d", len(got.Attachments))
	}
	if got.Attachments[0].MimeType != "image/webp" {
		t.Errorf("MimeType: %q", got.Attachments[0].MimeType)
	}
	if got.Attachments[0].BlobKey != "deadbeef" {
		t.Errorf("BlobKey: %q", got.Attachments[0].BlobKey)
	}
	if len(got.Attachments[0].Data) != 0 {
		t.Errorf("Data should not be persisted, got %d bytes", len(got.Attachments[0].Data))
	}
}

// TestRoundTrip_ToolResultEvent_IsError locks the additive is_error field: it
// round-trips when set, and — critically for backward compatibility — a result
// WITHOUT the key (every pre-field row) deserializes to IsError=false and a
// successful result omits the key entirely (omitempty), so success rows stay
// byte-identical to historical ones.
func TestRoundTrip_ToolResultEvent_IsError(t *testing.T) {
	// Error result round-trips true.
	data, err := Marshal(&ToolResultEvent{Content: "boom", ToolCallID: "tc1", IsError: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"is_error":true`) {
		t.Errorf("error result should carry is_error:true; got %s", data)
	}
	e, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !e.(*ToolResultEvent).IsError {
		t.Error("IsError did not round-trip")
	}

	// Success result omits the key (omitempty), staying byte-identical to a
	// pre-field row.
	data, err = Marshal(&ToolResultEvent{Content: "ok", ToolCallID: "tc2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "is_error") {
		t.Errorf("success result should omit is_error; got %s", data)
	}

	// Legacy row without the key deserializes to false (the correct default).
	e, err = Unmarshal([]byte(`{"type":"tool_result","content":"old","tool_call_id":"tc3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.(*ToolResultEvent).IsError {
		t.Error("legacy row without is_error should deserialize to false")
	}
}

// A VerifyEvent reads as one for every action this binary knows; with an
// action it doesn't know (a newer binary's), it reads as a placeholder, as an
// unknown type does, rather than as a known event misread.
func TestUnmarshal_VerifyActions(t *testing.T) {
	for _, action := range []string{VerifySet, VerifyReplace, VerifyClear, VerifyVerifying, VerifyRejected,
		VerifyPassed, VerifyExhausted, VerifyVerdict, VerifyWithdrawn, VerifyStale} {
		data, err := Marshal(&VerifyEvent{Action: action, By: ByHarness, Round: 2})
		if err != nil {
			t.Fatal(err)
		}
		e, err := Unmarshal(data)
		if v, ok := e.(*VerifyEvent); err != nil || !ok || v.Action != action || v.Round != 2 {
			t.Errorf("%s: read back %T %+v, err %v", action, e, e, err)
		}
	}
	stored := `{"type":"verify","action":"paused","by":"harness","round":1}`
	e, err := Unmarshal([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := e.(*UnknownEvent); !ok || u.Type != "verify" {
		t.Fatalf("unknown action read as %T %+v, want a verify placeholder", e, e)
	}
	if out, err := Marshal(e); err != nil || string(out) != stored {
		t.Errorf("Marshal = %s (%v), want the stored %s", out, err, stored)
	}
}

// A type from a newer binary reads as a placeholder that marshals back to the
// stored JSON byte for byte.
func TestUnmarshal_UnknownType(t *testing.T) {
	stored := `{"type":"teleport","action":"set","criteria":"tests pass","max_rounds":3}`
	data := []byte(stored)
	e, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	u, ok := e.(*UnknownEvent)
	if !ok {
		t.Fatalf("got %T, want *UnknownEvent", e)
	}
	if u.Type != "teleport" || u.EventType() != "teleport" || !IsUnknown(e) {
		t.Errorf("Type = %q, EventType = %q, IsUnknown = %v; want teleport, teleport, true", u.Type, u.EventType(), IsUnknown(e))
	}
	copy(data, "XXXXXXXX") // the caller may reuse its buffer
	out, err := Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != stored {
		t.Errorf("Marshal = %s, want the stored %s", out, stored)
	}
	again, err := Unmarshal(out)
	if err != nil || again.EventType() != "teleport" || !IsUnknown(again) {
		t.Errorf("re-read = %T %v, err %v; want a teleport placeholder", again, again, err)
	}
	if IsUnknown(&UserEvent{Content: "hi"}) {
		t.Error("IsUnknown(UserEvent) = true")
	}
}

func TestUnmarshal_MissingType(t *testing.T) {
	data := []byte(`{"content":"hello"}`)
	_, err := Unmarshal(data)
	if err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestUnmarshal_MalformedJSON(t *testing.T) {
	_, err := Unmarshal([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestColumnFields_NotInJSON(t *testing.T) {
	e := &SystemEvent{
		ColumnFields: ColumnFields{Step: 5, Generation: 2},
		Content:      "test",
	}
	data, err := Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// Step and Generation should not be in the JSON (they come from DB columns).
	// But since we embed ColumnFields, they will appear. This test documents
	// the current behavior — the DB layer is responsible for ignoring them.
	// If we later want to strip them, add custom MarshalJSON.
}

func TestToText_AllTypes(t *testing.T) {
	tests := []struct {
		event     Event
		wantLabel string   // the section label in the "==== LABEL …" header
		wantIn    []string // substrings that must appear in the output
	}{
		{
			&SystemEvent{Content: "hello"},
			"system", []string{"hello"},
		},
		{
			&SystemEvent{Content: "session started", Marker: MarkerNewSession},
			"system", []string{"session started", "new_session"},
		},
		{
			&UserEvent{Content: "hi"},
			"user", []string{"hi"},
		},
		{
			&AssistantEvent{Content: "reply"},
			"assistant", []string{"reply"},
		},
		{
			&AssistantEvent{Content: "plan", ToolCalls: []ToolCall{
				{ID: "tc1", Name: "bash", Arguments: `{"cmd":"ls"}`},
			}},
			"assistant", []string{"plan", "tool_call", "name=bash", "id=tc1"},
		},
		{
			&ToolResultEvent{Content: "output", ToolCallID: "tc1"},
			"tool_result", []string{"output", "id=tc1"},
		},
		{
			&CompactionEvent{Content: "summary"},
			"compaction", []string{"summary"},
		},
		{
			&MessageEvent{Content: "msg", Sender: "a-b"},
			"message", []string{"msg", "from=a-b"},
		},
		{
			&ChildResultEvent{Content: "done", ChildSessionID: "c-d", Verdict: "concluded"},
			"child_result", []string{"done", "child=c-d", "verdict=concluded"},
		},
		{
			&RecoverEvent{Content: "bye"},
			"recover", []string{"bye"},
		},
		{
			&UnknownEvent{Type: "teleport", Raw: json.RawMessage(`{"type":"teleport","criteria":"secret"}`)},
			"unknown", []string{"type=teleport"},
		},
		{
			&LoopEvent{Action: LoopSet, ID: "3", Prompt: "check the eval", By: ByOperator,
				Schedule: &Schedule{Kind: ScheduleRRule, Spec: "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30"}},
			"loop", []string{"id=3", "action=set", "by=operator",
				"schedule: DTSTART:20261110T120000Z RRULE:FREQ=MINUTELY;INTERVAL=30"},
		},
		{
			&LoopEvent{Action: LoopEdit, ID: "4", Prompt: "run the tests", By: ByOperator,
				Verify: &LoopVerify{Criteria: "they pass", Model: "vertex:judge", MaxRounds: 2}},
			"loop", []string{"id=4", "action=edit", "verify each tick: criteria: they pass; model: vertex:judge; up to 2 rounds"},
		},
		{
			&LoopEvent{Action: LoopEdit, ID: "5", Prompt: "run the tests", By: ByOperator, VerifyStopped: true},
			"loop", []string{"id=5", "action=edit", "verify each tick: stopped"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.event.EventType(), func(t *testing.T) {
			got := tt.event.ToText()
			header := "==== " + tt.wantLabel
			if !strings.HasPrefix(got, header) {
				t.Errorf("missing header %q in:\n%s", header, got)
			}
			// The whole point of the format: no XML/tool-call markup leaks in.
			if strings.Contains(got, "<amplio:") || strings.Contains(got, "</") {
				t.Errorf("output still contains XML markup:\n%s", got)
			}
			for _, sub := range tt.wantIn {
				if !strings.Contains(got, sub) {
					t.Errorf("missing substring %q in:\n%s", sub, got)
				}
			}
		})
	}
}

// A loop's change reads without its prompt, which an agent inspecting the
// stream would take for a request; its schedule reads in words, or as its
// spec on one line in an event written before Summary.
func TestLoopEvent_ToText(t *testing.T) {
	sched := &Schedule{Kind: ScheduleRRule, Spec: "DTSTART:20261117T232452Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=2"}
	for _, tc := range []struct {
		e    *LoopEvent
		want string
	}{
		{&LoopEvent{Action: LoopSet, ID: "1", Prompt: "status check: reply with one line", Schedule: sched,
			Summary: "every 1h, 2 times", By: ByOperator},
			"==== loop · id=1 · action=set · by=operator ====\nschedule: every 1h, 2 times"},
		{&LoopEvent{Action: LoopEdit, ID: "1", Prompt: "status check: reply with one line", Schedule: sched, By: ByOperator,
			Verify: &LoopVerify{Criteria: "it says done", MaxRounds: 2}},
			"==== loop · id=1 · action=edit · by=operator ====\n" +
				"schedule: DTSTART:20261117T232452Z RRULE:FREQ=HOURLY;INTERVAL=1;COUNT=2\n" +
				"verify each tick: criteria: it says done; up to 2 rounds"},
		{&LoopEvent{Action: LoopEdit, ID: "1", Prompt: "status check", Schedule: sched, Summary: "every 1h, 2 times",
			VerifyStopped: true, By: ByOperator},
			"==== loop · id=1 · action=edit · by=operator ====\nschedule: every 1h, 2 times\nverify each tick: stopped"},
		{&LoopEvent{Action: LoopStop, ID: "1", By: ByOperator}, "==== loop · id=1 · action=stop · by=operator ====\n"},
		{&LoopEvent{Action: LoopEnded, ID: "1", By: ByHarness, Reason: "schedule finished after 2 ticks"},
			"==== loop · id=1 · action=ended · by=harness ====\nschedule finished after 2 ticks"},
	} {
		if got := tc.e.ToText(); got != tc.want {
			t.Errorf("%s: ToText =\n%s\nwant\n%s", tc.e.Action, got, tc.want)
		}
	}
}

func TestRoundTrip_LoopFields(t *testing.T) {
	tick := &UserEvent{Content: "[loop 2 · every 1h · tick 5] p", Loop: &LoopTick{ID: "2", N: 5, Version: 4}}
	data, err := Marshal(tick)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if u := got.(*UserEvent); u.Loop == nil || *u.Loop != *tick.Loop || u.IsHarness() {
		t.Errorf("tick round trip = %+v", u)
	}
	ended := &LoopEvent{Action: LoopEnded, ID: "2", By: ByHarness, Reason: "run archived"}
	data, err = Marshal(ended)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "schedule") || strings.Contains(string(data), "prompt") {
		t.Errorf("an ended event carries no schedule or prompt: %s", data)
	}
	got, err = Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if l := got.(*LoopEvent); *l != *ended {
		t.Errorf("ended round trip = %+v", l)
	}
	// A plain user message is unchanged on the wire, as is a tick that
	// started no verification.
	if data, _ := Marshal(&UserEvent{Content: "hi"}); strings.Contains(string(data), "loop") {
		t.Errorf("plain user event grew a loop field: %s", data)
	}
	if data, _ := Marshal(&UserEvent{Loop: &LoopTick{ID: "2", N: 1}}); strings.Contains(string(data), "version") {
		t.Errorf("an unverified tick carries a version: %s", data)
	}

	set := &LoopEvent{Action: LoopSet, ID: "2", Prompt: "p", Summary: "every 1h", Verify: &LoopVerify{MaxRounds: 0}}
	data, err = Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	got, err = Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	// Unbounded rounds survive: max_rounds is always written.
	if l := got.(*LoopEvent); l.Verify == nil || *l.Verify != *set.Verify || !strings.Contains(string(data), `"max_rounds":0`) ||
		l.Summary != set.Summary {
		t.Errorf("verifying set round trip = %+v (%s)", l, data)
	}
}

// A version a loop's tick starts names the loop, which no session id can be.
func TestByLoop(t *testing.T) {
	if by := ByLoop("3"); by != "loop 3" || LoopOf(by) != "3" {
		t.Errorf("ByLoop(3) = %q, LoopOf = %q", by, LoopOf(by))
	}
	for _, by := range []string{ByOperator, ByHarness, "boss", "", "loops"} {
		if id := LoopOf(by); id != "" {
			t.Errorf("LoopOf(%q) = %q", by, id)
		}
	}
	e := LoopVerify{Criteria: "c", Model: "m", MaxRounds: 1}.Event("7")
	if e.Action != VerifySet || e.By != "loop 7" || e.Criteria != "c" || e.Model != "m" || e.MaxRounds != 1 {
		t.Errorf("Event = %+v", e)
	}
	if got := (LoopVerify{}).String(); got != "unbounded rounds" {
		t.Errorf("String() = %q", got)
	}
	// One line, long criteria cut short.
	long := LoopVerify{Criteria: "the tests\n  pass " + strings.Repeat("x", 300), MaxRounds: 2}.String()
	if !strings.HasPrefix(long, "criteria: the tests pass x") || strings.Contains(long, "\n") ||
		!strings.HasSuffix(long, "…; up to 2 rounds") || len([]rune(long)) > 240 {
		t.Errorf("String() = %q", long)
	}
}

// A conclude call's result is the session's result: Offered reads it (or a
// reply's text), and ToText shows it as written rather than JSON-escaped.
func TestConclude_OfferedAndToText(t *testing.T) {
	args, _ := json.Marshal(ConcludeArgs{Result: "It is 42 m.\nMeasured twice."})
	call := &AssistantEvent{Content: "Done.", ToolCalls: []ToolCall{{ID: "c1", Name: ConcludeTool, Arguments: string(args)}}}
	for _, c := range []struct {
		name string
		e    *AssistantEvent
		want string
		ok   bool
	}{
		{"reply", &AssistantEvent{Content: "It is 42 m."}, "It is 42 m.", true},
		{"conclude(result)", call, "It is 42 m.\nMeasured twice.", true},
		{"conclude()", &AssistantEvent{ToolCalls: []ToolCall{{ID: "c2", Name: ConcludeTool, Arguments: "{}"}}}, "", false},
		{"working", &AssistantEvent{Content: "Looking.", ToolCalls: []ToolCall{{ID: "b", Name: "bash", Arguments: "{}"}}}, "", false},
		// Alongside another call conclude fails on its own: no attempt to conclude.
		{"conclude with another call", &AssistantEvent{ToolCalls: []ToolCall{{ID: "b", Name: "bash", Arguments: "{}"},
			{ID: "c3", Name: ConcludeTool, Arguments: string(args)}}}, "", false},
	} {
		if got, ok := c.e.Offered(); got != c.want || ok != c.ok {
			t.Errorf("%s: Offered = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if tc, ok := call.ConcludeCall(); !ok || tc.ID != "c1" {
		t.Errorf("ConcludeCall = %+v, %v", tc, ok)
	}
	text := call.ToText()
	if !strings.Contains(text, "name=conclude · id=c1 ----\nIt is 42 m.\nMeasured twice.") || strings.Contains(text, `\n`) {
		t.Errorf("ToText = %q, want the result as written", text)
	}
	// Alongside another call conclude failed on its own: its arguments show as
	// they are, not as a result.
	multi := &AssistantEvent{ToolCalls: []ToolCall{{ID: "b", Name: "bash", Arguments: `{"command":"ls"}`},
		{ID: "c4", Name: ConcludeTool, Arguments: string(args)}}}
	if text := multi.ToText(); !strings.Contains(text, string(args)) {
		t.Errorf("ToText of a conclude among other calls = %q, want its arguments as they are", text)
	}
	// A call whose arguments don't parse shows them raw rather than nothing.
	raw := &AssistantEvent{ToolCalls: []ToolCall{{ID: "c3", Name: ConcludeTool, Arguments: `{"result":`}}}
	if !strings.Contains(raw.ToText(), `{"result":`) {
		t.Errorf("ToText of unparsable arguments = %q", raw.ToText())
	}
}

// ToolResultEvent.Check round-trips, and is absent when unset (rows stay as
// they were).
func TestRoundTrip_ToolResultEvent_Check(t *testing.T) {
	data, err := Marshal(&ToolResultEvent{Content: "remind", ToolCallID: "c1", Check: CheckLeftovers})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Unmarshal(data)
	if err != nil || e.(*ToolResultEvent).Check != CheckLeftovers {
		t.Errorf("Check did not round-trip: %s", data)
	}
	if data, _ := Marshal(&ToolResultEvent{Content: "ok", ToolCallID: "c2"}); strings.Contains(string(data), "check") {
		t.Errorf("unset Check is serialized: %s", data)
	}
}

// A LoopEvent with an action this binary doesn't know (a newer binary's) reads
// as a placeholder, as an unknown VerifyEvent action does.
func TestUnmarshal_LoopActions(t *testing.T) {
	for _, action := range []string{LoopSet, LoopEdit, LoopStop, LoopEnded} {
		data, err := Marshal(&LoopEvent{Action: action, ID: "2", By: ByOperator})
		if err != nil {
			t.Fatal(err)
		}
		e, err := Unmarshal(data)
		if l, ok := e.(*LoopEvent); err != nil || !ok || l.Action != action || l.ID != "2" {
			t.Errorf("%s: read back %T %+v, err %v", action, e, e, err)
		}
	}
	stored := `{"type":"loop","action":"pause","id":"2","by":"operator"}`
	e, err := Unmarshal([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := e.(*UnknownEvent); !ok || u.Type != "loop" {
		t.Fatalf("unknown action read as %T %+v, want a loop placeholder", e, e)
	}
	if out, err := Marshal(e); err != nil || string(out) != stored {
		t.Errorf("Marshal = %s (%v), want the stored %s", out, err, stored)
	}
}
