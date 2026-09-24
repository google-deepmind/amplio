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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/tool"
	"amplio/internal/workspace/plain"
)

const cyberExplanation = "This request was declined because it could enable cyber harm."

func cyberRefusal() *llm.Refusal {
	return &llm.Refusal{Category: "cyber", Explanation: cyberExplanation}
}

// TestEventLoop_RefusalPersisted: a refused turn must not be a silent empty
// reply. The provider's Refusal is persisted on the AssistantEvent (and survives
// the store round-trip, which is what the chat and trajectory views read) and is
// rendered by ToText for the inspection tools.
func TestEventLoop_RefusalPersisted(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model:     "test-model",
		Responses: []llm.Response{{StopReason: "refusal", Refusal: cyberRefusal()}},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); !errors.Is(err, errProviderRefused) {
		t.Fatalf("Run error = %v, want a provider-refusal failure", err)
	}

	events, err := store.GetEvents(ctx, runID, "main-agent", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var refused *event.AssistantEvent
	for _, e := range events {
		if a, ok := e.Event.(*event.AssistantEvent); ok && a.Refusal != nil {
			refused = a
		}
	}
	if refused == nil {
		t.Fatal("no AssistantEvent carries the refusal after the store round-trip")
		return
	}
	if refused.StopReason != "refusal" || refused.Refusal.Category != "cyber" ||
		refused.Refusal.Explanation != cyberExplanation {
		t.Errorf("persisted refusal = %+v (stop_reason %q)", refused.Refusal, refused.StopReason)
	}

	txt := refused.ToText()
	for _, want := range []string{"refusal · category=cyber", "could enable cyber harm"} {
		if !strings.Contains(txt, want) {
			t.Errorf("ToText missing %q:\n%s", want, txt)
		}
	}
}

// countUserEvents counts UserEvents with exactly this content on a session's
// stream (e.g. the conclude / output-limit nudges).
func countUserEvents(t *testing.T, store db.Store, runID, sid, content string) int {
	t.Helper()
	events, err := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Content == content {
			n++
		}
	}
	return n
}

// errorMarker returns the content of the session's error SystemEvent (the
// crash marker recordFailure writes), or "" if there is none.
func errorMarker(t *testing.T, store db.Store, runID, sid string) string {
	t.Helper()
	events, err := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if s, ok := e.Event.(*event.SystemEvent); ok && s.Marker == event.MarkerError {
			return s.Content
		}
	}
	return ""
}

// An autonomous agent whose turn is refused fails with the provider's reason:
// no conclude-nudge (the retry would replay the same context), no empty
// "successful" conclusion. The crash is recorded on its own stream and posted
// to the parent as child_result(crashed) carrying the reason.
func TestEventLoop_RefusalCrashesAutonomousAgent(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	_ = store.CreateSession(ctx, db.SessionRecord{
		RunID: runID, SessionID: "main-agent", Status: db.SessionOngoing,
	})

	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{StopReason: "refusal", Refusal: cyberRefusal()},
			{Content: "SHOULD NOT BE CALLED", StopReason: "end_turn"},
		},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "child", Task: "do it", ParentID: "main-agent",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	err := ag.Run(ctx)
	if !errors.Is(err, errProviderRefused) {
		t.Fatalf("Run error = %v, want a provider-refusal failure", err)
	}
	if got := mock.CallCount(); got != 1 {
		t.Errorf("LLM calls = %d, want 1 (a refusal is not retried)", got)
	}
	if n := countUserEvents(t, store, runID, "child", concludeNudgeText); n != 0 {
		t.Errorf("conclude nudges = %d, want 0 after a refusal", n)
	}

	child, _ := store.GetSession(ctx, runID, "child")
	if child.Status != db.SessionCrashed {
		t.Errorf("child status = %q, want crashed", child.Status)
	}
	wantReason := []string{`stop_reason="refusal"`, "category=cyber", cyberExplanation}
	marker := errorMarker(t, store, runID, "child")
	for _, want := range wantReason {
		if !strings.Contains(marker, want) {
			t.Errorf("error marker %q missing %q", marker, want)
		}
	}

	evts, _ := store.GetEvents(ctx, runID, "main-agent", db.EventFilter{})
	var found *event.ChildResultEvent
	for _, e := range evts {
		if cr, ok := e.Event.(*event.ChildResultEvent); ok {
			found = cr
		}
	}
	if found == nil {
		t.Fatal("parent did not receive a ChildResultEvent")
		return
	}
	if found.Verdict != db.SessionCrashed || found.ChildSessionID != "child" {
		t.Errorf("child result = %+v, want verdict crashed from child", found)
	}
	for _, want := range wantReason {
		if !strings.Contains(found.Content, want) {
			t.Errorf("child result content %q missing %q", found.Content, want)
		}
	}
}

// A refusal that arrives after some text (a mid-stream classifier stop) must not
// be concluded with the cut-off text as the agent's result.
func TestEventLoop_RefusalWithPartialTextCrashes(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "Here is the first part of", StopReason: "refusal", Refusal: cyberRefusal()},
			{Content: "SHOULD NOT BE CALLED", StopReason: "end_turn"},
		},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); !errors.Is(err, errProviderRefused) {
		t.Fatalf("Run error = %v, want a provider-refusal failure", err)
	}
	if got := mock.CallCount(); got != 1 {
		t.Errorf("LLM calls = %d, want 1", got)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionCrashed {
		t.Errorf("status = %q, want crashed (not concluded with the partial text)", sess.Status)
	}
}

// A refused turn persisted just before the process died (status still ongoing,
// the refusal as the stream tail) must fail on resume too, without re-calling
// the LLM — the recovery path may not nudge it as an empty turn or conclude it.
// Also covers a refusal with no category / explanation.
func TestEventLoop_ResumeAtRestRefusalCrashes(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{
		RunID: runID, SessionID: "main-agent", AgentType: "standard_agent", Status: db.SessionOngoing,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceStep(ctx, runID, "main-agent"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeStep(ctx, runID, "main-agent", 1, []event.Event{
		&event.UserEvent{Content: "do the thing"},
		&event.AssistantEvent{StopReason: "PROMPT_BLOCKED", Refusal: &event.Refusal{}},
	}); err != nil {
		t.Fatal(err)
	}

	mock := &llm.MockProvider{
		Model:     "test-model",
		Responses: []llm.Response{{Content: "SHOULD NOT BE CALLED", StopReason: "end_turn"}},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); !errors.Is(err, errProviderRefused) {
		t.Fatalf("Run error = %v, want a provider-refusal failure", err)
	}
	if got := mock.CallCount(); got != 0 {
		t.Errorf("LLM calls = %d, want 0 on the at-rest resume", got)
	}
	if n := countUserEvents(t, store, runID, "main-agent", concludeNudgeText); n != 0 {
		t.Errorf("conclude nudges = %d, want 0", n)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionCrashed {
		t.Errorf("status = %q, want crashed", sess.Status)
	}
	if marker := errorMarker(t, store, runID, "main-agent"); !strings.Contains(marker, `stop_reason="PROMPT_BLOCKED", category=unspecified`) {
		t.Errorf("error marker = %q, want the stop reason and an unspecified category", marker)
	}
}

// Interactive agents are unaffected: a refused chatbot turn parks idle for the
// operator, who sees the refusal in the chat; the session is not crashed.
func TestEventLoop_RefusalInteractiveParksIdle(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock := &llm.MockProvider{
		Model:     "test-model",
		Responses: []llm.Response{{StopReason: "refusal", Refusal: cyberRefusal()}},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "chatbot", AgentType: "chatbot", FirstMessage: "hi",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
		Interactive: true, IdleTimeout: time.Minute,
	})
	done := make(chan error, 1)
	go func() { done <- ag.Run(ctx) }()

	waitForIdle(t, store, runID, "chatbot")
	if got := mock.CallCount(); got != 1 {
		t.Errorf("LLM calls = %d, want 1", got)
	}
	if marker := errorMarker(t, store, runID, "chatbot"); marker != "" {
		t.Errorf("interactive refusal recorded a crash: %q", marker)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run error = %v, want nil after cancel", err)
	}
}

// The provider's stop detail is persisted verbatim with its stop reason, for the
// chat and trajectory views to show on an abnormal stop.
func TestEventLoop_StopMessagePersisted(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "partial", StopReason: "MALFORMED_FUNCTION_CALL", StopMessage: "Malformed function call: print(x)"},
		},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := store.GetEvents(ctx, runID, "main-agent", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got *event.AssistantEvent
	for _, e := range events {
		if a, ok := e.Event.(*event.AssistantEvent); ok {
			got = a
		}
	}
	if got == nil || got.StopReason != "MALFORMED_FUNCTION_CALL" || got.StopMessage != "Malformed function call: print(x)" {
		t.Errorf("persisted assistant turn = %+v, want the stop reason and message", got)
	}
}
