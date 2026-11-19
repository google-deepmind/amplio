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

package db

import (
	"strings"
	"testing"
	"time"

	"amplio/internal/event"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		evt  event.Event
		want EventClass
	}{
		{"user input", &event.UserEvent{Content: "hi"}, ClassInput},
		{"agent message", &event.MessageEvent{Content: "x", SenderType: event.SenderTypeAgent}, ClassInput},
		// The behavior change: an environment notification ($AMPLIO_NOTIFY) now
		// revives a dormant recipient (was ClassNotice).
		{"environment notify", &event.MessageEvent{Content: "done", SenderType: event.SenderTypeEnvironment}, ClassInput},
		{"child concluded", &event.ChildResultEvent{Verdict: SessionConcluded}, ClassInput},
		{"child crashed", &event.ChildResultEvent{Verdict: SessionCrashed}, ClassNotice},
		{"recover", &event.RecoverEvent{}, ClassInput},
		{"verification change", &event.VerifyEvent{Action: event.VerifySet}, ClassNotice},
		{"candidate withdrawn", &event.VerifyEvent{Action: event.VerifyWithdrawn}, ClassNotice},
		{"verifier's verdict", event.VerifierOutcome("v", "FAIL: x", "", time.Time{}), ClassNotice},
		{"verifier stopped", event.VerifierOutcome("v", "replaced", SessionCancelled, time.Time{}), ClassNotice},
		{"self/system write", &event.AssistantEvent{Content: "thinking"}, ClassNotice},
		{"newer binary's event", &event.UnknownEvent{Type: "teleport"}, ClassNotice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.evt); got != tt.want {
				t.Errorf("Classify(%s) = %v, want %v", tt.name, got, tt.want)
			}
			if got := IsInput(tt.evt); got != (tt.want == ClassInput) {
				t.Errorf("IsInput(%s) = %v, want %v", tt.name, got, tt.want == ClassInput)
			}
		})
	}
}

func TestKnownEvents(t *testing.T) {
	recs := []EventRecord{
		{EventID: "a", Event: &event.UserEvent{Content: "hi"}},
		{EventID: "b", Event: &event.UnknownEvent{Type: "teleport"}},
		{EventID: "c", Event: &event.AssistantEvent{Content: "done"}},
	}
	var ids []string
	for _, r := range KnownEvents(recs) {
		ids = append(ids, r.EventID)
	}
	if got := strings.Join(ids, ","); got != "a,c" {
		t.Errorf("KnownEvents kept %q, want a,c", got)
	}
}

func TestIsSpine(t *testing.T) {
	tests := []struct {
		name string
		s    SessionRecord
		want bool
	}{
		{"ongoing", SessionRecord{Status: SessionOngoing}, true},
		{"awaiting", SessionRecord{Status: SessionAwaiting}, true},
		{"crashed root", SessionRecord{Status: SessionCrashed}, true},
		{"crashed child", SessionRecord{Status: SessionCrashed, ParentID: "p"}, false},
		{"idle", SessionRecord{Status: SessionIdle}, false},
		{"concluded", SessionRecord{Status: SessionConcluded}, false},
		{"cancelled", SessionRecord{Status: SessionCancelled}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSpine(tt.s); got != tt.want {
				t.Errorf("IsSpine(%+v) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestIsSummarizationFailure(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		want    bool
	}{
		{"real summary", "explored the codebase", false},
		{"empty", "", false},
		{"failure sentinel", SummarizationFailedPrefix + " LLM call failed", true},
		{"failure sentinel leading space", "  " + SummarizationFailedPrefix + " boom", true},
		{"mentions phrase mid-string", "the [summarization failed] earlier", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSummarizationFailure(tt.summary); got != tt.want {
				t.Errorf("IsSummarizationFailure(%q) = %v, want %v", tt.summary, got, tt.want)
			}
		})
	}
}

// Record-only events wake no one; WakingEventsSince skips them.
func TestIsRecordOnly(t *testing.T) {
	for _, c := range []struct {
		evt  event.Event
		want bool
	}{
		{&event.ToolResultEvent{}, true},
		{&event.VerifyEvent{Action: event.VerifySet}, true},
		{&event.VerifyEvent{Action: event.VerifyStale, Outcome: &event.VerifyOutcome{Verifier: "v", Verdict: "PASS"}}, true},
		{&event.VerifyEvent{Action: event.VerifyWithdrawn}, false},
		{event.VerifierOutcome("v", "PASS", "", time.Time{}), false},
		{event.VerifierOutcome("v", "boom", SessionCrashed, time.Time{}), false},
		{&event.UnknownEvent{Type: "teleport"}, true},
		{&event.LoopEvent{Action: event.LoopSet}, true},
		{&event.UserEvent{Content: "tick", Loop: &event.LoopTick{ID: "1", N: 1}}, false},
		{&event.ChildResultEvent{Verdict: SessionCancelled}, false},
		{&event.UserEvent{Content: "x", Check: event.CheckVerify}, false},
		{&event.MessageEvent{Content: "x"}, false},
	} {
		if got := IsRecordOnly(c.evt); got != c.want {
			t.Errorf("IsRecordOnly(%s) = %v, want %v", c.evt.EventType(), got, c.want)
		}
	}
}

// OpenConclude finds the latest turn's conclude call while it has no result,
// whatever came after it (the verifier's events, messages); an answered call,
// an older one, or a turn with other calls is not open.
func TestOpenConclude(t *testing.T) {
	conclude := func(id string) EventRecord {
		return EventRecord{Step: 3, Event: &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: id, Name: event.ConcludeTool}}}}
	}
	result := func(id string) EventRecord { return EventRecord{Event: &event.ToolResultEvent{ToolCallID: id}} }
	arrived := EventRecord{Step: 4, Event: &event.MessageEvent{Content: "hi"}}
	work := EventRecord{Step: 5, Event: &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "b1", Name: "bash"}}}}
	for _, c := range []struct {
		name   string
		events []EventRecord
		open   string
	}{
		{"waiting", []EventRecord{conclude("c1"), arrived}, "c1"},
		{"answered", []EventRecord{conclude("c1"), result("c1"), arrived}, ""},
		{"older call, then work", []EventRecord{conclude("c1"), work}, ""},
		{"no turns", []EventRecord{arrived}, ""},
		{"alongside another call", []EventRecord{{Step: 3, Event: &event.AssistantEvent{ToolCalls: []event.ToolCall{
			{ID: "b1", Name: "bash"}, {ID: "c1", Name: event.ConcludeTool}}}}}, ""},
	} {
		call, step, ok := OpenConclude(c.events)
		if (c.open != "") != ok || call.ID != c.open || (ok && step != 3) {
			t.Errorf("%s: OpenConclude = %+v, %d, %v; want %q at step 3", c.name, call, step, ok, c.open)
		}
	}
}
