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
	"encoding/json"
	"strings"
	"testing"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/tool"
	"amplio/internal/workspace/plain"
)

func TestEventLoop_OutputLimitNudgesThenConcludes(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "partial result", StopReason: "length"},
			{Content: "complete result", StopReason: "stop"},
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

	if got := mock.CallCount(); got != 2 {
		t.Fatalf("LLM calls = %d, want 2", got)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionConcluded {
		t.Fatalf("status = %q, want concluded", sess.Status)
	}

	recorded := mock.Recorded()
	second := recorded[1].Messages
	if len(second) < 2 {
		t.Fatalf("second request has %d messages, want replayed partial turn + nudge", len(second))
	}
	var sawPartial bool
	for _, msg := range second {
		if msg.Role == llm.RoleAssistant && msg.Content == "partial result" {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Fatal("second request did not replay the truncated assistant turn")
	}
	last := second[len(second)-1]
	if last.Role != llm.RoleUser || last.Content != outputLimitNudgeText {
		t.Fatalf("last message = %q/%q, want output-limit nudge", last.Role, last.Content)
	}
}

func TestEventLoop_OutputLimitRetryBoundCrashes(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "partial 1", StopReason: "length"},
			{Content: "partial 2", StopReason: "max_tokens"},
			{Content: "partial 3", StopReason: "MAX_TOKENS"},
			{Content: "SHOULD NOT BE CALLED", StopReason: "stop"},
		},
	}

	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	err := ag.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "repeatedly reached output-token limit") {
		t.Fatalf("Run error = %v, want bounded output-limit failure", err)
	}
	if got := mock.CallCount(); got != maxOutputLimitNudges+1 {
		t.Fatalf("LLM calls = %d, want %d", got, maxOutputLimitNudges+1)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionCrashed {
		t.Fatalf("status = %q, want crashed", sess.Status)
	}
}

func TestEventLoop_OutputLimitBudgetIndependentFromEmptyTurns(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "", StopReason: "stop"},
			{Content: "", StopReason: "stop"},
			{Content: "partial 1", StopReason: "length"},
			{Content: "partial 2", StopReason: "length"},
			{Content: "complete", StopReason: "stop"},
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
	if got := mock.CallCount(); got != 5 {
		t.Fatalf("LLM calls = %d, want 5", got)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionConcluded {
		t.Fatalf("status = %q, want concluded", sess.Status)
	}
}

func TestEventLoop_OutputLimitRetryBudgetResetsOnToolProgress(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()

	calls := 0
	progressTool := &tool.Tool{
		Name:        "progress",
		Description: "record progress",
		ParamType:   &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			calls++
			return &tool.Result{Content: "progress recorded"}, nil
		},
	}
	mock := &llm.MockProvider{
		Model: "test-model",
		Responses: []llm.Response{
			{Content: "partial 1", StopReason: "length"},
			{Content: "", StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "progress", Arguments: `{}`}}},
			{Content: "partial 2", StopReason: "length"},
			{Content: "complete", StopReason: "stop"},
		},
	}

	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "q",
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry,
		Tools: []*tool.Tool{progressTool}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
	if got := mock.CallCount(); got != 4 {
		t.Fatalf("LLM calls = %d, want 4", got)
	}
	if sess, _ := store.GetSession(ctx, runID, "main-agent"); sess.Status != db.SessionConcluded {
		t.Fatalf("status = %q, want concluded", sess.Status)
	}
}

func TestEventLoop_OutputLimitRetryBoundSurvivesColdRestarts(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()

	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	seedResumeStep(t, store, runID, "agent")

	appendTruncatedTurn := func(step int) {
		t.Helper()
		if err := store.FinalizeStep(ctx, runID, "agent", step, []event.Event{&event.AssistantEvent{
			Content: "partial", StopReason: "length",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	reconcileFresh := func() error {
		t.Helper()
		sess, err := store.GetSession(ctx, runID, "agent")
		if err != nil {
			t.Fatal(err)
		}
		ag := newT(testCfg{
			RunID: runID, SessionID: "agent", Store: store, Registry: registry,
			Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
		})
		_, _, err = ag.reconcileResume(ctx, sess)
		return err
	}

	appendTruncatedTurn(1)
	if err := reconcileFresh(); err != nil {
		t.Fatalf("first cold restart: %v", err)
	}
	if _, err := store.AdvanceStep(ctx, runID, "agent"); err != nil {
		t.Fatal(err)
	}

	appendTruncatedTurn(2)
	if err := reconcileFresh(); err != nil {
		t.Fatalf("second cold restart: %v", err)
	}
	if _, err := store.AdvanceStep(ctx, runID, "agent"); err != nil {
		t.Fatal(err)
	}

	appendTruncatedTurn(3)
	if err := reconcileFresh(); err == nil || !strings.Contains(err.Error(), "repeatedly reached output-token limit") {
		t.Fatalf("third cold restart error = %v, want bounded output-limit failure", err)
	}

	events, err := store.GetEvents(ctx, runID, "agent", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var nudges int
	for _, rec := range events {
		if e, ok := rec.Event.(*event.UserEvent); ok && e.Content == outputLimitNudgeText {
			nudges++
		}
	}
	if nudges != maxOutputLimitNudges {
		t.Fatalf("persisted output-limit nudges = %d, want %d", nudges, maxOutputLimitNudges)
	}
}

func TestEventLoop_ResumeAtRestOutputLimitNudges(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()

	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEventAtStep(ctx, runID, "agent", 1, &event.AssistantEvent{
		Content: "partial before crash", StopReason: "length",
	}); err != nil {
		t.Fatal(err)
	}
	seedResumeStep(t, store, runID, "agent")

	mock := &llm.MockProvider{
		Model:     "test-model",
		Responses: []llm.Response{{Content: "complete after resume", StopReason: "stop"}},
	}
	ag := newT(testCfg{
		RunID: runID, SessionID: "agent", LLM: mock, Store: store,
		Registry: registry, Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := mock.CallCount(); got != 1 {
		t.Fatalf("LLM calls = %d, want 1", got)
	}
	recorded := mock.Recorded()
	msgs := recorded[0].Messages
	if len(msgs) == 0 {
		t.Fatal("resume request has no messages")
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || last.Content != outputLimitNudgeText {
		t.Fatalf("last resume message = %q/%q, want output-limit nudge", last.Role, last.Content)
	}
	if sess, _ := store.GetSession(ctx, runID, "agent"); sess.Status != db.SessionConcluded {
		t.Fatalf("status = %q, want concluded", sess.Status)
	}
}
