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
	"amplio/internal/tool/conclude"
)

// requiring is ag where only the conclude tool concludes ([agent]
// require_conclude).
func requiring(ag *EventLoopAgent) *EventLoopAgent {
	ag.env.RequireConclude = true
	return ag
}

var noop = &tool.Tool{Name: "noop", Description: "does nothing", ParamType: &struct{}{},
	Execute: func(context.Context, json.RawMessage) (*tool.Result, error) { return &tool.Result{Content: "ok"}, nil }}

func repeat(n int, r llm.Response) []llm.Response {
	out := make([]llm.Response, n)
	for i := range out {
		out[i] = r
	}
	return out
}

// A reply without tool calls doesn't conclude: it gets a note asking for
// conclude, which then concludes.
func TestRequireConclude_ReplyGetsANote(t *testing.T) {
	withJobs(t)
	mock := turns(reply("It is 42 m."), concludeTurn("It is 42 m tall."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, requiring(ag))
	if got := checkedMessages(store, runID, "worker", event.CheckReply); len(got) != 1 || got[0] != harness(replyNote) {
		t.Errorf("notes = %q; want one", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m tall." || cr.Verdict != db.SessionConcluded {
		t.Errorf("child_result = %+v; want conclude's result", cr)
	}
}

// The fifth reply in a row fails the session; the note on the fourth says it
// would.
func TestRequireConclude_FifthReplyInARowFails(t *testing.T) {
	withJobs(t)
	mock := turns(repeat(maxReplies, reply("It is 42 m."))...)
	ag, store, runID, _ := concluder(t, mock)
	_ = requiring(ag).Run(context.Background())
	if s, _ := store.GetSession(context.Background(), runID, "worker"); s.Status != db.SessionCrashed {
		t.Fatalf("worker is %s; want crashed", s.Status)
	}
	notes := checkedMessages(store, runID, "worker", event.CheckReply)
	if len(notes) != maxReplies-1 {
		t.Fatalf("notes = %q; want %d", notes, maxReplies-1)
	}
	for i, n := range notes {
		if warns := strings.Contains(n, "this session fails"); warns != (i == maxReplies-2) {
			t.Errorf("note %d warns %v: %q", i+1, warns, n)
		}
	}
	if cr := childResult(t, store, runID); cr.Verdict != db.SessionCrashed || !strings.Contains(cr.Content, "kept replying without calling conclude") {
		t.Errorf("child_result = %+v; want the crash, saying why", cr)
	}
}

// A turn with tool calls starts the count again: four replies, some work,
// four more, and conclude concludes.
func TestRequireConclude_WorkStartsTheCountAgain(t *testing.T) {
	withJobs(t)
	rs := repeat(maxReplies-1, reply("thinking"))
	rs = append(rs, toolTurn("noop", "{}"))
	rs = append(rs, repeat(maxReplies-1, reply("thinking"))...)
	rs = append(rs, concludeTurn("It is 42 m."))
	ag, store, runID, _ := concluder(t, turns(rs...), noop)
	runToEnd(t, requiring(ag))
	if cr := childResult(t, store, runID); cr.Verdict != db.SessionConcluded || cr.Content != "It is 42 m." {
		t.Errorf("child_result = %+v; want concluded", cr)
	}
	notes := checkedMessages(store, runID, "worker", event.CheckReply)
	if len(notes) != 2*(maxReplies-1) || !strings.Contains(notes[maxReplies-2], "this session fails") ||
		strings.Contains(notes[maxReplies-1], "this session fails") {
		t.Errorf("notes = %q; want two runs of %d, each ending in the warning", notes, maxReplies-1)
	}
}

// A reply a crash left at the end of the stream isn't taken as the result on
// restart either: it gets its note, and conclude concludes.
func TestRequireConclude_ReplyLeftByACrash(t *testing.T) {
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
	if err := store.AppendEventAtStep(ctx, runID, "worker", 1, &event.AssistantEvent{Content: "It is 42 m."}); err != nil {
		t.Fatal(err)
	}
	mock := turns(concludeTurn("It is 42 m tall."))
	runToEnd(t, requiring(concluderIn(store, runID, registry, mock)))
	if got := checkedMessages(store, runID, "worker", event.CheckReply); len(got) != 1 {
		t.Errorf("notes = %q; want one, for the reply", got)
	}
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m tall." {
		t.Errorf("child_result = %+v; want conclude's result, not the reply", cr)
	}
}

// Only an autonomous agent that concludes with a result is held to it: not a
// chatbot, nor a verifier, nor an agent without the tool; and only with the
// setting on.
func TestRequireConclude_WhoItHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  testCfg
		on   bool
		want bool
	}{
		{"autonomous", testCfg{Tools: []*tool.Tool{conclude.New()}}, true, true},
		{"setting off", testCfg{Tools: []*tool.Tool{conclude.New()}}, false, false},
		{"chatbot", testCfg{Interactive: true}, true, false},
		{"verifier", testCfg{AgentType: typedVerifierType, Tools: []*tool.Tool{conclude.NewVerdict()}}, true, false},
		{"no conclude tool", testCfg{}, true, false},
	} {
		ag := newT(tc.cfg)
		ag.env.RequireConclude = tc.on
		if got := ag.requiresConclude(); got != tc.want {
			t.Errorf("%s: requiresConclude = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// A session the fifth reply crashed, resumed (as a crashed root is when the
// server restarts), gets one turn: a reply crashes it again at once, with no
// new note, and work or conclude carries on.
func TestRequireConclude_ResumedAfterTheCrash(t *testing.T) {
	withJobs(t)
	ag, store, runID, registry := concluder(t, turns(repeat(maxReplies, reply("It is 42 m."))...))
	_ = requiring(ag).Run(context.Background())
	resume := func(mock *llm.MockProvider) string {
		t.Helper()
		if _, err := store.AppendEvent(context.Background(), runID, "worker", &event.RecoverEvent{Content: "[RESUMED]"}); err != nil {
			t.Fatal(err)
		}
		_ = requiring(concluderIn(store, runID, registry, mock)).Run(context.Background())
		if mock.CallCount() != 1 {
			t.Errorf("model calls = %d; want the one turn", mock.CallCount())
		}
		s, _ := store.GetSession(context.Background(), runID, "worker")
		return s.Status
	}
	if got := resume(turns(reply("Still 42 m."))); got != db.SessionCrashed {
		t.Errorf("resumed, then a reply: %s; want crashed again", got)
	}
	if got := checkedMessages(store, runID, "worker", event.CheckReply); len(got) != maxReplies-1 {
		t.Errorf("notes = %d; want the %d from before, no new one", len(got), maxReplies-1)
	}
	if got := resume(turns(concludeTurn("It is 42 m tall."))); got != db.SessionConcluded {
		t.Errorf("resumed, then conclude: %s; want concluded", got)
	}
}
