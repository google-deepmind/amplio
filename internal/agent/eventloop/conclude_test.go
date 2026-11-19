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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/tool/conclude"
	"amplio/internal/verify"
	"amplio/internal/workspace/plain"
)

var callSeq atomic.Int64

// concludeTurn is a scripted turn that calls conclude with result ("" for
// none), under a fresh call id.
func concludeTurn(result string) llm.Response {
	args := "{}"
	if result != "" {
		b, _ := json.Marshal(event.ConcludeArgs{Result: result})
		args = string(b)
	}
	return llm.Response{StopReason: "tool_use", ToolCalls: []llm.ToolCall{
		{ID: fmt.Sprintf("conclude-%d", callSeq.Add(1)), Name: event.ConcludeTool, Arguments: args}}}
}

// reply is a scripted turn without tool calls.
func reply(text string) llm.Response { return llm.Response{Content: text, StopReason: "end_turn"} }

func turns(rs ...llm.Response) *llm.MockProvider { return &llm.MockProvider{Model: "m", Responses: rs} }

// callResults are the results of the worker's conclude calls, in order.
func callResults(store db.Store, runID string) []string {
	events, _ := store.GetEvents(context.Background(), runID, "worker", db.EventFilter{})
	calls := map[string]bool{}
	var out []string
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *event.AssistantEvent:
			for _, tc := range ev.ToolCalls {
				if tc.Name == event.ConcludeTool {
					calls[tc.ID] = true
				}
			}
		case *event.ToolResultEvent:
			if calls[ev.ToolCallID] {
				out = append(out, ev.Content)
			}
		}
	}
	return out
}

// checkedMessages are a session's harness messages marked with check.
func checkedMessages(store db.Store, runID, sid, check string) []string {
	events, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check == check {
			out = append(out, u.Content)
		}
	}
	return out
}

// assertPaired checks a request is one every provider accepts as to tool
// calls: each assistant tool call is answered by a tool result before the
// next assistant turn, and no result answers a call that wasn't made.
func assertPaired(t *testing.T, req llm.Request) {
	t.Helper()
	open := map[string]bool{}
	for i, m := range req.Messages {
		switch m.Role {
		case llm.RoleAssistant:
			for id := range open {
				t.Errorf("message %d: tool call %s has no result before the next assistant turn", i, id)
			}
			clear(open)
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case llm.RoleToolResult:
			if !open[m.ToolCallID] {
				t.Errorf("message %d: a result for %s, which no pending call made", i, m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		case llm.RoleSystem, llm.RoleUser:
		}
	}
	for id := range open {
		t.Errorf("tool call %s has no result", id)
	}
}

// lastToolResult is the content of a request's last tool result.
func lastToolResult(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleToolResult {
			return req.Messages[i].Content
		}
	}
	return ""
}

// concluder is an autonomous agent "worker" under parent "boss" with the
// conclude tool, and any others.
func concluder(t *testing.T, mock llm.Provider, tools ...*tool.Tool) (*EventLoopAgent, db.Store, string, *session.Registry) {
	t.Helper()
	store, runID, registry := testSetup(t)
	if err := store.CreateSession(context.Background(), db.SessionRecord{RunID: runID, SessionID: "boss",
		AgentType: "standard_agent", Status: db.SessionAwaiting}); err != nil {
		t.Fatal(err)
	}
	return concluderIn(store, runID, registry, mock, tools...), store, runID, registry
}

func concluderIn(store db.Store, runID string, registry *session.Registry, mock llm.Provider, tools ...*tool.Tool) *EventLoopAgent {
	return newT(testCfg{RunID: runID, SessionID: "worker", ParentID: "boss", Task: "How tall is it?", SystemPrompt: "sp",
		LLM: mock, Store: store, Registry: registry, Workspace: plain.New("/tmp"),
		Tools: append([]*tool.Tool{conclude.New()}, tools...)})
}

func childResult(t *testing.T, store db.Store, runID string) *event.ChildResultEvent {
	t.Helper()
	events, _ := store.GetEvents(context.Background(), runID, "boss", db.EventFilter{})
	for i := len(events) - 1; i >= 0; i-- {
		if c, ok := events[i].Event.(*event.ChildResultEvent); ok {
			return c
		}
	}
	t.Fatal("no child_result")
	return nil
}

func runToEnd(t *testing.T, ag *EventLoopAgent) {
	t.Helper()
	if err := ag.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// conclude(result) with nothing holding it concludes with exactly result; its
// tool result is written with the conclusion, at the call's step, which is
// finalized.
func TestConclude_ConcludesWithResult(t *testing.T) {
	withJobs(t)
	mock := turns(llm.Response{Content: "Done measuring.", StopReason: "tool_use", ToolCalls: concludeTurn("It is 42 m tall.").ToolCalls})
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)

	if s, _ := store.GetSession(context.Background(), runID, "worker"); s.Status != db.SessionConcluded {
		t.Fatalf("worker is %s, want concluded", s.Status)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m tall." || cr.Verdict != db.SessionConcluded {
		t.Errorf("child_result = %+v, want the call's result, not the turn's text", cr)
	}
	events, _ := store.GetEvents(context.Background(), runID, "worker", db.EventFilter{})
	var callStep, resultStep = -1, -2
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *event.AssistantEvent:
			callStep = e.Step
		case *event.ToolResultEvent:
			resultStep = e.Step
			if ev.Content != "Concluded." || ev.IsError {
				t.Errorf("tool result = %+v", ev)
			}
		}
	}
	if resultStep != callStep {
		t.Errorf("the call's result is at step %d, its call at %d", resultStep, callStep)
	}
	if s, _ := store.GetSession(context.Background(), runID, "worker"); s.LastFinalizedStep < callStep {
		t.Errorf("last finalized step = %d, want the concluding step %d finalized", s.LastFinalizedStep, callStep)
	}
	if mock.CallCount() != 1 {
		t.Errorf("LLM calls = %d, want 1", mock.CallCount())
	}
}

// conclude must be called alone: alongside other calls it fails on its own
// (they run), and the next turn's call concludes.
func TestConclude_AloneInItsTurn(t *testing.T) {
	withJobs(t)
	ran := false
	work := &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			ran = true
			return &tool.Result{Content: "worked"}, nil
		}}
	both := concludeTurn("early")
	both.ToolCalls = append([]llm.ToolCall{{ID: "w1", Name: "work", Arguments: "{}"}}, both.ToolCalls...)
	mock := turns(both, concludeTurn("It is 42 m."))
	ag, store, runID, _ := concluder(t, mock, work)
	runToEnd(t, ag)
	if !ran {
		t.Error("the other call didn't run")
	}
	got := callResults(store, runID)
	if len(got) != 2 || !strings.Contains(got[0], "must be the only tool call in the step") || got[1] != "Concluded." {
		t.Errorf("conclude results = %q", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// conclude() without a result, with nothing waiting for a verdict, is an
// error: there is nothing to deliver.
func TestConclude_WithoutResultNothingPendingIsAnError(t *testing.T) {
	withJobs(t)
	mock := turns(concludeTurn(""), concludeTurn("It is 42 m."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)
	events, _ := store.GetEvents(context.Background(), runID, "worker", db.EventFilter{})
	var first *event.ToolResultEvent
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok {
			first = tr
			break
		}
	}
	if first == nil || !first.IsError || first.Content != nothingPending {
		t.Errorf("result of conclude() = %+v, want the nothing-pending error", first)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// With a background job running, conclude returns the reminder once, marked
// as the leftovers check's; calling it again concludes.
func TestConclude_ReminderThenConcludes(t *testing.T) {
	withJobs(t, "worker")
	mock := turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m; the benchmark is left running on purpose."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)
	events, _ := store.GetEvents(context.Background(), runID, "worker", db.EventFilter{})
	var reminder *event.ToolResultEvent
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok && tr.Check == event.CheckLeftovers {
			reminder = tr
		}
	}
	if reminder == nil || !strings.HasPrefix(reminder.Content, reminderCallPrefix) ||
		!strings.Contains(reminder.Content, "process group 48213") || !strings.Contains(reminder.Content, "call conclude again") {
		t.Fatalf("reminder = %+v", reminder)
	}
	if cr := childResult(t, store, runID); !strings.Contains(cr.Content, "left running on purpose") ||
		len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteLeftover {
		t.Errorf("child_result = %+v, want the second result, noting the job", cr)
	}
	if n := len(reminders(t, store, runID, "worker")); n != 0 {
		t.Errorf("reminder messages = %d, want none: the call carried it", n)
	}
}

// A call never concludes over input that arrived while its turn was
// generated: it returns saying what came, and the next call concludes.
func TestConclude_NotOverUnreadInput(t *testing.T) {
	withJobs(t)
	var store db.Store
	var runID string
	mock := &hookLLM{MockProvider: turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m, as asked.")), hook: func() {
		if _, err := store.AppendEvent(context.Background(), runID, "worker", &event.MessageEvent{
			Content: "use metres", Sender: "boss", SenderType: event.SenderTypeAgent}); err != nil {
			panic(err)
		}
	}}
	ag, s, r, _ := concluder(t, mock)
	store, runID = s, r
	runToEnd(t, ag)
	got := callResults(store, runID)
	if len(got) != 2 || got[0] != "Not concluded yet: this arrived meanwhile: a message from boss. "+resultAgain {
		t.Errorf("conclude results = %q", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m, as asked." {
		t.Errorf("child_result = %q", cr.Content)
	}
	if !strings.Contains(lastUserContent(mock.Recorded()[1]), "use metres") {
		t.Error("the turn after the held call didn't read the message")
	}
}

// An agent with the conclude tool that replies without tool calls, nothing
// pending, concludes with the reply, as before the tool existed.
func TestConclude_ReplyStillConcludes(t *testing.T) {
	withJobs(t)
	mock := turns(reply("It is 42 m."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// An empty reply's nudge tells an agent with the tool to call conclude.
func TestConclude_EmptyTurnNudgeNamesTheTool(t *testing.T) {
	withJobs(t)
	mock := turns(reply(""), concludeTurn("It is 42 m."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)
	if got := checkedMessages(store, runID, "worker", event.CheckEmptyTurn); len(got) != 1 || got[0] != concludeNudgeToolText {
		t.Errorf("empty-turn nudges = %q", got)
	}
}

// A session that concluded through conclude, revived by a follow-up, sends
// the provider a history in which every tool call has its result.
func TestConclude_RevivedHistoryIsValid(t *testing.T) {
	withJobs(t)
	ag, store, runID, registry := concluder(t, turns(concludeTurn("It is 42 m.")))
	runToEnd(t, ag)
	if _, err := store.AppendEvent(context.Background(), runID, "worker", &event.UserEvent{Content: "And in feet?"}); err != nil {
		t.Fatal(err)
	}
	mock := turns(concludeTurn("About 138 ft."))
	runToEnd(t, concluderIn(store, runID, registry, mock))
	assertPaired(t, mock.Recorded()[0])
	if cr := childResult(t, store, runID); cr.Content != "About 138 ft." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// A conclude call its process died in (before it was answered) is run again
// by the next life, not mended as interrupted: no model call, and it
// concludes.
func TestConclude_ResumedAfterACrash(t *testing.T) {
	withJobs(t)
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	for _, s := range []db.SessionRecord{
		{RunID: runID, SessionID: "boss", AgentType: "standard_agent", Status: db.SessionAwaiting},
		{RunID: runID, SessionID: "worker", ParentID: "boss", AgentType: "standard_agent", Task: "q", Status: db.SessionOngoing},
	} {
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	seedResumeStep(t, store, runID, "worker")
	call := concludeTurn("It is 42 m.").ToolCalls[0]
	if err := store.AppendEventAtStep(ctx, runID, "worker", 1, &event.AssistantEvent{
		ToolCalls: []event.ToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}}}); err != nil {
		t.Fatal(err)
	}
	mock := turns()
	runToEnd(t, concluderIn(store, runID, registry, mock))
	if mock.CallCount() != 0 {
		t.Errorf("LLM calls = %d, want 0", mock.CallCount())
	}
	if got := callResults(store, runID); len(got) != 1 || got[0] != "Concluded." {
		t.Errorf("conclude results = %q, want it run, not mended", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// pendingNudges counts the nudges since the agent's last turn with tool
// calls: a turn with tool calls starts the count again.
func TestPendingNudges(t *testing.T) {
	nudge := func() db.EventRecord {
		return db.EventRecord{Event: &event.UserEvent{Content: "n", Check: event.CheckConclude}}
	}
	reply := db.EventRecord{Event: &event.AssistantEvent{Content: "waiting"}}
	work := db.EventRecord{Event: &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "a", Name: "await_event"}}}}
	for _, c := range []struct {
		events []db.EventRecord
		want   int
	}{
		{nil, 0},
		{[]db.EventRecord{reply, nudge()}, 1},
		{[]db.EventRecord{reply, nudge(), reply, nudge()}, 2},
		{[]db.EventRecord{reply, nudge(), work, reply, nudge()}, 1},
		// A message about the verification (a wait's note, a verdict) starts a new run.
		{[]db.EventRecord{reply, nudge(), reply, nudge(), {Event: &event.UserEvent{Content: "v", Check: event.CheckVerify}}, reply}, 0},
	} {
		if got := pendingNudges(c.events); got != c.want {
			t.Errorf("pendingNudges(%d events) = %d, want %d", len(c.events), got, c.want)
		}
	}
}

// describeArrivals names what ended a wait, once each, briefly.
func TestDescribeArrivals(t *testing.T) {
	recs := []db.EventRecord{
		{Event: &event.MessageEvent{Sender: "boss", SenderType: event.SenderTypeAgent}},
		{Event: &event.MessageEvent{Sender: "environment (pgid=7)", SenderType: event.SenderTypeEnvironment}},
		{Event: &event.MessageEvent{Sender: "boss", SenderType: event.SenderTypeAgent}},
		{Event: &event.UserEvent{Content: "hi"}},
		{Event: &event.UserEvent{Content: "[loop 2 · tick 1] check", Loop: &event.LoopTick{ID: "2", N: 1}}},
		{Event: &event.ChildResultEvent{ChildSessionID: "helper", Verdict: db.SessionCrashed}},
	}
	want := "a message from boss; a notification from environment (pgid=7); a message from the operator; " +
		"a tick of loop 2; and 1 more"
	if got := describeArrivals(recs); got != want {
		t.Errorf("describeArrivals = %q, want %q", got, want)
	}
}

// The output-limit retry budget survives a restart for an agent with the
// conclude tool too, whose nudge reads differently: the nudges are counted by
// their Check.
func TestOutputLimitNudgesFromHistory_CountsByCheck(t *testing.T) {
	nudge := db.EventRecord{Event: &event.UserEvent{Content: outputLimitNudgeToolText, Check: event.CheckOutputLimit}}
	cut := db.EventRecord{Event: &event.AssistantEvent{Content: "partial", StopReason: "length"}}
	if n := outputLimitNudgesFromHistory([]db.EventRecord{cut, nudge, cut, nudge}); n != 2 {
		t.Errorf("nudges counted = %d, want 2", n)
	}
}

// A conclude call made alongside another call, cut off by a crash before
// either got a result, is no open conclusion: both are mended as interrupted
// and the agent takes a turn, rather than concluding unseen.
func TestConclude_MultiCallTurnIsNotResumedAsAConclusion(t *testing.T) {
	withJobs(t)
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	for _, s := range []db.SessionRecord{
		{RunID: runID, SessionID: "boss", AgentType: "standard_agent", Status: db.SessionAwaiting},
		{RunID: runID, SessionID: "worker", ParentID: "boss", AgentType: "standard_agent", Task: "q", Status: db.SessionOngoing},
	} {
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	seedResumeStep(t, store, runID, "worker")
	call := concludeTurn("It is 42 m.").ToolCalls[0]
	if err := store.AppendEventAtStep(ctx, runID, "worker", 1, &event.AssistantEvent{ToolCalls: []event.ToolCall{
		{ID: "b1", Name: "bash", Arguments: `{"command":"make test"}`}, {ID: call.ID, Name: call.Name, Arguments: call.Arguments}}}); err != nil {
		t.Fatal(err)
	}
	mock := turns(concludeTurn("It is 42 m, tests passing."))
	runToEnd(t, concluderIn(store, runID, registry, mock))
	if mock.CallCount() != 1 {
		t.Errorf("LLM calls = %d, want 1: the agent sees the interrupted calls first", mock.CallCount())
	}
	if got := callResults(store, runID); len(got) != 2 || !strings.Contains(got[0], "interrupted by process crash") {
		t.Errorf("conclude results = %q, want the interrupted one mended", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m, tests passing." {
		t.Errorf("child_result = %q, want the turn after", cr.Content)
	}
}

// A wait never parks over a cancel that landed before it: the status stays
// cancelled and the wait reports the session stopped.
func TestWaitForVerdict_NeverOverACancel(t *testing.T) {
	withJobs(t)
	ag, store, runID, _ := concluder(t, turns())
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "worker", ParentID: "boss",
		AgentType: "standard_agent", Status: db.SessionCancelled}); err != nil {
		t.Fatal(err)
	}
	ag.handle = session.NewHandle(func() {})
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, stopped, err := ag.waitForVerdict(waitCtx, 1, nil)
	if err != nil || !stopped {
		t.Errorf("waitForVerdict = stopped %v, %v; want stopped at once", stopped, err)
	}
	if s, _ := store.GetSession(ctx, runID, "worker"); s.Status != db.SessionCancelled {
		t.Errorf("status = %s, want cancelled", s.Status)
	}
}

// A conclusion whose terminal write finds the session already stopped (a
// cancel won the race) delivered nothing: no "verification passed" line, and
// the call's result isn't written.
func TestConclude_NothingLoggedWhenAlreadyStopped(t *testing.T) {
	withJobs(t)
	var buf strings.Builder
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ag, store, runID, _ := concluder(t, turns())
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "worker", ParentID: "boss",
		AgentType: "standard_agent", Status: db.SessionCancelled}); err != nil {
		t.Fatal(err)
	}
	out := passedOutcome(verify.State{Verifier: "v", Candidate: "It is 42 m."}, verify.Verdict{Given: true, Pass: true})
	if err := ag.conclude(out, attempt{call: &event.ToolCall{ID: "c1", Name: event.ConcludeTool}, step: 1}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "verification passed") {
		t.Errorf("an outcome was logged for a conclusion that didn't happen:\n%s", buf.String())
	}
	if got := callResults(store, runID); len(got) != 0 {
		t.Errorf("call results = %q, want none written", got)
	}
}
