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
	"amplio/internal/proctree"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/workspace/plain"
)

// withJobs makes the process table show, for each session, one background job.
func withJobs(t *testing.T, sessions ...string) {
	t.Helper()
	var roots []*proctree.Process
	for i, sid := range sessions {
		roots = append(roots, &proctree.Process{PID: 48213 + i, PGID: 48213 + i, SessionID: sid,
			Elapsed: 2834, Cmdline: "./bench.sh --iters 50"})
	}
	orig := scanProcs
	scanProcs = func(string) proctree.Snapshot { return proctree.Snapshot{Supported: true, Roots: roots} }
	t.Cleanup(func() { scanProcs = orig })
}

func reminders(t *testing.T, store db.Store, runID, sid string) []string {
	t.Helper()
	events, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && strings.HasPrefix(u.Content, reminderPrefix) {
			out = append(out, u.Content)
		}
	}
	return out
}

func lastAssistant(t *testing.T, store db.Store, runID, sid string) string {
	t.Helper()
	events, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	last := ""
	for _, e := range events {
		if a, ok := e.Event.(*event.AssistantEvent); ok {
			last = a.Content
		}
	}
	return last
}

func runAutonomous(t *testing.T, store db.Store, runID, sid, parent string, registry *session.Registry, mock *llm.MockProvider, tools ...*tool.Tool) {
	t.Helper()
	cfg := testCfg{RunID: runID, SessionID: sid, ParentID: parent, Task: "q", SystemPrompt: "sp",
		LLM: mock, Store: store, Registry: registry, Tools: tools, Workspace: plain.New("/tmp")}
	if err := newT(cfg).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.GetSession(context.Background(), runID, sid); s == nil || s.Status != db.SessionConcluded {
		t.Fatalf("session not concluded: %+v", s)
	}
}

// An agent that concludes with a background job still running gets one
// reminder, listing the job and what concluding will do; its next final turn
// concludes, and that turn's text is the result.
func TestEndOfRun_ReminderThenConcludes(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t, "main-agent")
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{Content: "The answer is 42.", StopReason: "end_turn"},
		{Content: "The answer is 42. The benchmark is left running on purpose.", StopReason: "end_turn"},
	}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)

	if mock.CallCount() != 2 {
		t.Errorf("LLM calls = %d, want 2 (a reminder, then conclude)", mock.CallCount())
	}
	rs := reminders(t, store, runID, "main-agent")
	if len(rs) != 1 {
		t.Fatalf("reminders = %d, want 1", len(rs))
	}
	if c := checksOf(t, store, runID, "main-agent"); len(c) != 1 || c[0] != event.CheckLeftovers {
		t.Errorf("checks on harness messages = %v, want [leftovers]", c)
	}
	for _, want := range []string{
		"- background process you started: process group 48213, 47 min: ./bench.sh --iters 50",
		"notifications from background processes will no longer reach you",
		"wait with await_event", "kill -- -<process group>",
		"restate your full conclusion",
	} {
		if !strings.Contains(rs[0], want) {
			t.Errorf("reminder lacks %q:\n%s", want, rs[0])
		}
	}
	if strings.Contains(rs[0], "sub-agents") || strings.Contains(rs[0], "session_cancel") {
		t.Errorf("reminder mentions sub-agents when there are none:\n%s", rs[0])
	}
	if got := lastAssistant(t, store, runID, "main-agent"); !strings.Contains(got, "left running on purpose") {
		t.Errorf("result = %q, want the turn after the reminder", got)
	}
}

// Nothing running: no reminder, the first final turn concludes.
func TestEndOfRun_NothingRunningConcludes(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t) // an empty process table
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{{Content: "Done.", StopReason: "end_turn"}}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)
	if mock.CallCount() != 1 || len(reminders(t, store, runID, "main-agent")) != 0 {
		t.Errorf("calls = %d, reminders = %d; want 1 and 0", mock.CallCount(), len(reminders(t, store, runID, "main-agent")))
	}
}

// After a reminder, an agent that goes back to work and later concludes with
// the job still running is reminded again: only the attempt right after a
// reminder is let through.
func TestEndOfRun_BackToWorkRearms(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t, "main-agent")
	waitTool := &tool.Tool{Name: "wait", Description: "wait", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			return &tool.Result{Content: "waited"}, nil
		}}
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{Content: "Done.", StopReason: "end_turn"},
		{Content: "", StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "w1", Name: "wait", Arguments: `{}`}}},
		{Content: "Done, after waiting.", StopReason: "end_turn"},
		{Content: "Done, after waiting; the job keeps running.", StopReason: "end_turn"},
	}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock, waitTool)
	if mock.CallCount() != 4 || len(reminders(t, store, runID, "main-agent")) != 2 {
		t.Errorf("calls = %d, reminders = %d; want 4 and 2", mock.CallCount(), len(reminders(t, store, runID, "main-agent")))
	}
}

// Running sub-agents are listed too, and the reminder says they'll be cancelled.
func TestEndOfRun_RunningSubAgentListed(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t)
	ctx := context.Background()
	for sid, status := range map[string]string{"calm-heron": db.SessionAwaiting, "done-owl": db.SessionConcluded} {
		if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: sid,
			ParentID: "main-agent", AgentType: "standard_agent", Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{Content: "Done.", StopReason: "end_turn"}, {Content: "Done; calm-heron is not needed.", StopReason: "end_turn"},
	}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)
	rs := reminders(t, store, runID, "main-agent")
	if len(rs) != 1 || !strings.Contains(rs[0], "- sub-agents: calm-heron\n") || !strings.Contains(rs[0], "running sub-agents are cancelled") {
		t.Errorf("reminders = %q, want one listing calm-heron (and not the finished done-owl)", rs)
	}
	if len(rs) == 1 && (strings.Contains(rs[0], "background process") || strings.Contains(rs[0], "kill --")) {
		t.Errorf("reminder about a sub-agent alone talks about background processes:\n%s", rs[0])
	}
}

// A sub-agent that concludes with a job still running tells its parent so, on
// the child_result header; the result text is the child's own words.
func TestEndOfRun_ChildResultCarriesLeftover(t *testing.T) {
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "boss",
		AgentType: "standard_agent", Status: db.SessionAwaiting}); err != nil {
		t.Fatal(err)
	}
	withJobs(t, "worker")
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{Content: "Done.", StopReason: "end_turn"}, {Content: "Done; the job keeps running.", StopReason: "end_turn"},
	}}
	runAutonomous(t, store, runID, "worker", "boss", registry, mock)
	events, _ := store.GetEvents(ctx, runID, "boss", db.EventFilter{})
	var cr *event.ChildResultEvent
	for _, e := range events {
		if c, ok := e.Event.(*event.ChildResultEvent); ok {
			cr = c
		}
	}
	if cr == nil {
		t.Fatal("parent got no child_result")
	}
	if len(cr.Notes) != 1 || cr.Notes[0] != (event.ResultNote{Kind: event.NoteLeftover, Text: "1 background process group"}) ||
		cr.Content != "Done; the job keeps running." {
		t.Errorf("child_result = %+v", cr)
	}
	if text := cr.ToText(); !strings.Contains(text, "left running: 1 background process group") {
		t.Errorf("child_result header lacks the leftover: %s", text)
	}
}

// seedAtRest makes a session whose stream ends with a final answer, still
// marked ongoing, as a restart leaves it; extra events go before the answer.
func seedAtRest(t *testing.T, store db.Store, runID string, before ...event.Event) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "main-agent",
		AgentType: "standard_agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceStep(ctx, runID, "main-agent"); err != nil {
		t.Fatal(err)
	}
	evs := append([]event.Event{&event.UserEvent{Content: "do the thing"}}, before...)
	evs = append(evs, &event.AssistantEvent{Content: "The answer is 42.", StopReason: "end_turn"})
	if err := store.FinalizeStep(ctx, runID, "main-agent", 1, evs); err != nil {
		t.Fatal(err)
	}
}

// The restart path concludes an already-final answer without a model call; with
// work still running it reminds first, as the live loop does.
func TestEndOfRun_ResumeAtRestReminds(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t, "main-agent")
	seedAtRest(t, store, runID)
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{{Content: "The answer is 42; the job may stay.", StopReason: "end_turn"}}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)
	if mock.CallCount() != 1 || len(reminders(t, store, runID, "main-agent")) != 1 {
		t.Errorf("calls = %d, reminders = %d; want 1 and 1", mock.CallCount(), len(reminders(t, store, runID, "main-agent")))
	}
}

// A final answer that already answers a reminder concludes on the restart path
// without another reminder or a model call.
func TestEndOfRun_ResumeAfterReminderConcludes(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t, "main-agent")
	seedAtRest(t, store, runID,
		&event.AssistantEvent{Content: "First answer.", StopReason: "end_turn"},
		&event.UserEvent{Content: reminderPrefix + "\n- background process you started: ..."})
	mock := &llm.MockProvider{Model: "m"}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)
	if mock.CallCount() != 0 || len(reminders(t, store, runID, "main-agent")) != 1 {
		t.Errorf("calls = %d, reminders = %d; want 0 and the 1 seeded", mock.CallCount(), len(reminders(t, store, runID, "main-agent")))
	}
}

// Interactive agents go idle rather than concluding, and are never reminded.
func TestEndOfRun_InteractiveNotReminded(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t, "bot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{{Content: "Started the job; I'll report back.", StopReason: "end_turn"}}}
	ag := newT(testCfg{RunID: runID, SessionID: "bot", FirstMessage: "start it", Interactive: true,
		SystemPrompt: "sp", LLM: mock, Store: store, Registry: registry, Workspace: plain.New("/tmp")})
	done := make(chan error, 1)
	go func() { done <- ag.Run(ctx) }()
	waitForIdle(t, store, runID, "bot")
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if n := len(reminders(t, store, runID, "bot")); n != 0 {
		t.Errorf("interactive agent got %d reminders", n)
	}
}

// justReminded: this attempt answers a reminder iff one came after the agent's
// previous turn. Whatever arrived after the reminder doesn't re-arm it (the
// turn was produced with the reminder in context); a reminder from before the
// agent's previous turn never counts.
func TestJustReminded(t *testing.T) {
	rem := &event.UserEvent{Content: reminderPrefix + "\n- ..."}
	final := &event.AssistantEvent{Content: "Done."}
	toolTurn := &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "t1", Name: "wait"}}}
	for _, c := range []struct {
		name string
		evs  []event.Event
		want bool
	}{
		{"right after a reminder", []event.Event{final, rem}, true},
		{"operator message after the reminder", []event.Event{final, rem, &event.UserEvent{Content: "also check X"}}, true},
		{"notification after the reminder", []event.Event{final, rem, &event.MessageEvent{Content: "job done", SenderType: "environment"}}, true},
		{"back to work after the reminder", []event.Event{final, rem, toolTurn, &event.ToolResultEvent{ToolCallID: "t1"}}, false},
		{"reminder from an earlier round, then a follow-up", []event.Event{final, rem, final, &event.UserEvent{Content: "one more thing"}}, false},
		{"never reminded", []event.Event{&event.UserEvent{Content: "task"}}, false},
		{"reminder found by its Check", []event.Event{final, &event.UserEvent{Content: "any wording", Check: event.CheckLeftovers}}, true},
		{"another check's message is not a reminder", []event.Event{final, &event.UserEvent{Content: "[system] Your previous turn produced no tool calls", Check: event.CheckEmptyTurn}}, false},
	} {
		recs := make([]db.EventRecord, len(c.evs))
		for i, e := range c.evs {
			recs[i] = db.EventRecord{Event: e}
		}
		if got := justReminded(recs); got != c.want {
			t.Errorf("%s: justReminded = %v, want %v", c.name, got, c.want)
		}
	}
}

// checksOf lists the Check of every harness-written user message in a stream.
func checksOf(t *testing.T, store db.Store, runID, sid string) []string {
	t.Helper()
	events, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check != "" {
			out = append(out, u.Check)
		}
	}
	return out
}

// The empty-final-turn nudge is marked as the harness's, so UIs don't show it as
// the operator's and code can find it without matching its text.
func TestEventLoop_EmptyTurnNudgeIsMarked(t *testing.T) {
	store, runID, registry := testSetup(t)
	withJobs(t)
	mock := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{Content: "", StopReason: "end_turn"}, {Content: "Done.", StopReason: "end_turn"},
	}}
	runAutonomous(t, store, runID, "main-agent", "", registry, mock)
	if c := checksOf(t, store, runID, "main-agent"); len(c) != 1 || c[0] != event.CheckEmptyTurn {
		t.Errorf("checks = %v, want [empty_turn]", c)
	}
}
