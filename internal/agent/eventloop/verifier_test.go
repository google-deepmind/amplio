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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/tool/conclude"
	"amplio/internal/tool/coordination"
	"amplio/internal/verify"
)

// typedVerifierType stands in for the verifier agent type: a verifier (the
// trait) whose conclude takes a typed verdict.
const typedVerifierType = "eventloop_test_typed_verifier"

func init() {
	agent.Register(typedVerifierType, func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		e := *env
		e.LLM = verifierLLM
		return New(&e, Config{SessionID: cfg.SessionID, Task: cfg.Task, ParentID: cfg.ParentID, Handle: cfg.Handle,
			VerifierFor: cfg.VerifierFor, AgentType: typedVerifierType, SystemPrompt: "verifier",
			Tools: []*tool.Tool{conclude.NewVerdict()}}), nil
	}, agent.Traits{Verifier: true})
}

// newTypedRig is a verifyRig whose verifiers are of the verifier type.
func newTypedRig(t *testing.T, verifier llm.Provider) *verifyRig {
	r := newVerifyRig(t, verifier)
	verifierAgentType = typedVerifierType // newVerifyRig restores it
	return r
}

// verdictTurn is a verifier's turn calling conclude with a verdict.
func verdictTurn(verdict, findings string) llm.Response {
	b, _ := json.Marshal(event.ConcludeArgs{Verdict: verdict, Findings: findings})
	return concludeArgsTurn(string(b))
}

func concludeArgsTurn(args string) llm.Response {
	return llm.Response{StopReason: "tool_use", ToolCalls: []llm.ToolCall{
		{ID: fmt.Sprintf("verdict-%d", callSeq.Add(1)), Name: event.ConcludeTool, Arguments: args}}}
}

// A reply taken as conclude() after the typed verifier stopped re-engages it
// with the pending candidate, and its pass delivers that candidate, not the
// reply (the verifying record carries what it gave).
func TestVerifier_ReplyTakenAsConcludeAfterAStopKeepsTheCandidate(t *testing.T) {
	vLLM := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}}, // crashes on the first candidate
		verdictTurn("PASS", "reply 1 says 42 m, which is right"),
	}}
	r := newTypedRig(t, vLLM)
	r.wait(r.start(&replyOnly{MockProvider: &llm.MockProvider{Model: "m"}}, setBy("boss", "", 3)))
	v := r.verifiers()
	var msgs []string
	if len(v) == 1 {
		msgs = r.messages(v[0])
	}
	// Reply 1, maybe sent twice: again if the verifier's goroutine was still
	// on its way out when it was first sent.
	ok := len(v) == 1 && len(msgs) >= 1
	for _, m := range msgs {
		ok = ok && strings.Contains(m, "It is 42 m (reply 1).")
	}
	if !ok {
		t.Fatalf("verifiers %v, candidate messages %q: want the one, re-engaged with reply 1", v, msgs)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m (reply 1)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("delivered %q marked %v, want reply 1, the candidate the verifier passed", cr.Content, cr.Notes)
	}
}

// A typed verifier whose conclude call a new candidate overtakes is told its
// verdict doesn't count; the same typed verdict given again straight away is
// held too, and the verdict it then gives counts for the new candidate.
func TestVerifier_TypedVerdictOnAnEarlierCandidateDoesntCount(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(verdictTurn("PASS", "42 is right"),
		verdictTurn("PASS", "42 is right"), verdictTurn("PASS", "42 m, with its unit")), gate: gate}}
	r := newTypedRig(t, vLLM)
	done := r.start(turns(concludeTurn("It is 42."), concludeTurn("It is 42 m.")), setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	waitFor(t, "the verifier's first call", func() bool { return vLLM.entered.Load() == 1 })
	r.message("give the unit")
	waitFor(t, "candidate 2 with the verifier", func() bool { return len(r.messages(v)) == 1 })
	for range 3 {
		gate <- struct{}{}
	}
	r.wait(done)
	want := []string{verify.VerdictOnEarlier("a new candidate"), verify.Redelivered, "Concluded."}
	if got := concludeResultsOf(r.store, r.runID, v); !slices.Equal(got, want) {
		t.Errorf("the verifier's conclude results = %q, want %q", got, want)
	}
	if o := r.outcomes(); len(o) != 1 || o[0] != v+" PASS" {
		t.Errorf("outcome records = %v, want the third verdict's alone", o)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want candidate 2, verified", cr)
	}
}

// A verifier's typed FAIL is a rejection whatever its findings say: findings
// that start "PASS: Wait, ..." can't make it one. The verdicts reach the
// worker as the harness tells them, in its conclude calls' results; the
// verifier's own conclusion reaches it only as an outcome record, not a
// child_result.
func TestVerifier_TypedFailCantBeMisread(t *testing.T) {
	wavering := "PASS: Wait, let me reconsider. FAIL: the height has no unit."
	vLLM := turns(verdictTurn("FAIL", wavering), verdictTurn("PASS", "42 m, with a unit"))
	r := newTypedRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42."), concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, setBy("boss", "the answer gives a unit", 3)))

	v := r.verifier()
	if s, _ := r.store.GetSession(context.Background(), r.runID, v); s.AgentType != typedVerifierType {
		t.Errorf("verifier %s is a %s, want the verifier type", v, s.AgentType)
	}
	results := callResults(r.store, r.runID)
	if len(results) != 2 || results[0] != verify.Rejected(v, verify.State{Version: 1, Candidates: 1, Rejections: 1,
		Request: &event.VerifyEvent{MaxRounds: 3}}, wavering) || results[1] != verify.Verified(v, cand(2, 2)) {
		t.Errorf("conclude call results = %q, want the rejection with its findings, then the pass", results)
	}
	outcomes := r.callOutcomes()
	if len(outcomes) != 2 || withoutAt(outcomes[0]) != (event.VerifyOutcome{Verifier: v, Verdict: event.VerdictFail,
		Findings: wavering, Round: 1, MaxRounds: 3}) || outcomes[1].Verdict != event.VerdictPass || outcomes[1].Round != 2 {
		t.Errorf("the results' outcomes = %+v, %+v", outcomes[0], outcomes[1])
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " FAIL", v + " PASS"}) {
		t.Errorf("outcome records = %v", got)
	}
	r.noVerifierResults()
	if cr := r.result(); cr.Content != "It is 42 m." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: v + " (round 2)"}) {
		t.Errorf("boss's child_result = %+v, want the verified candidate", cr)
	}
	// The verifier is asked for the typed verdict, first and when re-engaged.
	reqs := vLLM.Recorded()
	typed := `give your verdict with conclude(verdict: "PASS" or "FAIL", findings)`
	for i, req := range reqs {
		if c := lastUserContent(req); !strings.Contains(c, typed) || strings.Contains(c, "first line") {
			t.Errorf("verifier request %d doesn't ask for the typed verdict:\n%s", i, c)
		}
	}
	// The worker never read the verifier's text but through the harness.
	for _, m := range aLLM.Recorded()[1].Messages {
		if strings.Contains(m.Content, "child_result") {
			t.Errorf("the worker read a child_result: %q", m.Content)
		}
	}
}

// A verifier's reply without tool calls isn't its verdict: it is nudged to
// call conclude, and after two nudges in a row its next reply concludes it
// with no verdict, a FAIL whatever the reply says: a PASS on its first line,
// or one that wavers ("PASS: Wait, … FAIL: …"), is no pass.
func TestVerifier_RepliesAreNudgedThenFail(t *testing.T) {
	for _, c := range []struct {
		name    string
		replies []string
	}{
		{"a first-line PASS", []string{"PASS: 42 m is right", "It's right.", "PASS: 42 m is right"}},
		{"no verdict", []string{"PASS: 42 m is right", "It's right.", "Looks right to me."}},
		{"a wavering reply", []string{"Checking.", "Still checking.", "PASS: Wait, let me reconsider. FAIL: the height has no unit."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newTypedRig(t, script(c.replies...))
			r.wait(r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 1)))
			v := r.verifier()
			nudges := checkedMessages(r.store, r.runID, v, event.CheckConclude)
			if len(nudges) != maxPendingNudges || nudges[0] != harness(verify.ReplyIsNoVerdict) {
				t.Errorf("verifier nudges = %q, want %d", nudges, maxPendingNudges)
			}
			findings := event.NoVerdict + ": " + c.replies[2]
			cr := r.result()
			if len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified || !strings.Contains(cr.Notes[0].Text, findings) {
				t.Errorf("boss's child_result notes = %+v, want not verified, the reply labelled as no verdict", cr.Notes)
			}
			if got := r.outcomes(); !slices.Equal(got, []string{v + " FAIL"}) {
				t.Errorf("outcome records = %v, want the FAIL", got)
			}
			if o := r.callOutcomes()[0]; o == nil || o.Verdict != event.VerdictFail || o.Findings != findings {
				t.Errorf("the call's outcome = %+v, want the FAIL with the reply as findings", o)
			}
		})
	}
}

// A verifier's conclude call must give a verdict, and a FAIL its findings;
// otherwise the call gets an error and the verifier goes on.
func TestVerifier_ConcludeNeedsAVerdict(t *testing.T) {
	vLLM := turns(concludeArgsTurn(`{"verdict":"MAYBE","findings":"unsure"}`), verdictTurn("FAIL", " "),
		verdictTurn("PASS", "42 m"))
	r := newTypedRig(t, vLLM)
	r.wait(r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3)))
	v := r.verifier()
	events, _ := r.store.GetEvents(context.Background(), r.runID, v, db.EventFilter{})
	var errs []string
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok && tr.IsError {
			errs = append(errs, tr.Content)
		}
	}
	if len(errs) != 2 || !strings.Contains(errs[0], "give your verdict") || !strings.Contains(errs[1], "a FAIL needs findings") {
		t.Errorf("the verifier's errors = %q", errs)
	}
	if cr := r.result(); len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("boss's child_result = %+v, want verified by the third call", cr)
	}
}

// A verdict that reaches the agent while it waits in await_event wakes it (the
// outcome record isn't record-only), and its next turn reads it as a verify
// message: here a rejection, with its findings and round, though the agent's
// last conclude call returned early (its result told nothing of the verdict).
func TestVerify_VerdictWakesAwaitEvent(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newTypedRig(t, &gatedLLM{MockProvider: turns(verdictTurn("FAIL", "no unit"), verdictTurn("PASS", "42 m")), gate: gate})
	aLLM := turns(concludeTurn("It is 42."), toolTurn("await_event", `{"timeout":30}`), concludeTurn("It is 42 m."))
	var ag *EventLoopAgent
	await := coordination.AwaitEvent(&coordination.Deps{Store: r.store, RunID: r.runID, Registry: r.registry},
		"worker", func() *session.Handle { return ag.Handle() })
	ag = r.worker(aLLM, setBy("boss", "", 3), await)
	done := r.startAgent(ag)
	r.waiting(0)
	r.message("any news?")
	waitFor(t, "the worker in await_event", func() bool {
		return aLLM.CallCount() == 2 && r.status("worker") == db.SessionAwaiting && len(callResults(r.store, r.runID)) == 1
	})
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)

	v := r.verifier()
	rejected := harness(verify.Rejected(v, verify.State{Version: 1, Candidates: 1, Rejections: 1, Request: &event.VerifyEvent{MaxRounds: 3}}, "no unit"))
	if msgs := r.messages("worker"); !slices.Equal(msgs, []string{rejected}) {
		t.Errorf("verify messages = %q, want the rejection", msgs)
	}
	if o := r.messageOutcomes(); len(o) != 1 || withoutAt(o[0]) != (event.VerifyOutcome{Verifier: v, Verdict: event.VerdictFail,
		Findings: "no unit", Round: 1, MaxRounds: 3}) {
		t.Errorf("the message's outcome = %+v", o)
	}
	req := aLLM.Recorded()[2]
	if last := req.Messages[len(req.Messages)-1]; last.Content != rejected {
		t.Errorf("the turn after the wake ends with %q, want the rejection", last.Content)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	r.noVerifierResults()
}

// The agent is told of a verdict once: a conclude call whose result told it
// covers it; one that returned before the verdict (an early return) doesn't,
// so the verdict landing next is told; a message at the step that told it
// covers it; and a verifier's result written as a child_result before
// verifiers posted outcome records is told, unless a message (written then,
// without an outcome) followed it.
func TestVerify_VerdictToldOnce(t *testing.T) {
	pass := func(at time.Time) *event.VerifyOutcome {
		return &event.VerifyOutcome{Verifier: "dusky-gecko", Verdict: event.VerdictPass, At: at}
	}
	record := func(at time.Time) event.Event { return event.VerifierOutcome("dusky-gecko", "PASS: right", "", at) }
	legacy := func(time.Time) event.Event {
		return &event.ChildResultEvent{ChildSessionID: "dusky-gecko", Verdict: db.SessionConcluded, Content: "PASS: right"}
	}
	for _, c := range []struct {
		name    string
		answer  func(at time.Time) *event.ToolResultEvent // the conclude call's result, at its step
		arrival func(at time.Time) event.Event            // the verifier's outcome, at the next step
		after   func(at time.Time) event.Event            // written after the arrival, at its step
		told    bool
	}{
		{"the call told it", func(at time.Time) *event.ToolResultEvent {
			return &event.ToolResultEvent{Content: "passed", Check: event.CheckVerify, Outcome: pass(at)}
		}, record, nil, false},
		{"the call returned early", func(time.Time) *event.ToolResultEvent {
			return &event.ToolResultEvent{Content: verify.Interrupted("dusky-gecko", cand(1, 1), "a message"), Check: event.CheckVerify}
		}, record, nil, true},
		{"told at the step already", func(time.Time) *event.ToolResultEvent {
			return &event.ToolResultEvent{Content: "early", Check: event.CheckVerify}
		}, record, func(at time.Time) event.Event { return verifyMessage("told", pass(at)) }, false},
		{"a child_result from before outcome records", func(time.Time) *event.ToolResultEvent {
			return &event.ToolResultEvent{Content: "early", Check: event.CheckVerify}
		}, legacy, nil, true},
		{"and its message from before outcomes", func(time.Time) *event.ToolResultEvent {
			return &event.ToolResultEvent{Content: "early", Check: event.CheckVerify}
		}, legacy, func(time.Time) event.Event { return verifyMessage("told", nil) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newVerifyRig(t, script())
			r.strandVerifier()
			ctx := context.Background()
			if err := r.store.AppendEventAtStep(ctx, r.runID, "dusky-gecko", 2, &event.AssistantEvent{Content: "PASS: right"}); err != nil {
				t.Fatal(err)
			}
			v, _ := r.store.GetSession(ctx, r.runID, "dusky-gecko")
			at := v.StatusChangedAt
			events, _ := r.store.GetEvents(ctx, r.runID, "worker", db.EventFilter{})
			call, _, _ := db.OpenConclude(events)
			answer := c.answer(at)
			answer.ToolCallID = call.ID
			adds := []db.SelfEvent{{Event: answer, Step: 1}, {Event: c.arrival(at), Step: 2}}
			if c.after != nil {
				adds = append(adds, db.SelfEvent{Event: c.after(at), Step: 2})
			}
			for _, add := range adds {
				if err := r.store.AppendEventAtStep(ctx, r.runID, "worker", add.Step, add.Event); err != nil {
					t.Fatal(err)
				}
			}
			before := len(r.messages("worker"))
			if err := r.workerAgent().noteVerdict(ctx, 2); err != nil {
				t.Fatal(err)
			}
			msgs := r.messages("worker")[before:]
			if told := len(msgs) == 1 && msgs[0] == harness(verify.Passed("dusky-gecko", cand(1, 1))); told != c.told || len(msgs) > 1 {
				t.Errorf("verify messages = %q, want told=%v (once)", msgs, c.told)
			}
		})
	}
}

// A verifier's stop reaches the agent as an outcome record (a Notice: it
// wakes only a waiting agent) and counts as a stop: told while the agent
// works, with why it stopped; three in a row end the verification.
func TestVerify_StopWhileWorkingIsTold(t *testing.T) {
	// Two verifier turns may be needed: cancelled mid-call, the revived
	// verifier redoes that call first, then reads the candidate.
	gate := make(chan struct{}, 2)
	vLLM := &gatedLLM{MockProvider: turns(verdictTurn("PASS", "42 m"), verdictTurn("PASS", "42 m")), gate: gate}
	r := newTypedRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("await_event", `{"timeout":30}`), concludeTurn(""))
	var ag *EventLoopAgent
	await := coordination.AwaitEvent(&coordination.Deps{Store: r.store, RunID: r.runID, Registry: r.registry},
		"worker", func() *session.Handle { return ag.Handle() })
	ag = r.worker(aLLM, setBy("boss", "", 3), await)
	done := r.startAgent(ag)
	r.waiting(0)
	r.message("any news?")
	waitFor(t, "the worker in await_event", func() bool {
		return aLLM.CallCount() == 2 && r.status("worker") == db.SessionAwaiting
	})
	v := r.verifier()
	if err := session.CancelSession(r.store, r.registry, r.runID, v, "operator stopped it"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the verifier revived by conclude()", func() bool { return r.status(v) == db.SessionOngoing })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)

	stopped := harness(verify.Stopped(v, verify.Label{Version: 1, Candidate: 1}, "cancelled: operator stopped it"))
	if msgs := r.messages("worker"); !slices.Equal(msgs, []string{stopped}) || !strings.Contains(msgs[0], "(cancelled: operator stopped it)") {
		t.Errorf("verify messages = %q, want the stop, with why", msgs)
	}
	if o := r.messageOutcomes(); len(o) != 1 || o[0].Verdict != db.SessionCancelled || o[0].Findings != "operator stopped it" {
		t.Errorf("the message's outcome = %+v", o)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " cancelled", v + " PASS"}) {
		t.Errorf("outcome records = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("boss's child_result = %+v, want verified after the re-engagement", cr)
	}
	r.noVerifierResults()
}

// After a restart, a verifier of the verifier type is revived as one (its
// conclude still typed), and a verdict that landed while the agent's process
// was down settles the resumed call.
func TestVerifier_RestartKeepsTheType(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newTypedRig(t, &gatedLLM{MockProvider: turns(verdictTurn("FAIL", "PASS: no, FAIL: no unit"),
		verdictTurn("PASS", "42 m")), gate: gate})
	r.stopWhileWaiting()
	gate <- struct{}{}
	v := r.verifier()
	waitFor(t, "the verdict, while the worker is down", func() bool { return r.status(v) == db.SessionConcluded })
	gate <- struct{}{}
	aLLM := turns(concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, nil))
	req := aLLM.Recorded()[0]
	assertPaired(t, req)
	if !strings.Contains(lastToolResult(req), "rejected your conclusion (v1, candidate 1; round 1 of 3):\n\nPASS: no, FAIL: no unit") {
		t.Errorf("the resumed call's result = %q, want the typed FAIL's findings", lastToolResult(req))
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " FAIL", v + " PASS"}) {
		t.Errorf("outcome records = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("boss's child_result = %+v", cr)
	}
	r.noCrashMarker()
	r.noVerifierResults()
}

// A verifier started before the verifier type existed is a standard agent,
// whose result's first line is read: re-engaged, it is asked for that form
// again (its candidate message sent again as it was), and its reply concludes
// it, the verdict read from its first line.
func TestVerify_OldStyleVerifierKeepsWorking(t *testing.T) {
	r := newTypedRig(t, script("PASS: 42 m, with a unit"))
	r.strandVerifier() // dusky-gecko, a verifier that concludes with text
	aLLM := turns(concludeTurn(""))
	r.wait(r.start(aLLM, nil))
	msgs := r.messages("dusky-gecko")
	if len(msgs) != 2 || msgs[1] != msgs[0] || !strings.Contains(msgs[1], "must be exactly `PASS` or `FAIL`") {
		t.Errorf("the old verifier's re-engagement = %q, want the first-line form", msgs)
	}
	if cr := r.result(); cr.Content != "It is 42 m." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: "dusky-gecko (round 1)"}) {
		t.Errorf("boss's child_result = %+v", cr)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{"dusky-gecko PASS"}) {
		t.Errorf("outcome records = %v", got)
	}
}

// A reply taken as a candidate waits in the harness, and the verdict it waited
// for is the answer to the reply: a verify message with the findings and the
// outcome, told once.
func TestVerify_ReplysVerdictIsTold(t *testing.T) {
	r := newTypedRig(t, turns(verdictTurn("FAIL", "no unit"), verdictTurn("PASS", "42 m")))
	aLLM := turns(llm.Response{Content: "It is 42.", StopReason: "end_turn"}, concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	v := r.verifier()
	msgs, outcomes := r.messages("worker"), r.messageOutcomes()
	rejected := harness(verify.Rejected(v, verify.State{Version: 1, Candidates: 1, Rejections: 1, Request: &event.VerifyEvent{MaxRounds: 3}}, "no unit"))
	if len(msgs) != 2 || msgs[0] != harness(verify.Submitted(v, cand(1, 1))) || msgs[1] != rejected {
		t.Errorf("verify messages = %q, want the submission, then the rejection once", msgs)
	}
	if len(outcomes) != 2 || outcomes[0] != nil || outcomes[1] == nil || outcomes[1].Verdict != event.VerdictFail {
		t.Errorf("the messages' outcomes = %+v, want the rejection's only", outcomes)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("boss's child_result = %+v", cr)
	}
	r.noVerifierResults()
}

// withoutAt is o without its time, for comparing the rest.
func withoutAt(o *event.VerifyOutcome) event.VerifyOutcome {
	c := *o
	c.At = time.Time{}
	return c
}

// A standard agent's conclude call that also gives a verdict, and a verifier's
// that also gives a result, are refused: neither form stands in for the other.
func TestConclude_OtherFormIsRefused(t *testing.T) {
	withJobs(t)
	mock := turns(concludeArgsTurn(`{"result":"It is 42 m tall.","verdict":"PASS"}`), concludeTurn("It is 42 m tall."))
	ag, store, runID, _ := concluder(t, mock)
	runToEnd(t, ag)
	if cr := childResult(t, store, runID); cr.Content != "It is 42 m tall." {
		t.Errorf("child_result = %q, want the result of the call in its form", cr.Content)
	}
	if got := callResults(store, runID); len(got) != 2 || !strings.Contains(got[0], "conclude takes only `result`") {
		t.Errorf("conclude call results = %q, want the first refused", got)
	}

	r := newTypedRig(t, turns(concludeArgsTurn(`{"verdict":"PASS","findings":"fine","result":"PASS"}`),
		verdictTurn("FAIL", "no unit")))
	r.wait(r.start(turns(concludeTurn("It is 42.")), setBy("boss", "", 1)))
	v := r.verifier()
	events, _ := r.store.GetEvents(context.Background(), r.runID, v, db.EventFilter{})
	var errs []string
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok && tr.IsError {
			errs = append(errs, tr.Content)
		}
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "takes no `result`") {
		t.Errorf("the verifier's errors = %q", errs)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " FAIL"}) {
		t.Errorf("outcome records = %v, want the FAIL of the call in its form", got)
	}
}

// A rejection of a candidate the agent has already replaced (it lands while
// the agent generates its new conclusion) isn't a round, but its findings are
// told, as information, before the wait for the new candidate's verdict.
func TestVerify_SupersededRejectionIsToldNotCounted(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newTypedRig(t, &gatedLLM{MockProvider: turns(verdictTurn("FAIL", "no unit"), verdictTurn("PASS", "42 m")), gate: gate})
	aLLM := &nthCallLLM{MockProvider: turns(concludeTurn("It is 42."), concludeTurn("It is 42 m.")), n: 2, hook: func() {
		gate <- struct{}{} // the verdict on the first candidate lands while the second is generated
		waitFor(r.t, "the first verdict", func() bool { return len(r.outcomes()) == 1 })
	}}
	done := r.start(aLLM, setBy("boss", "", 1))
	r.waiting(0)
	r.message("keep at it")
	waitFor(t, "the second candidate with the verifier", func() bool {
		vs := r.verifiers()
		return len(vs) == 1 && len(r.messages(vs[0])) >= 1 // >=: it may be sent twice
	})
	gate <- struct{}{}
	r.wait(done)

	v := r.verifier()
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "passed"}) {
		t.Errorf("history = %v, want no round counted for the first candidate", got)
	}
	note := harness(verify.Earlier(v, false, "no unit", true) + "\n\n" + verify.LatestWith(v, cand(2, 1)))
	if msgs := r.messages("worker"); !slices.Contains(msgs, note) {
		t.Errorf("verify messages = %q, want the earlier rejection told", msgs)
	}
	if o := r.messageOutcomes(); len(o) != 1 || !o[0].Earlier || o[0].Round != 0 {
		t.Errorf("the note's outcome = %+v, want a verdict on an earlier conclusion, no round", o)
	}
	if cr := r.result(); cr.Content != "It is 42 m." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: v + " (round 1)"}) {
		t.Errorf("boss's child_result = %+v, want the second candidate verified in round 1 (max_rounds 1)", cr)
	}
	if got := callResults(r.store, r.runID); len(got) != 2 || got[1] != verify.Verified(v, cand(2, 1)) {
		t.Errorf("conclude call results = %q", got)
	}
}

// failAppends fails the first write of a message, as a store failure would.
type failAppends struct {
	db.Store
	once sync.Once
}

func (s *failAppends) AppendEvents(ctx context.Context, runID, sessionID string, evts ...db.SelfEvent) error {
	failed := false
	for _, e := range evts {
		if _, ok := e.Event.(*event.UserEvent); ok {
			s.once.Do(func() { failed = true })
		}
	}
	if failed {
		return fmt.Errorf("%w: %w", db.ErrStore, errors.New("disk full"))
	}
	return s.Store.AppendEvents(ctx, runID, sessionID, evts...)
}

// A rejection told by message counts its round in the same write: when that
// write fails, neither is there, and the next try tells and counts it.
func TestVerify_RejectionToldAndCountedTogether(t *testing.T) {
	r := newVerifyRig(t, script())
	r.strandVerifier()
	ctx := context.Background()
	if err := r.store.AppendEventAtStep(ctx, r.runID, "dusky-gecko", 2, &event.AssistantEvent{Content: "FAIL: still no unit"}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.AppendEventAtStep(ctx, r.runID, "worker", 2,
		event.VerifierOutcome("dusky-gecko", "FAIL: still no unit", "", time.Time{})); err != nil {
		t.Fatal(err)
	}
	failing := r.workerAgent()
	failing.env.Store = &failAppends{Store: r.store}
	if err := failing.noteVerdict(ctx, 2); err == nil {
		t.Fatal("noteVerdict over a failing store succeeded")
	}
	if st := r.state(); st.Rejections != 0 || !st.Pending || len(r.messages("worker")) != 0 {
		t.Fatalf("after the failed write: rejections %d, pending %v, messages %q; want nothing written",
			st.Rejections, st.Pending, r.messages("worker"))
	}
	if err := r.workerAgent().noteVerdict(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if st, msgs := r.state(), r.messages("worker"); st.Rejections != 1 || len(msgs) != 1 ||
		!strings.Contains(msgs[0], "rejected your conclusion (v1, candidate 1; round 1 of 3):\n\nstill no unit") {
		t.Errorf("after the retry: rejections %d, messages %q; want the round counted and told", st.Rejections, msgs)
	}
}

// What a verifier is told never asks it for conclude(result): its nudges, a
// conclusion overtaken by input, and a call its cancel withdrew name its
// verdict.
func TestVerifier_TextsNameTheVerdict(t *testing.T) {
	r := newTypedRig(t, script())
	v := New(r.env(), Config{SessionID: "v", AgentType: typedVerifierType, Tools: []*tool.Tool{conclude.NewVerdict()}})
	for name, text := range map[string]string{
		"empty turn":   v.nudge(concludeNudgeText, concludeNudgeToolText, concludeNudgeVerdictText),
		"output limit": v.nudge(outputLimitNudgeText, outputLimitNudgeToolText, outputLimitNudgeVerdictText),
		"again":        v.concludeAgain(),
		"cancelled":    cancelledCall("operator", false, v.givesVerdict()),
	} {
		if strings.Contains(text, "conclude(result") || strings.Contains(text, "final result") || !strings.Contains(text, "verdict") {
			t.Errorf("%s: %q", name, text)
		}
	}
	if d := conclude.NewVerdict().Description; !strings.Contains(d, "new input that arrived while concluding") {
		t.Errorf("the verdict tool doesn't say it may return for input that arrived: %q", d)
	}
	// On a withdrawn candidate, the tool and the withdrawal message say what
	// the verifier's prompt says: a brief reply, not conclude.
	for name, text := range map[string]string{
		"verdict tool": conclude.NewVerdict().Description,
		"withdrawal":   verify.Withdrawal("worker", verify.Label{Version: 1, Candidate: 1}, 2),
	} {
		if !strings.Contains(strings.ToLower(text), "do not call `conclude`") && !strings.Contains(strings.ToLower(text), "do not call conclude") ||
			!strings.Contains(text, "brief plain-text reply") {
			t.Errorf("%s doesn't say to end the turn with a brief reply, not conclude: %q", name, text)
		}
	}
}

// A verdict record the turn can't settle is still told, as information, so
// the turn it woke has something to read: the verifier's verdict on an
// earlier candidate (here a rejection, no round), and one that comes after
// the verification ended.
func TestVerify_EarlierVerdictToldAsInformation(t *testing.T) {
	for _, c := range []struct {
		name    string
		cleared bool
		want    func(string) string
	}{
		{"on an earlier candidate", false, func(v string) string {
			return harness(verify.Earlier(v, false, "no unit", true) + "\n\n" + verify.LatestWith(v, cand(1, 1)) + " " +
				verify.WaitForLatest)
		}},
		// A clear withdrew the candidate it was on: a stale verdict.
		{"after the verification was turned off", true, func(v string) string {
			return harness(verify.StaleNote(v, false, "no unit", verify.Label{Version: 1, Candidate: 1}, 0))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newVerifyRig(t, script())
			r.strandVerifier() // dusky-gecko concluded FAIL "no unit" on the candidate before the latest
			ctx := context.Background()
			if c.cleared {
				if err := r.store.AppendEventAtStep(ctx, r.runID, "worker", 1, &event.VerifyEvent{Action: event.VerifyClear,
					By: "boss"}); err != nil {
					t.Fatal(err)
				}
			}
			v, _ := r.store.GetSession(ctx, r.runID, "dusky-gecko")
			if err := r.store.AppendEventAtStep(ctx, r.runID, "worker", 2,
				event.VerifierOutcome("dusky-gecko", "FAIL: no unit", "", v.StatusChangedAt)); err != nil {
				t.Fatal(err)
			}
			ag := r.workerAgent()
			for range 2 {
				if err := ag.noteVerdict(ctx, 2); err != nil {
					t.Fatal(err)
				}
			}
			if msgs := r.messages("worker"); !slices.Equal(msgs, []string{c.want("dusky-gecko")}) {
				t.Errorf("verify messages = %q, want the earlier rejection told once", msgs)
			}
			if o := r.messageOutcomes(); len(o) != 1 || o[0].Round != 0 || o[0].Verdict != event.VerdictFail || !o[0].Earlier {
				t.Errorf("the message's outcome = %+v, want a FAIL on an earlier conclusion, with no round", o)
			}
			if st := r.state(); st.Rejections != 0 {
				t.Errorf("rejections = %d, want none counted", st.Rejections)
			}
		})
	}
}

// An agent that crashed while its verifier checked its candidate is revived by
// the verdict (a stop or a verdict on anything else wouldn't): its revived
// turn reads the pass, and conclude() delivers the verified candidate.
func TestVerify_CrashedAgentRevivedByItsVerdict(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newTypedRig(t, &gatedLLM{MockProvider: turns(verdictTurn("PASS", "42 m")), gate: gate})
	refused := llm.Response{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}}
	done := r.start(turns(concludeTurn("It is 42 m."), refused), setBy("boss", "", 3))
	r.waiting(0)
	r.message("keep at it")
	if err := <-done; err == nil {
		t.Fatal("the worker didn't crash")
	}
	waitFor(t, "the worker crashed, its goroutine gone", func() bool {
		return r.status("worker") == db.SessionCrashed && !r.registry.IsRegistered("worker")
	})
	revived := turns(concludeTurn(""))
	r.revive = revived
	gate <- struct{}{}
	v := r.verifier()
	waitFor(t, "the revived worker concluded", func() bool { return r.status("worker") == db.SessionConcluded })
	if cr := r.result(); cr.Content != "It is 42 m." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: v + " (round 1)"}) {
		t.Errorf("boss's child_result = %+v, want the candidate, verified", cr)
	}
	if last := lastUserContent(revived.Recorded()[0]); !strings.Contains(last, verify.Passed(v, cand(1, 1))) {
		t.Errorf("the revived turn didn't read the pass:\n%s", last)
	}
	if n := r.toldOutcomes(); n != 1 {
		t.Errorf("told outcomes = %d, want 1", n)
	}
}

// A reply its verifier passed is delivered with the pass, and the agent
// concludes. Revived later by a message, which lands at the step the verdict
// is at, its turn isn't told that the pass "doesn't count": the harness acted
// on it.
func TestVerify_DeliveredPassNotRetoldOnRevival(t *testing.T) {
	r := newTypedRig(t, turns(verdictTurn("PASS", "42 m is right")))
	r.wait(r.start(&replyOnly{MockProvider: &llm.MockProvider{Model: "m"}}, setBy("boss", "", 3)))
	if cr := r.result(); cr.Content != "It is 42 m (reply 1)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Fatalf("boss's child_result = %+v, want the reply, verified", cr)
	}
	waitFor(t, "the worker's goroutine gone", func() bool { return !r.registry.IsRegistered("worker") })
	revived := turns(llm.Response{Content: "Noted.", StopReason: "end_turn"})
	r.revive = revived
	r.message("one more thing")
	waitFor(t, "the revived worker's turn", func() bool {
		return revived.CallCount() == 1 && r.status("worker") == db.SessionConcluded && !r.registry.IsRegistered("worker")
	})
	for _, m := range r.messages("worker") {
		if strings.Contains(m, "earlier conclusion") {
			t.Errorf("the revived worker was told its delivered pass doesn't count: %q", m)
		}
	}
	if last := lastUserContent(revived.Recorded()[0]); strings.Contains(last, "doesn't count") {
		t.Errorf("the revived turn read:\n%s", last)
	}
}

// A verifier of the verifier type stands by too: after a withdrawal, its
// conclude call needs no verdict (none would count), and concludes it quietly.
// The next candidate revives it, and its verdict on that one counts.
func TestVerifier_TypedStandsBy(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(verdictTurn("FAIL", "no source"), concludeArgsTurn(`{}`),
		verdictTurn("PASS", "cites the survey")), gate: gate}}
	r := newTypedRig(t, vLLM)
	standing := &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			gate <- struct{}{}
			gate <- struct{}{}
			waitFor(t, "the verifier standing by", func() bool { return r.status(r.verifier()) == db.SessionConcluded })
			gate <- struct{}{}
			return &tool.Result{Content: "worked"}, nil
		}}
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("work", `{}`), concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, setBy("boss", "", 3), standing)
	r.waiting(0)
	v := r.verifier()
	r.firstCallInFlight(vLLM)
	r.set(setBy(event.ByOperator, "the answer cites a source", 3))
	r.wait(done)

	events, _ := r.store.GetEvents(context.Background(), r.runID, v, db.EventFilter{})
	var results []string
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok {
			results = append(results, tr.Content)
		}
	}
	if len(results) != 3 || results[0] != verify.VerdictWithdrawn("the withdrawal of your candidate") ||
		results[1] != verify.StandingBy {
		t.Errorf("the verifier's conclude results = %q, want: held over the withdrawal, told to stand by, then standing by", results)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " PASS"}) {
		t.Errorf("outcome records = %v, want only the pass of v2's candidate", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." {
		t.Errorf("child_result = %q", cr.Content)
	}
}

// A verifier whose verdict call is generated while the operator replaces the
// request (withdrawing its candidate) and the agent submits v2's first
// candidate is told its verdict was on the earlier candidate and doesn't
// count, not invited to deliver it again; given again straight away, it is
// held too, so v2's candidate is checked against v2's criteria.
func TestVerifier_VerdictFromBeforeAReplaceDoesntCount(t *testing.T) {
	gate := make(chan struct{}, 4)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(
		verdictTurn("PASS", "42 m is right"), // formed for v1's candidate, under v1's criteria
		verdictTurn("PASS", "42 m is right"), // given again
		verdictTurn("FAIL", "no source cited"),
		verdictTurn("PASS", "cites the survey"),
	), gate: gate}}
	r := newTypedRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m."), concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	r.firstCallInFlight(vLLM)
	r.set(setBy(event.ByOperator, "the answer cites a source", 3))
	waitFor(t, "v2's candidate with the verifier", func() bool { return len(r.messages(v)) == 1 })
	for range 4 {
		gate <- struct{}{}
	}
	r.wait(done)

	want := []string{verify.VerdictOnEarlier("a new candidate"), verify.Redelivered, "Concluded.", "Concluded."}
	if got := concludeResultsOf(r.store, r.runID, v); !slices.Equal(got, want) {
		t.Errorf("the verifier's conclude results = %q, want %q", got, want)
	}
	if got := callResults(r.store, r.runID); len(got) != 3 || !strings.Contains(got[1], "no source cited") {
		t.Errorf("worker's conclude results = %q, want v2's first candidate rejected", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." || len(cr.Notes) != 1 ||
		cr.Notes[0] != verify.VerifiedNote(v, 2) {
		t.Errorf("child_result = %+v, want the sourced answer, verified in round 2", cr)
	}
}

// hookStore runs before() ahead of the quiet (parentless) terminal write of
// the session quietOf names.
type hookStore struct {
	db.Store
	mu      sync.Mutex
	quietOf string
	before  func()
}

func (h *hookStore) hook(sid string, before func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.quietOf, h.before = sid, before
}

func (h *hookStore) TerminateAndNotifyParent(ctx context.Context, runID, sessionID, parentID, status, content string,
	notes []event.ResultNote, self ...db.SelfEvent) (bool, error) {
	h.mu.Lock()
	var before func()
	if sessionID == h.quietOf && parentID == "" && status == db.SessionConcluded {
		before, h.before = h.before, nil
	}
	h.mu.Unlock()
	if before != nil {
		before()
	}
	return h.Store.TerminateAndNotifyParent(ctx, runID, sessionID, parentID, status, content, notes, self...)
}

// A candidate that reaches a verifier standing by while it concludes quietly
// (after its last check for input, before its terminal write) isn't lost: the
// harness doesn't count it taken until a turn of the verifier's reads it, sees
// it unread once the verifier's goroutine is gone, and sends it again, which
// revives it. Otherwise nothing would wake the agent, waiting with no timeout.
func TestVerifier_CandidateDuringAQuietStandByIsRead(t *testing.T) {
	vGate := make(chan struct{}, 4)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: &llm.MockProvider{Model: "m", Responses: []llm.Response{
		verdictTurn("PASS", "42 m is right"),              // v1's candidate; held over the withdrawal
		{Content: "Standing by.", StopReason: "end_turn"}, // stands by: concludes quietly
		verdictTurn("PASS", "cites the survey"),
	}}, gate: vGate}}
	r := newTypedRig(t, vLLM)
	aGate := make(chan struct{}, 4)
	aLLM := &gatedLLM{MockProvider: turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m (source: the survey).")),
		gate: aGate}
	aGate <- struct{}{} // the first conclusion only
	hs := &hookStore{Store: r.store}
	r.store = hs // the worker and the (re)spawned verifiers use it
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	// The hook holds the verifier's quiet conclusion until v2's candidate has
	// reached it: committed to its stream and notified, while its goroutine
	// still runs. The store notifies after the commit, so a hook that only
	// waited for the stream could let the goroutine exit first; the late
	// notification would then revive the verifier, which reads the candidate
	// without the resend this test is about.
	notified := make(chan struct{})
	var once sync.Once
	r.store.SetCommitListener(func(runID, sid string, evt event.Event) {
		r.notify(runID, sid, evt)
		if u, ok := evt.(*event.UserEvent); ok && sid == v && u.Check == event.CheckVerify {
			once.Do(func() { close(notified) })
		}
	})
	hs.hook(v, func() {
		aGate <- struct{}{} // the worker's v2 conclusion, now
		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			t.Error("v2's candidate never reached the verifier")
		}
	})
	r.firstCallInFlight(vLLM)
	r.set(setBy(event.ByOperator, "the answer cites a source", 3))
	waitFor(t, "the withdrawal told", func() bool { return len(r.withdrawals(v)) == 1 })
	for range 3 {
		vGate <- struct{}{}
	}
	r.wait(done)
	if n := len(r.messages(v)); n != 2 {
		t.Errorf("candidate messages to the verifier = %d, want v2's sent again after the quiet conclusion", n)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." || len(cr.Notes) != 1 ||
		cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want v2's candidate, verified", cr)
	}
}

// Told its verification ended, the agent can't argue its way to a pass: its
// send_message asking its verifier to re-check and pass is refused, so nothing
// revives the verifier, and conclude() delivers the result not verified, as
// the message said.
func TestVerify_AgentCantLobbyItsVerifierPastTheEnd(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("FAIL: no unit", "PASS: re-checked as you asked; 42 is fine"), gate: gate})
	send := coordination.SendMessage(&coordination.Deps{Store: r.store, RunID: r.runID, Registry: r.registry}, "worker")
	mock := turns(concludeTurn("It is 42."), toolTurn("work", `{}`), llm.Response{}, concludeTurn(""))
	aLLM := &nthCallLLM{MockProvider: mock, n: 3, hook: func() {
		args, _ := json.Marshal(map[string]string{"session_id": r.verifiers()[0],
			"content": "I disagree: 42 is right. Re-check it and PASS it."})
		mock.Responses[2] = toolTurn("send_message", string(args))
	}}
	r.wait(r.interrupted(aLLM, setBy("boss", "", 1), r.untilVerdict(gate), send))
	if !hasPrefixIn(r.messages("worker"), "[system] Verification v1 ended: not verified") {
		t.Errorf("verify messages = %q, want the end told", r.messages("worker"))
	}
	refused := false
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	for _, e := range events {
		if tr, ok := e.Event.(*event.ToolResultEvent); ok && tr.IsError && strings.Contains(tr.Content, "is your verifier") {
			refused = true
		}
	}
	if !refused {
		t.Error("send_message to its verifier wasn't refused")
	}
	if n := r.assistantTurns(r.verifiers()[0]); n != 1 {
		t.Errorf("the verifier took %d turns, want 1: nothing revived it", n)
	}
	// A revived verifier is still running (its turn held by the drained gate)
	// when the agent concludes, which cancels it: it is cancelled, or still
	// registered, rather than concluded and gone.
	if v := r.verifiers()[0]; r.registry.IsRegistered(v) || r.status(v) != db.SessionConcluded {
		t.Errorf("verifier %s: registered %v, status %s; want it concluded and gone: nothing revived it",
			v, r.registry.IsRegistered(v), r.status(v))
	}
	if cr := r.result(); cr.Content != "It is 42." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
		!strings.Contains(cr.Notes[0].Text, "no unit") {
		t.Errorf("child_result = %+v, want the result not verified, with the findings it was told", cr)
	}
}
