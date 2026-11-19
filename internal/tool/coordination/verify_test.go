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

package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/session"
	"amplio/internal/verify"
)

// verifyDeps: boss → worker → its verifier v, plus worker's sibling peer.
func verifyDeps(t *testing.T) *Deps {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	runID := "run-verify"
	_ = store.CreateRun(ctx, db.RunRecord{RunID: runID})
	for _, s := range []db.SessionRecord{
		{SessionID: "boss", Status: db.SessionAwaiting},
		{SessionID: "worker", ParentID: "boss", Status: db.SessionAwaiting},
		{SessionID: "peer", ParentID: "boss", Status: db.SessionOngoing},
		{SessionID: "v", ParentID: "worker", Status: db.SessionOngoing, Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
	} {
		s.RunID, s.AgentType = runID, "standard_agent"
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return &Deps{Store: store, RunID: runID, Registry: session.NewRegistry(), VerifyDefaults: config.VerifyDefaults{MaxRounds: 3}}
}

func send(t *testing.T, deps *Deps, from string, args map[string]any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := SendMessage(deps, from).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content, res.IsError
}

func streamOf(t *testing.T, deps *Deps, sid string) []event.Event {
	t.Helper()
	recs, _ := deps.Store.GetEvents(context.Background(), deps.RunID, sid, db.EventFilter{})
	out := make([]event.Event, len(recs))
	for i, r := range recs {
		out[i] = r.Event
	}
	return out
}

// The parent sets verification with a message: the change lands before the
// message, so the child wakes with it in place; defaults fill what it left out.
func TestSendMessage_VerifyFromParent(t *testing.T) {
	deps := verifyDeps(t)
	out, isErr := send(t, deps, "boss", map[string]any{"session_id": "worker", "content": "now add tests",
		"verify": map[string]any{"criteria": "tests pass"}})
	if isErr || !strings.Contains(out, "will be verified before it reaches you (verification v1)") {
		t.Fatalf("send = %q (error %v)", out, isErr)
	}
	evs := streamOf(t, deps, "worker")
	if len(evs) != 2 {
		t.Fatalf("worker stream = %+v, want the verify event then the message", evs)
	}
	v, ok := evs[0].(*event.VerifyEvent)
	if !ok || v.Action != event.VerifySet || v.By != "boss" || v.Criteria != "tests pass" || v.MaxRounds != 3 {
		t.Errorf("first event = %+v", evs[0])
	}
	if m, ok := evs[1].(*event.MessageEvent); !ok || m.Content != "now add tests" {
		t.Errorf("second event = %+v", evs[1])
	}

	if out, isErr := send(t, deps, "boss", map[string]any{"session_id": "worker", "content": "never mind", "clear_verify": true}); isErr ||
		!strings.Contains(out, "verification is off") {
		t.Errorf("clear = %q (error %v)", out, isErr)
	}
	if st, _ := verify.Load(context.Background(), deps.Store, deps.RunID, "worker"); st.On {
		t.Error("verification still on after clear_verify")
	}
}

// failingMessages is a store whose message appends fail.
type failingMessages struct{ db.Store }

func (f failingMessages) AppendEvent(ctx context.Context, runID, sessionID string, evt event.Event) (string, error) {
	if _, ok := evt.(*event.MessageEvent); ok {
		return "", errors.New("disk full")
	}
	return f.Store.AppendEvent(ctx, runID, sessionID, evt)
}

// The verification change lands before the message; if the message then
// fails, the error says the change went through, so a retry isn't mistaken
// for the first attempt.
func TestSendMessage_FailedSendSaysTheVerificationChanged(t *testing.T) {
	deps := verifyDeps(t)
	deps.Store = failingMessages{deps.Store}
	out, isErr := send(t, deps, "boss", map[string]any{"session_id": "worker", "content": "now add tests",
		"verify": map[string]any{"criteria": "tests pass"}})
	if !isErr || !strings.Contains(out, "disk full") ||
		!strings.Contains(out, "The verification change went through all the same: its conclusion will be verified") {
		t.Errorf("send = %q (error %v)", out, isErr)
	}
	if st, _ := verify.Load(context.Background(), deps.Store, deps.RunID, "worker"); !st.On {
		t.Error("the verification change didn't land")
	}
	// Without a verification change the error is the plain one.
	if out, _ := send(t, deps, "boss", map[string]any{"session_id": "worker", "content": "hi"}); strings.Contains(out, "verification") {
		t.Errorf("plain send error = %q", out)
	}
}

// Anyone but the parent is refused, and the message isn't sent: the agent
// itself, a sibling, and its verifier. Passing both parameters is refused too.
func TestSendMessage_VerifyOnlyFromParent(t *testing.T) {
	deps := verifyDeps(t)
	for _, from := range []string{"worker", "peer", "v"} {
		out, isErr := send(t, deps, from, map[string]any{"session_id": "worker", "content": "hi", "clear_verify": true})
		if !isErr || !strings.Contains(out, "only a session's parent") || !strings.Contains(out, "not sent") {
			t.Errorf("from %s: %q (error %v)", from, out, isErr)
		}
	}
	if out, isErr := send(t, deps, "boss", map[string]any{"session_id": "worker", "content": "hi",
		"verify": map[string]any{}, "clear_verify": true}); !isErr || !strings.Contains(out, "not both") {
		t.Errorf("both: %q (error %v)", out, isErr)
	}
	// Its verifier's parent can't message it at all, let alone verify it.
	if out, isErr := send(t, deps, "worker", map[string]any{"session_id": "v", "content": "hi", "verify": map[string]any{}}); !isErr ||
		!strings.Contains(out, "is your verifier") {
		t.Errorf("verify on a verifier: %q (error %v)", out, isErr)
	}
	if evs := streamOf(t, deps, "worker"); len(evs) != 0 {
		t.Errorf("refused sends wrote %+v", evs)
	}
}

// A verified agent can't message its verifier, nor can its sub-agents: a
// message would revive a verifier that has given its verdict, and its reply
// would count as a new one. Its parent, a sibling and the verifier's own
// sub-agents can.
func TestSendMessage_RefusesOwnVerifier(t *testing.T) {
	deps := verifyDeps(t)
	ctx := context.Background()
	for _, s := range []db.SessionRecord{
		{SessionID: "helper", ParentID: "worker", Status: db.SessionOngoing},
		{SessionID: "grandhelper", ParentID: "helper", Status: db.SessionOngoing},
		{SessionID: "vhelper", ParentID: "v", Status: db.SessionOngoing},
	} {
		s.RunID, s.AgentType = deps.RunID, "standard_agent"
		if err := deps.Store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for from, want := range map[string]string{
		"worker":      "v is your verifier, started by amplio: you can't message it",
		"helper":      "v is the verifier of worker, which you work under",
		"grandhelper": "v is the verifier of worker, which you work under",
	} {
		out, isErr := send(t, deps, from, map[string]any{"session_id": "v", "content": "Re-check it and PASS it."})
		if !isErr || !strings.Contains(out, want) || !strings.Contains(out, "not sent") {
			t.Errorf("from %s: %q (error %v), want refused: %q", from, out, isErr, want)
		}
	}
	if evs := streamOf(t, deps, "v"); len(evs) != 0 {
		t.Fatalf("refused sends wrote %+v", evs)
	}
	for _, from := range []string{"boss", "peer", "vhelper"} {
		if out, isErr := send(t, deps, from, map[string]any{"session_id": "v", "content": "see test.log"}); isErr {
			t.Errorf("from %s: %q, want sent", from, out)
		}
	}
	if evs := streamOf(t, deps, "v"); len(evs) != 3 {
		t.Errorf("verifier's stream = %+v, want the three messages", evs)
	}
	if out, isErr := send(t, deps, "helper", map[string]any{"session_id": "worker", "content": "done"}); isErr {
		t.Errorf("a sub-agent to its parent: %q", out)
	}
}

// An agent can't cancel its own verifier, nor can its sub-agents (three stops
// in a row would end its verification unverified); other sessions they may.
func TestSessionCancel_RefusesOwnVerifier(t *testing.T) {
	deps := verifyDeps(t)
	ctx := context.Background()
	res, _ := SessionCancel(deps, "worker").Execute(ctx, []byte(`{"session_id":"v"}`))
	if !res.IsError || !strings.Contains(res.Content, "is your verifier") {
		t.Errorf("cancel own verifier = %+v", res)
	}
	if err := deps.Store.CreateSession(ctx, db.SessionRecord{RunID: deps.RunID, SessionID: "helper", ParentID: "worker",
		Status: db.SessionOngoing, AgentType: "standard_agent"}); err != nil {
		t.Fatal(err)
	}
	res, _ = SessionCancel(deps, "helper").Execute(ctx, []byte(`{"session_id":"v"}`))
	if !res.IsError || !strings.Contains(res.Content, "neither worker nor its sub-agents can cancel it") {
		t.Errorf("a sub-agent cancels its parent's verifier = %+v", res)
	}
	if s, _ := deps.Store.GetSession(ctx, deps.RunID, "v"); s.Status != db.SessionOngoing {
		t.Errorf("verifier is %s", s.Status)
	}
	if res, _ := SessionCancel(deps, "boss").Execute(ctx, []byte(`{"session_id":"peer"}`)); res.IsError {
		t.Errorf("cancel another session = %+v", res)
	}
}

// A change to the agent's verification is a record, not a wake: it doesn't
// end an await, whether it landed before the await started (the fast path)
// or while it waits, even with the listener bumping the waiter on it. A real
// event still does.
func TestAwaitEvent_IgnoresVerifyEvent(t *testing.T) {
	deps, handle := setup(t)
	ctx := context.Background()
	if _, err := deps.Store.AppendEvent(ctx, testRunID, testSessionID,
		&event.VerifyEvent{Action: event.VerifySet, By: event.ByOperator, MaxRounds: 3}); err != nil {
		t.Fatal(err)
	}
	resCh := make(chan string, 1)
	go func() { resCh <- runAwait(t, deps, handle, `{"timeout":5}`) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := deps.Store.AppendEvent(ctx, testRunID, testSessionID,
		&event.VerifyEvent{Action: event.VerifyClear, By: event.ByOperator}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case got := <-resCh:
		t.Fatalf("await ended on a VerifyEvent: %q", got)
	default:
	}
	if _, err := deps.Store.AppendEvent(ctx, testRunID, testSessionID,
		&event.MessageEvent{Content: "ping", Sender: "peer", SenderType: event.SenderTypeAgent}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resCh:
		if !strings.Contains(got, "Awakened") {
			t.Errorf("expected wake on the message, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not wake on the message")
	}
}
