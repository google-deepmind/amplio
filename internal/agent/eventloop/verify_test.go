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
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/proctree"
	"amplio/internal/runtime"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/tool/conclude"
	"amplio/internal/tool/coordination"
	"amplio/internal/verify"
	"amplio/internal/workspace/plain"
)

const (
	testVerifierType = "eventloop_test_verifier"
	// brokenVerifierType's agents fail before creating their session.
	brokenVerifierType = "eventloop_test_broken_verifier"
)

// verifierLLM answers the test verifiers' turns; set by newVerifyRig.
var verifierLLM llm.Provider

func init() {
	agent.Register(testVerifierType, func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		e := *env
		e.LLM = verifierLLM
		return New(&e, Config{SessionID: cfg.SessionID, Task: cfg.Task, ParentID: cfg.ParentID, Handle: cfg.Handle,
			VerifierFor: cfg.VerifierFor, AgentType: testVerifierType, SystemPrompt: "verifier",
			Tools: []*tool.Tool{conclude.New(), checkTool}}), nil
	}, agent.Traits{})
}

// checkTool is a test verifier's turn of work.
var checkTool = &tool.Tool{Name: "check", Description: "check", ParamType: &struct{}{},
	Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
		return &tool.Result{Content: "checked"}, nil
	}}

type brokenAgent struct{ id string }

func (b brokenAgent) Run(context.Context) error { return errors.New("bootstrap: no workspace") }
func (b brokenAgent) SessionID() string         { return b.id }

func init() {
	agent.Register(brokenVerifierType, func(_ *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		return brokenAgent{id: cfg.SessionID}, nil
	}, agent.Traits{})
}

// gatedLLM answers from its script, each call waiting for a token on gate.
type gatedLLM struct {
	*llm.MockProvider
	gate chan struct{}
}

func (g *gatedLLM) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.MockProvider.Call(ctx, req)
}

// script answers with replies without tool calls (how the test verifiers
// conclude).
func script(texts ...string) *llm.MockProvider {
	m := &llm.MockProvider{Model: "m"}
	for _, t := range texts {
		m.Responses = append(m.Responses, llm.Response{Content: t, StopReason: "end_turn"})
	}
	return m
}

// verifyRig runs a verified agent "worker" (parent "boss"), which has the
// conclude tool, with a stand-in verifier type, and revives stopped verifiers
// the way the run manager would.
type verifyRig struct {
	t        *testing.T
	store    db.Store
	runID    string
	registry *session.Registry
	notify   db.CommitListener // the wake path, for a test that wraps it
	// noRespawn holds sessions a message must not revive ("*": any), as a
	// respawn that fails (a model whose credentials are gone, say).
	noRespawn sync.Map
	// revive, if set, answers a revived worker's turns.
	revive llm.Provider
}

func newVerifyRig(t *testing.T, verifier llm.Provider) *verifyRig {
	t.Helper()
	withJobs(t)
	store := testStore(t)
	ctx := context.Background()
	runID := db.NewRunID()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	runReg := runtime.NewRunRegistry()
	r := &verifyRig{t: t, store: store, runID: runID, registry: runReg.GetOrCreate(runID)}
	r.notify = runtime.NewCommitNotifier(runReg, r.respawn,
		func(runID, sid string) (string, bool) {
			sess, err := store.GetSession(context.Background(), runID, sid)
			if err != nil {
				return "", false
			}
			return sess.Status, true
		},
		func(runID, sid, verifier string) bool {
			return verify.AwaitsVerdict(context.Background(), store, runID, sid, verifier)
		})
	store.SetCommitListener(r.notify)
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "boss",
		AgentType: "standard_agent", Status: db.SessionAwaiting}); err != nil {
		t.Fatal(err)
	}
	origType, origLLM := verifierAgentType, verifierLLM
	verifierAgentType, verifierLLM = testVerifierType, verifier
	t.Cleanup(func() {
		r.drain()
		verifierAgentType, verifierLLM = origType, origLLM
		r.checkTypedLoad()
	})
	return r
}

// checkTypedLoad checks, for every session the test left, that the
// verification verify.Load reads by type is the one the whole stream gives.
func (r *verifyRig) checkTypedLoad() {
	r.t.Helper()
	ctx := context.Background()
	sessions, err := r.store.ListSessions(ctx, r.runID)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range sessions {
		all, err := r.store.GetEvents(ctx, r.runID, s.SessionID, db.EventFilter{})
		if err != nil {
			r.t.Fatal(err)
		}
		typed, err := verify.Load(ctx, r.store, r.runID, s.SessionID)
		if err != nil {
			r.t.Fatal(err)
		}
		if whole := verify.Derive(all); !reflect.DeepEqual(typed, whole) {
			r.t.Errorf("%s: verify.Load gives\n%+v\nthe whole stream\n%+v", s.SessionID, typed, whole)
		}
	}
}

func (r *verifyRig) env() *agent.Env {
	return &agent.Env{Store: r.store, RunID: r.runID, Registry: r.registry,
		Names: session.NewNameAllocator(r.store, r.runID), Workspace: plain.New("/tmp")}
}

// respawn revives a stopped verifier that a message re-engages, and the
// worker when revive is set.
func (r *verifyRig) respawn(runID, sid string) {
	if sid == "worker" && r.revive != nil {
		ag := r.worker(r.revive, nil)
		go func() { _ = ag.Run(context.Background()) }()
		return
	}
	sess, err := r.store.GetSession(context.Background(), runID, sid)
	if err != nil || (sess.AgentType != testVerifierType && sess.AgentType != typedVerifierType) {
		return
	}
	if _, blocked := r.noRespawn.Load(sid); blocked {
		return
	}
	if _, blocked := r.noRespawn.Load("*"); blocked {
		return
	}
	ctx, h, release, ok := r.registry.RegisterAndContext(context.Background(), sid)
	if !ok {
		return
	}
	factory, _ := agent.Get(sess.AgentType)
	ag, _ := factory(r.env(), &agent.Config{SessionID: sid, Task: sess.Task, ParentID: sess.ParentID, Handle: h})
	go func() {
		defer release()
		_ = ag.Run(ctx)
	}()
}

// drain waits for every session goroutine to exit, stopping any left; one
// that outlives its cancellation fails the test instead of hanging it.
func (r *verifyRig) drain() {
	deadline := time.Now().Add(2 * time.Second)
	for !r.registry.IsEmpty() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.registry.CancelAll()
	for deadline = time.Now().Add(5 * time.Second); !r.registry.IsEmpty(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Error("session goroutines still running 5s after CancelAll")
			return
		}
	}
}

// start runs the worker in the background, with the conclude tool and any
// others, and verification set by its parent; the channel yields Run's error.
func (r *verifyRig) start(agentLLM llm.Provider, v *event.VerifyEvent, tools ...*tool.Tool) <-chan error {
	return r.startAgent(r.worker(agentLLM, v, tools...))
}

func (r *verifyRig) worker(agentLLM llm.Provider, v *event.VerifyEvent, tools ...*tool.Tool) *EventLoopAgent {
	return newT(testCfg{RunID: r.runID, SessionID: "worker", ParentID: "boss", Task: "How tall is it?",
		SystemPrompt: "sp", LLM: agentLLM, Store: r.store, Registry: r.registry, Workspace: plain.New("/tmp"), Verify: v,
		Tools: append([]*tool.Tool{conclude.New()}, tools...)})
}

func (r *verifyRig) startAgent(ag *EventLoopAgent) <-chan error {
	done := make(chan error, 1)
	go func() { done <- ag.Run(context.Background()) }()
	return done
}

func (r *verifyRig) wait(done <-chan error) {
	r.t.Helper()
	select {
	case err := <-done:
		if err != nil {
			r.t.Fatalf("worker: %v", err)
		}
	case <-time.After(5 * time.Second):
		r.t.Fatal("worker did not finish")
	}
}

func setBy(by string, criteria string, maxRounds int) *event.VerifyEvent {
	return &event.VerifyEvent{Action: event.VerifySet, By: by, Criteria: criteria, MaxRounds: maxRounds}
}

func (r *verifyRig) status(sid string) string {
	s, err := r.store.GetSession(context.Background(), r.runID, sid)
	if err != nil {
		return ""
	}
	return s.Status
}

func (r *verifyRig) state() verify.State {
	st, err := verify.Load(context.Background(), r.store, r.runID, "worker")
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

// actions are the steps of the worker's verification: its history but the
// verifiers' outcome records (see outcomes), whose place among the harness's
// own records depends on how fast a verifier is.
func (r *verifyRig) actions() []string {
	var out []string
	for _, e := range r.state().History {
		if e.Action != event.VerifyVerdict {
			out = append(out, e.Action)
		}
	}
	return out
}

// steps are the worker's verification records but the verifiers' outcomes.
func (r *verifyRig) steps() []*event.VerifyEvent {
	var out []*event.VerifyEvent
	for _, e := range r.state().History {
		if e.Action != event.VerifyVerdict {
			out = append(out, e.VerifyEvent)
		}
	}
	return out
}

// callOutcomes are the outcomes the worker's conclude call results told, one
// per result (nil where none).
func (r *verifyRig) callOutcomes() []*event.VerifyOutcome {
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	calls := map[string]bool{}
	var out []*event.VerifyOutcome
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *event.AssistantEvent:
			if tc, ok := ev.ConcludeCall(); ok {
				calls[tc.ID] = true
			}
		case *event.ToolResultEvent:
			if calls[ev.ToolCallID] {
				out = append(out, ev.Outcome)
			}
		}
	}
	return out
}

// messageOutcomes are the outcomes the worker's verify messages told, one per
// message (nil where none).
func (r *verifyRig) messageOutcomes() []*event.VerifyOutcome {
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	var out []*event.VerifyOutcome
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check == event.CheckVerify {
			out = append(out, u.Outcome)
		}
	}
	return out
}

// noVerifierResults fails if a verifier's result reached the worker as a
// child_result: its outcome is the harness's to tell.
func (r *verifyRig) noVerifierResults() {
	r.t.Helper()
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	for _, e := range events {
		if cr, ok := e.Event.(*event.ChildResultEvent); ok {
			r.t.Errorf("the worker got a child_result from %s", cr.ChildSessionID)
		}
	}
}

// outcomes are the verifiers' outcome records in the worker's stream, as
// "verifier verdict".
func (r *verifyRig) outcomes() []string {
	var out []string
	for _, e := range r.state().History {
		if e.Action == event.VerifyVerdict {
			out = append(out, e.Outcome.Verifier+" "+e.Outcome.Verdict)
		}
	}
	return out
}

// verifiers lists the worker's verifier sessions, oldest first.
func (r *verifyRig) verifiers() []string {
	children, _ := r.store.GetChildSessions(context.Background(), r.runID, "worker")
	var out []string
	for _, c := range children {
		if verify.VerifierFor(&c) == "worker" {
			out = append(out, c.SessionID)
		}
	}
	return out
}

// verifier waits until the worker has a verifier and returns its first.
func (r *verifyRig) verifier() string {
	r.t.Helper()
	waitFor(r.t, "a verifier", func() bool { return len(r.verifiers()) > 0 })
	return r.verifiers()[0]
}

// waiting waits until the worker waits in a conclude call, with n of its
// conclude calls answered so far. The count is read first: the worker marks
// itself ongoing before it records an answer, so an awaiting status read after
// the n-th answer is the wait that follows it, not the one before.
func (r *verifyRig) waiting(n int) {
	r.t.Helper()
	waitFor(r.t, "worker waiting in conclude", func() bool {
		return len(callResults(r.store, r.runID)) == n && r.status("worker") == db.SessionAwaiting
	})
}

// messages are the harness's verify messages in a stream.
func (r *verifyRig) messages(sid string) []string {
	events, _ := r.store.GetEvents(context.Background(), r.runID, sid, db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check == event.CheckVerify {
			out = append(out, u.Content)
		}
	}
	return out
}

// result is the worker's child_result in boss's stream.
func (r *verifyRig) result() *event.ChildResultEvent {
	r.t.Helper()
	events, _ := r.store.GetEvents(context.Background(), r.runID, "boss", db.EventFilter{})
	var cr *event.ChildResultEvent
	for _, e := range events {
		if c, ok := e.Event.(*event.ChildResultEvent); ok && c.ChildSessionID == "worker" {
			cr = c
		}
	}
	if cr == nil {
		r.t.Fatal("boss got no child_result from worker")
	}
	return cr
}

func (r *verifyRig) message(content string) {
	r.t.Helper()
	if _, err := r.store.AppendEvent(context.Background(), r.runID, "worker", &event.MessageEvent{
		Content: content, Sender: "boss", SenderType: event.SenderTypeAgent}); err != nil {
		r.t.Fatal(err)
	}
}

// cand places candidate c of v1 in round.
func cand(c, round int) verify.Where {
	return verify.Where{Label: verify.Label{Version: 1, Candidate: c}, Round: fmt.Sprint(round)}
}

func lastUserContent(req llm.Request) string {
	var parts []string
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n---\n")
}

func hasPrefixIn(list []string, prefix string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// The flow end to end: the first conclusion goes to a verifier, which rejects
// it; the conclude call returns the findings; the agent concludes again, the
// same verifier gets the new candidate and passes it, and the call that waited
// for that verdict concludes the session with exactly that candidate.
func TestVerify_RejectThenPass(t *testing.T) {
	vLLM := script("FAIL: the height has no unit", "PASS: 42 m, with a unit")
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42."), concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, setBy("boss", "the answer gives a unit", 3)))

	if aLLM.CallCount() != 2 {
		t.Errorf("agent calls = %d, want 2: the pass concludes inside the call that waited for it", aLLM.CallCount())
	}
	vs := r.verifiers()
	if len(vs) != 1 {
		t.Fatalf("verifiers = %v, want one, re-engaged", vs)
	}
	if got, want := r.actions(), []string{"set", "verifying", "rejected", "verifying", "passed"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	cr := r.result()
	if cr.Content != "It is 42 m." || !slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: vs[0] + " (round 2)"}) {
		t.Errorf("child_result = %+v, want the verified candidate, verified by %s (round 2)", cr, vs[0])
	}
	results := callResults(r.store, r.runID)
	if len(results) != 2 || !strings.Contains(results[0], "rejected your conclusion (v1, candidate 1; round 1 of 3):\n\nthe height has no unit") ||
		!strings.Contains(results[0], "call conclude(result) with your new result") || results[1] != verify.Verified(vs[0], cand(2, 2)) {
		t.Errorf("conclude call results = %q", results)
	}
	if msgs := r.messages("worker"); len(msgs) != 0 {
		t.Errorf("verify messages = %q, want none: the calls carried every answer", msgs)
	}
	reqs := vLLM.Recorded()
	if len(reqs) != 2 {
		t.Fatalf("verifier calls = %d, want 2", len(reqs))
	}
	first := lastUserContent(reqs[0])
	for _, want := range []string{"agent `worker`", "Request (v1): How tall is it?", "Criteria: the answer gives a unit",
		"Candidate conclusion (v1, candidate 1):\nIt is 42.", "must be exactly `PASS` or `FAIL`, or start with `PASS:` or `FAIL:`"} {
		if !strings.Contains(first, want) {
			t.Errorf("verifier task lacks %q:\n%s", want, first)
		}
	}
	if second := lastUserContent(reqs[1]); !strings.Contains(second, "concluded again: v1, candidate 2 (round 2)") ||
		!strings.Contains(second, "Candidate conclusion (v1, candidate 2):\nIt is 42 m.") {
		t.Errorf("re-engagement lacks the new candidate:\n%s", second)
	}
	agentReqs := aLLM.Recorded()
	if !strings.Contains(lastUserContent(agentReqs[0]),
		"Verification v1: your conclusion will be checked by an independent verifier before it is delivered, against: the answer gives a unit.") {
		t.Errorf("the agent was not told about its verification:\n%s", lastUserContent(agentReqs[0]))
	}
	assertPaired(t, agentReqs[1])
}

// At the round limit the call concludes at once, with the candidate and the
// verifier's findings on the parent's note; verification ends.
func TestVerify_ExhaustedConcludesUnverified(t *testing.T) {
	r := newVerifyRig(t, script("FAIL: the number is made up"))
	aLLM := turns(concludeTurn("It is 42."))
	r.wait(r.start(aLLM, setBy("boss", "", 1)))

	if aLLM.CallCount() != 1 {
		t.Errorf("agent calls = %d, want 1: it concludes when the last round is rejected", aLLM.CallCount())
	}
	cr := r.result()
	want := event.ResultNote{Kind: event.NoteNotVerified, Text: "after 1 rejection. Latest findings: the number is made up"}
	if cr.Content != "It is 42." || !slices.Contains(cr.Notes, want) {
		t.Errorf("child_result = %+v, want the candidate with %+v", cr, want)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "exhausted"}) {
		t.Errorf("history = %v", got)
	}
	if st := r.state(); st.Status() != verify.StatusNotVerified || st.Round() != 1 {
		t.Errorf("state = %s round %d, want not_verified round 1", st.Status(), st.Round())
	}
	// The rounds are the reason: the record says so by its round, not a reason.
	if h := r.state().History; h[len(h)-1].Reason != "" {
		t.Errorf("exhausted record reason = %q, want none for a rounds ending", h[len(h)-1].Reason)
	}
	if got := callResults(r.store, r.runID); len(got) != 1 ||
		got[0] != verify.NotVerified(verify.RoundsReason(1, "the number is made up")) {
		t.Errorf("conclude call result = %q", got)
	}
}

// Anything else that arrives while the call waits ends the wait early, saying
// what came; conclude() then waits again, without messaging the verifier
// again (it has that very conclusion), and the pass concludes. The end-of-run
// reminder never counts the agent's own verifier as left running.
func TestVerify_WaitEndsEarlyOnOtherEvent(t *testing.T) {
	gate := make(chan struct{})
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: correct"), gate: gate})
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn(""))
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()

	r.message("any news?")
	r.waiting(1)
	results := callResults(r.store, r.runID)
	if want := "Not concluded yet: your conclusion (v1, candidate 1; round 1) is still with `" + v + "`, and this arrived meanwhile: " +
		"a message from boss. Call conclude() to keep waiting"; !strings.HasPrefix(results[0], want) {
		t.Errorf("result of the interrupted call = %q, want it to start %q", results[0], want)
	}
	if n := len(r.messages(v)); n != 0 {
		t.Errorf("verifier got %d more messages; conclude() re-sent the candidate it has", n)
	}
	if n := len(reminders(t, r.store, r.runID, "worker")); n != 0 {
		t.Errorf("reminders = %d: the agent's own verifier counted as left running", n)
	}
	close(gate)
	r.wait(done)
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q, want the candidate the verifier passed", cr.Content)
	}
}

// A verifier that crashes isn't a round: the call returns saying so, and
// conclude() re-engages the same verifier (a message revives it) with the
// same candidate.
func TestVerify_VerifierCrashIsNotARound(t *testing.T) {
	vLLM := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}},
		{Content: "PASS", StopReason: "end_turn"},
	}}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn(""))
	r.wait(r.start(aLLM, setBy("boss", "", 1)))

	vs := r.verifiers()
	if len(vs) != 1 {
		t.Fatalf("verifiers = %v, want the crashed one re-engaged", vs)
	}
	refusal := `crashed: provider refused the turn (stop_reason="refusal", category=test)`
	if got := callResults(r.store, r.runID); len(got) != 2 || got[0] != verify.Stopped(vs[0], verify.Label{Version: 1, Candidate: 1}, refusal) ||
		!strings.Contains(got[0], "(crashed: provider refused the turn") {
		t.Errorf("conclude call results = %q, want the stop first, with why", got)
	}
	if o := r.callOutcomes()[0]; o == nil || o.Verdict != db.SessionCrashed || o.Verifier != vs[0] || o.Round != 0 || o.At.IsZero() {
		t.Errorf("the stop's outcome = %+v, want no round (a stop isn't one) and its time", o)
	}
	if msgs := r.messages("worker"); len(msgs) != 0 {
		t.Errorf("verify messages = %q, want none: the call told the agent of the stop", msgs)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{vs[0] + " crashed", vs[0] + " PASS"}) {
		t.Errorf("outcome records = %v", got)
	}
	r.noVerifierResults()
	if cr := r.result(); cr.Content != "It is 42 m." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: vs[0] + " (round 1)"}) {
		t.Errorf("child_result = %+v, want verified in round 1 (max_rounds 1: the crash didn't count)", cr)
	}
}

// set starts a new version of the worker's verification, returning its
// number.
func (r *verifyRig) set(e *event.VerifyEvent) int {
	r.t.Helper()
	n, err := verify.Set(context.Background(), r.store, r.registry, r.runID, "worker", e)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// firstCallInFlight waits until the verifier's first model call is in flight,
// its context without what comes next.
func (r *verifyRig) firstCallInFlight(vLLM *enteredLLM) {
	r.t.Helper()
	waitFor(r.t, "the verifier's first call", func() bool { return vLLM.entered.Load() == 1 })
}

// withdrawals are the harness's withdrawal messages in a verifier's stream.
func (r *verifyRig) withdrawals(verifier string) []string {
	events, _ := r.store.GetEvents(context.Background(), r.runID, verifier, db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check == event.CheckWithdrawn {
			out = append(out, u.Content)
		}
	}
	return out
}

// Replacing verification while the call waits starts v2 without cancelling
// the verifier. The candidate is withdrawn: its record ends the wait, and the
// call returns "superseded by v2" (the replace itself wakes no one). The
// verifier is told to stop and wait, and the same verifier gets v2's first
// candidate, with the new criteria, keeping what it learned. Its first turn,
// generated before the withdrawal and the new candidate arrived, doesn't
// conclude over them; only v2's candidate is delivered.
func TestVerify_ReplaceKeepsTheVerifier(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("FAIL: no source", "PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	r.firstCallInFlight(vLLM)

	if n := r.set(setBy(event.ByOperator, "the answer cites a source", 3)); n != 2 {
		t.Fatalf("the replace started v%d, want v2", n)
	}
	waitFor(t, "v2's candidate with the verifier", func() bool { return len(r.messages(v)) == 1 })
	if s := r.status(v); s != db.SessionOngoing {
		t.Errorf("verifier is %s after the replace, want it kept at work", s)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)

	if vs := r.verifiers(); len(vs) != 1 || r.status(v) != db.SessionConcluded {
		t.Errorf("verifiers = %v (%s), want the one, concluded with its verdict on v2", vs, r.status(v))
	}
	got := callResults(r.store, r.runID)
	if got[0] != verify.Withdrawn(verify.Label{Version: 1, Candidate: 1}, 2) ||
		!strings.HasPrefix(got[0], "Not concluded: superseded by v2; your conclusion for v1 was withdrawn") {
		t.Errorf("result of the waiting call = %q", got[0])
	}
	if w := r.withdrawals(v); len(w) != 1 || w[0] != verify.Withdrawal("worker", verify.Label{Version: 1, Candidate: 1}, 2) {
		t.Errorf("the verifier's withdrawal messages = %q", w)
	}
	delivery := r.messages(v)[0]
	for _, want := range []string{"has a new version: v2", "Request (v2): How tall is it?", "Criteria: the answer cites a source",
		"Candidate conclusion (v2, candidate 1):\nIt is 42 m (source: the survey)."} {
		if !strings.Contains(delivery, want) {
			t.Errorf("v2's delivery lacks %q:\n%s", want, delivery)
		}
	}
	if got, want := r.actions(), []string{"set", "verifying", "replace", "withdrawn", "verifying", "passed"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if got := r.outcomes(); !slices.Equal(got, []string{v + " PASS"}) {
		t.Errorf("outcome records = %v, want only the pass of v2's candidate", got)
	}
	if !strings.Contains(lastUserContent(aLLM.Recorded()[1]), "The operator replaced your verification request: v2 supersedes v1") {
		t.Error("the agent's turn after the replace didn't show the change")
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want v2's candidate, verified", cr)
	}
	if call := callResults(r.store, r.runID); call[1] != verify.Verified(v, verify.Where{
		Label: verify.Label{Version: 2, Candidate: 1}, Round: "1"}) {
		t.Errorf("the concluding call's result = %q", call[1])
	}
}

// A verdict for a version that is no longer current is recorded as stale and
// changes nothing. The verifier passes v1's candidate while the agent works
// (its call returned early); v2 starts before the agent's next turn, which is
// told of v2, not of the pass. Its next conclusion is v2's first candidate,
// which the same verifier, revived, checks; only that one is delivered.
func TestVerify_StaleVerdictIgnored(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: 42 m is right", "PASS: cites one"), gate: gate})
	work := &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			gate <- struct{}{}
			waitFor(t, "the verifier's pass of v1", func() bool { return r.status(r.verifier()) == db.SessionConcluded })
			r.set(setBy(event.ByOperator, "the answer cites a source", 3))
			gate <- struct{}{} // for v2's candidate
			return &tool.Result{Content: "worked"}, nil
		}}
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("work", `{}`), concludeTurn("It is 42 m (source: the survey)."))
	r.wait(r.interrupted(aLLM, setBy("boss", "", 3), work))
	v := r.verifier()

	if vs := r.verifiers(); len(vs) != 1 {
		t.Errorf("verifiers = %v, want the one, revived for v2", vs)
	}
	if testing.Verbose() {
		evs, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
		for _, e := range evs {
			t.Logf("step %d %s %s", e.Step, e.Event.EventType(), strings.ReplaceAll(e.Event.ToText(), "\n", " | "))
		}
	}
	st := r.state()
	var stale []verify.Entry
	for _, e := range st.History {
		if e.Action == event.VerifyStale {
			stale = append(stale, e)
		}
	}
	if len(stale) != 1 || withoutAt(stale[0].Outcome) != (event.VerifyOutcome{Verifier: v, Verdict: event.VerdictPass, Findings: "42 m is right"}) ||
		stale[0].Reason != "for v1, candidate 1, which v2 superseded" || stale[0].Label != (verify.Label{Version: 1, Candidate: 1}) {
		t.Errorf("stale records = %+v, want v1's pass", stale)
	}
	if verdicts := slices.DeleteFunc(slices.Clone(st.History), func(e verify.Entry) bool { return e.Action != event.VerifyVerdict }); len(verdicts) != 2 ||
		!verdicts[0].Stale || verdicts[1].Stale {
		t.Errorf("verdicts = %+v, want v1's marked stale, v2's not", verdicts)
	}
	if got, want := r.actions(), []string{"set", "verifying", "replace", "withdrawn", "stale", "verifying", "passed"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if w := r.withdrawals(v); len(w) != 0 {
		t.Errorf("a verifier done with its candidate was told of the withdrawal: %q", w)
	}
	// The turn it woke reads why it doesn't count, not that it passed.
	stalePass := harness(verify.StaleNote(v, true, "42 m is right", verify.Label{Version: 1, Candidate: 1}, 2))
	if msgs := r.messages("worker"); !slices.Equal(msgs, []string{stalePass}) {
		t.Errorf("verify messages = %q, want only the stale pass, as information", msgs)
	}
	turn := lastUserContent(aLLM.Recorded()[2])
	if strings.Contains(turn, "Call conclude() to deliver it") || !strings.Contains(turn, "v2 supersedes v1") ||
		!strings.HasSuffix(turn, stalePass) {
		t.Errorf("the turn after the stale pass read:\n%s", turn)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want v2's candidate, verified", cr)
	}
	if !strings.Contains(r.messages(v)[0], "Candidate conclusion (v2, candidate 1)") {
		t.Errorf("the revived verifier's candidate = %q", r.messages(v)[0])
	}
}

// Clearing verification while the call waits withdraws the candidate (its
// record ends the wait) and keeps the verifier, told to stand by; the next
// conclusion goes through, with no verification note, and cascades to the
// verifier still at work.
func TestVerify_ClearWithdrawsTheCandidate(t *testing.T) {
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS"), gate: make(chan struct{})})
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m; not checked."))
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()

	if cleared, err := verify.Clear(context.Background(), r.store, r.registry, r.runID, "worker", "boss"); err != nil || !cleared {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	if s := r.status(v); s != db.SessionOngoing { // the agent's conclusion cancels it later
		t.Errorf("verifier is %s right after the clear, want it kept", s)
	}
	if w := r.withdrawals(v); len(w) != 1 || w[0] != verify.Withdrawal("worker", verify.Label{Version: 1, Candidate: 1}, 0) {
		t.Errorf("the verifier's withdrawal messages = %q", w)
	}
	r.wait(done)
	if got := callResults(r.store, r.runID); got[0] != verify.Withdrawn(verify.Label{Version: 1, Candidate: 1}, 0) {
		t.Errorf("result of the waiting call = %q", got[0])
	}
	cr := r.result()
	if cr.Content != "It is 42 m; not checked." || len(cr.Notes) != 0 {
		t.Errorf("child_result = %+v, want the next conclusion, with no notes", cr)
	}
	if !strings.Contains(lastUserContent(aLLM.Recorded()[1]), "Your parent boss turned verification off (v1 ended)") {
		t.Error("the agent's turn after the clear didn't show it")
	}
	if s := r.status(v); s != db.SessionCancelled {
		t.Errorf("verifier is %s after the agent concluded, want cancelled by the cascade", s)
	}
}

// A verifier whose candidate was withdrawn stands by: what it ends its turn
// with (here a reply, which a verifier is otherwise nudged for) concludes it
// quietly, with no outcome for the agent. The next candidate revives it.
func TestVerifier_StandsByAfterAWithdrawal(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("FAIL: no source", "Standing by.", "PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	standing := &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			gate <- struct{}{}
			gate <- struct{}{}
			// Concluded and gone: a candidate that lands while its goroutine is
			// still leaving only bumps it, and is sent again.
			waitFor(t, "the verifier standing by", func() bool {
				v := r.verifier()
				return r.status(v) == db.SessionConcluded && !r.registry.IsRegistered(v)
			})
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

	if got := r.outcomes(); !slices.Equal(got, []string{v + " PASS"}) {
		t.Errorf("outcome records = %v, want only the pass of v2's candidate: standing by tells nothing", got)
	}
	vEvents, _ := r.store.GetEvents(context.Background(), r.runID, v, db.EventFilter{})
	var replies []string
	for _, e := range vEvents {
		if ae, ok := e.Event.(*event.AssistantEvent); ok {
			replies = append(replies, ae.Content)
		}
	}
	if !slices.Equal(replies, []string{"FAIL: no source", "Standing by.", "PASS: cites one"}) {
		t.Errorf("verifier turns = %q", replies)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." {
		t.Errorf("child_result = %q", cr.Content)
	}
	if vs := r.verifiers(); len(vs) != 1 {
		t.Errorf("verifiers = %v, want the one", vs)
	}
}

// A restart in the middle of a version change: the call waited for v1's
// candidate when the process stopped, and v2 was set while it was down. The
// resumed call doesn't give the old candidate to v2: it is answered as
// superseded, without a model call, and the agent's next conclusion is v2's
// first candidate, to the same verifier.
func TestVerify_RestartMidVersionChange(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("PASS: about v1", "PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	r.stopWhileWaiting()
	v := r.verifier()
	r.firstCallInFlight(vLLM)
	r.set(setBy(event.ByOperator, "the answer cites a source", 3))

	aLLM := turns(concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, nil)
	waitFor(t, "v2's candidate with the verifier", func() bool { return len(r.messages(v)) == 1 })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)

	if aLLM.CallCount() != 1 {
		t.Errorf("agent calls after the restart = %d, want 1: the resumed call is answered without one", aLLM.CallCount())
	}
	req := aLLM.Recorded()[0]
	assertPaired(t, req)
	if got := lastToolResult(req); got != verify.Withdrawn(verify.Label{Version: 1, Candidate: 1}, 2) {
		t.Errorf("the resumed call's result = %q", got)
	}
	if vs := r.verifiers(); len(vs) != 1 {
		t.Errorf("verifiers = %v, want the one", vs)
	}
	if !strings.Contains(r.messages(v)[0], "Candidate conclusion (v2, candidate 1):\nIt is 42 m (source: the survey).") {
		t.Errorf("v2's candidate = %q", r.messages(v)[0])
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." {
		t.Errorf("child_result = %q, want v2's candidate", cr.Content)
	}
	r.noCrashMarker()
}

// Cancelling an agent that waits in conclude stops it quietly and cascades to
// its verifier; the call is left open, for a revival to resume.
func TestVerify_CancelWhileWaiting(t *testing.T) {
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS"), gate: make(chan struct{})})
	done := r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	if err := session.CancelSession(r.store, r.registry, r.runID, "worker", "not needed"); err != nil {
		t.Fatal(err)
	}
	r.wait(done)
	if r.status("worker") != db.SessionCancelled || r.status(v) != db.SessionCancelled {
		t.Errorf("worker %s, verifier %s; want both cancelled", r.status("worker"), r.status(v))
	}
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	if _, _, open := db.OpenConclude(events); !open {
		t.Error("the waiting call got a result; it should be left open")
	}
}

// A final answer (a reply without tool calls) a restart left unconcluded goes
// to the verifier on the recovery path too, as a candidate the agent is told
// about, and the harness waits for the pass, which delivers it.
func TestVerify_ResumeAtRestStartsVerifier(t *testing.T) {
	r := newVerifyRig(t, script("PASS: fine"))
	ctx := context.Background()
	if err := r.store.CreateSession(ctx, db.SessionRecord{RunID: r.runID, SessionID: "worker", ParentID: "boss",
		AgentType: "standard_agent", Task: "How tall is it?", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := r.store.FinalizeStep(ctx, r.runID, "worker", 1, []event.Event{
		&event.UserEvent{Content: "How tall is it?"}, setBy("boss", "", 3),
		&event.AssistantEvent{Content: "It is 42 m.", StopReason: "end_turn"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil { // as the loop leaves it after a turn
		t.Fatal(err)
	}
	aLLM := turns()
	r.wait(r.start(aLLM, nil))
	if aLLM.CallCount() != 0 || len(r.verifiers()) != 1 {
		t.Errorf("agent calls = %d, verifiers = %v; want none (the harness waits) and 1 verifier", aLLM.CallCount(), r.verifiers())
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if msgs := r.messages("worker"); len(msgs) != 1 || msgs[0] != harness(verify.Submitted(r.verifier(), cand(1, 1))) {
		t.Errorf("verify messages = %q, want the submission", msgs)
	}
	if cr := r.result(); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q, want the reply the verifier passed", cr.Content)
	}
}

// A reply without tool calls, with nothing pending, is a candidate like
// conclude(result=text). It has no call to wait in, so the harness waits for
// the verdict, after a note saying so, and the pass delivers the reply: no
// turn is spent in between.
func TestVerify_ReplyIsACandidate(t *testing.T) {
	r := newVerifyRig(t, script("PASS: fine"))
	aLLM := turns(reply("It is 42 m."))
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	if msgs := r.messages("worker"); len(msgs) != 1 || msgs[0] != harness(verify.Submitted(r.verifier(), cand(1, 1))) {
		t.Errorf("verify messages = %q, want the submission", msgs)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the reply, verified", cr)
	}
	if aLLM.CallCount() != 1 {
		t.Errorf("agent calls = %d, want 1", aLLM.CallCount())
	}
}

// replyOnly is a model that only ever replies without tool calls, each reply
// different.
type replyOnly struct {
	*llm.MockProvider
	n atomic.Int64
}

func (m *replyOnly) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	m.MockProvider.Call(ctx, req) //nolint:errcheck // records the request
	return &llm.Response{Content: fmt.Sprintf("It is 42 m (reply %d).", m.n.Add(1)), StopReason: "end_turn"}, nil
}

// A model that never calls the tool doesn't spin: its first reply is the
// candidate, and the harness waits for the verdict instead of taking turns.
func TestVerify_ReplyOnlyModelWaits(t *testing.T) {
	gate := make(chan struct{})
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS"), gate: gate})
	aLLM := &replyOnly{MockProvider: &llm.MockProvider{Model: "m"}}
	done := r.start(aLLM, setBy("boss", "", 3))
	v := r.verifier()
	waitFor(t, "the harness waiting", func() bool { return r.status("worker") == db.SessionAwaiting })
	time.Sleep(300 * time.Millisecond)
	if n := aLLM.CallCount(); n != 1 {
		t.Errorf("agent calls while the verifier works = %d, want 1", n)
	}
	close(gate)
	r.wait(done)
	if cr := r.result(); cr.Content != "It is 42 m (reply 1)." {
		t.Errorf("child_result = %q, want the first reply", cr.Content)
	}
	if n := len(r.messages(v)); n != 0 {
		t.Errorf("candidates re-sent to the verifier = %d, want none", n)
	}
}

// A reply while a candidate is pending is never a candidate: it gets a nudge
// to call conclude, and after maxPendingNudges in a row the next reply is
// taken as conclude(): the harness waits for the pending candidate's verdict,
// which then delivers that candidate, not the reply. The model takes no turn
// while it waits, and a verdict message starts a new run of nudges.
func TestVerify_RepliesWhilePendingNeverReplaceTheCandidate(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: right"), gate: gate})
	replies := &replyOnly{MockProvider: &llm.MockProvider{Model: "m"}}
	done := r.start(&scriptThen{first: turns(concludeTurn("It is 42 m.")), then: replies}, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	r.message("status?")
	waitFor(t, "the harness waiting after the nudges", func() bool {
		// The nudges first, then the status: see waiting.
		return len(checkedMessages(r.store, r.runID, "worker", event.CheckConclude)) == maxPendingNudges && r.status("worker") == db.SessionAwaiting
	})
	time.Sleep(200 * time.Millisecond)
	if n := replies.CallCount(); n != maxPendingNudges+1 {
		t.Errorf("replies = %d, want %d (two nudged, then the wait)", n, maxPendingNudges+1)
	}
	gate <- struct{}{}
	r.wait(done)
	nudges := checkedMessages(r.store, r.runID, "worker", event.CheckConclude)
	if want := harness(verify.ReplyWhilePending(v, cand(1, 1), false, "")); len(nudges) != maxPendingNudges || nudges[0] != want {
		t.Errorf("nudges = %q, want %d of %q", nudges, maxPendingNudges, want)
	}
	if !slices.Contains(r.messages("worker"), harness(verify.StillWaiting(v, cand(1, 1)))) {
		t.Errorf("verify messages = %q, want the wait's note", r.messages("worker"))
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) {
		t.Errorf("history = %v, want the one candidate", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." {
		t.Errorf("child_result = %q, want the pending candidate, not a reply", cr.Content)
	}
	if n := len(r.messages(v)); n != 0 {
		t.Errorf("candidates re-sent to the verifier = %d, want none", n)
	}
}

// A reply taken as conclude() after the verifier stopped re-engages it with
// the pending candidate, and a pass delivers that candidate, not the reply:
// the verifying record carries what it gave.
func TestVerify_ReplyTakenAsConcludeAfterAStopKeepsTheCandidate(t *testing.T) {
	vLLM := &llm.MockProvider{Model: "m", Responses: []llm.Response{
		{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}}, // crashes on the first candidate
		{Content: "PASS: reply 1 says 42 m, which is right", StopReason: "end_turn"},
	}}
	r := newVerifyRig(t, vLLM)
	aLLM := &replyOnly{MockProvider: &llm.MockProvider{Model: "m"}}
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	v := r.verifiers()
	if len(v) != 1 || !hasPrefixIn(r.messages(v[0]), "[system] Agent `worker` concluded again") {
		t.Fatalf("verifiers %v, messages %q: want the one, re-engaged", v, r.messages(v[0]))
	}
	if m := r.messages(v[0]); !strings.Contains(m[0], "It is 42 m (reply 1).") {
		t.Errorf("re-engaged with %q, want the pending candidate, reply 1", m[0])
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m (reply 1)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("delivered %q marked %v, want reply 1, the candidate the verifier passed", cr.Content, cr.Notes)
	}
}

// scriptThen answers from first until it runs out, then from then.
type scriptThen struct {
	first *llm.MockProvider
	then  llm.Provider
}

func (s *scriptThen) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if s.first.CallCount() < len(s.first.Responses) {
		return s.first.Call(ctx, req)
	}
	return s.then.Call(ctx, req)
}
func (s *scriptThen) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return s.first.Stream(ctx, req)
}
func (s *scriptThen) ModelID() string { return "m" }
func (s *scriptThen) MaxTokens() int  { return 200000 }

// A verifier that can't be started (here: a model this server can't run)
// doesn't hold the agent forever: it concludes, marked not verified.
func TestVerify_UnstartableVerifierConcludes(t *testing.T) {
	r := newVerifyRig(t, script())
	v := setBy("boss", "", 3)
	v.Model = "some-model"
	r.wait(r.start(turns(concludeTurn("It is 42 m.")), v))
	cr := r.result()
	if cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
		!strings.Contains(cr.Notes[0].Text, "the verifier could not be started") {
		t.Errorf("child_result = %+v", cr)
	}
	if got := callResults(r.store, r.runID); len(got) != 1 ||
		!strings.HasPrefix(got[0], "Concluded, marked not verified: the verifier could not be started") {
		t.Errorf("conclude call result = %q", got)
	}
}

// What the agent reads for a VerifyEvent: someone else's change is a note
// with the version it starts or ends; the harness's own actions come with
// messages of their own.
func TestVerify_EventProjection(t *testing.T) {
	a := New(&agent.Env{}, Config{})
	replace := &event.VerifyEvent{Action: event.VerifyReplace, By: "operator", Criteria: "tests pass"}
	clear := &event.VerifyEvent{Action: event.VerifyClear, By: "boss"}
	var evs []db.EventRecord
	for _, e := range []event.Event{&event.UserEvent{Content: "task"}, setBy("boss", "", 3), replace, clear,
		&event.VerifyEvent{Action: event.VerifyWithdrawn, By: event.ByHarness}, setBy("boss", "", 3)} {
		evs = append(evs, db.EventRecord{Event: e, Step: 1})
	}
	for _, action := range []string{event.VerifyVerifying, event.VerifyRejected, event.VerifyPassed, event.VerifyExhausted,
		event.VerifyWithdrawn, event.VerifyStale, event.VerifyVerdict} {
		evs = append(evs, db.EventRecord{Event: &event.VerifyEvent{Action: action}, Step: 1})
	}
	_, msgs := a.buildMessages(context.Background(), evs)
	var notes []string
	for _, m := range msgs[1:] {
		if m.Role != llm.RoleUser {
			t.Errorf("a note as %s", m.Role)
		}
		notes = append(notes, m.Content)
	}
	want := []string{"[system] Verification v1: your conclusion will be checked by an independent verifier before it is " +
		"delivered, against: your current request. Show evidence",
		"[system] The operator replaced your verification request: v2 supersedes v1, so a conclusion of yours still with " +
			"the verifier for v1 is withdrawn and won't be delivered. Your next conclusion is checked against: tests pass.",
		"[system] Your parent boss turned verification off (v2 ended)",
		"[system] Verification v3:"}
	if len(notes) != len(want) {
		t.Fatalf("notes = %q, want %d", notes, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(notes[i], want[i]) {
			t.Errorf("note %d = %q, want it to start %q", i, notes[i], want[i])
		}
	}
}

// A version a loop's tick started is named by the tick's label: the agent
// reads no note for it, but for one line of the loop's criteria, if any, and
// nothing for the loop's set itself.
func TestVerify_LoopVersionProjection(t *testing.T) {
	a := New(&agent.Env{}, Config{})
	set := &event.LoopEvent{Action: event.LoopSet, ID: "2", Prompt: "run the tests", By: event.ByOperator,
		Verify: &event.LoopVerify{MaxRounds: 3}}
	tick1 := &event.UserEvent{Content: "[loop 2 · every 1m · tick 1 · verification v1] run the tests",
		Loop: &event.LoopTick{ID: "2", N: 1, Version: 1}}
	edit := &event.LoopEvent{Action: event.LoopEdit, ID: "2", Prompt: "run the tests", By: event.ByOperator,
		Verify: &event.LoopVerify{Criteria: "the tests pass", MaxRounds: 3}}
	replace := edit.Verify.Event("2")
	replace.Action = event.VerifyReplace
	tick2 := &event.UserEvent{Content: "[loop 2 · every 1m · tick 2 · verification v2] run the tests",
		Loop: &event.LoopTick{ID: "2", N: 2, Version: 2}}
	var evs []db.EventRecord
	for _, e := range []event.Event{&event.UserEvent{Content: "task"}, set, set.Verify.Event("2"), tick1, edit, replace, tick2} {
		evs = append(evs, db.EventRecord{Event: e, Step: 1})
	}
	_, msgs := a.buildMessages(context.Background(), evs)
	var got []string
	for _, m := range msgs {
		got = append(got, m.Content)
	}
	want := []string{"task", tick1.Content, "[system] Verification v2 checks your conclusion against: the tests pass.", tick2.Content}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("messages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// After a compaction, the versions started before the context still count:
// a note's version is read from the whole stream.
func TestVerify_VersionsBeforeACompaction(t *testing.T) {
	r := newVerifyRig(t, script())
	ctx := context.Background()
	if err := r.store.CreateSession(ctx, db.SessionRecord{RunID: r.runID, SessionID: "worker", ParentID: "boss",
		AgentType: "standard_agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{setBy("boss", "", 3), &event.VerifyEvent{Action: event.VerifyReplace, By: "boss"}} {
		if _, err := r.store.AppendEvent(ctx, r.runID, "worker", e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.store.CompactContext(ctx, r.runID, "worker", 1, "summary"); err != nil {
		t.Fatal(err)
	}
	// One that lands after the context was read is of this generation: not before it.
	if _, err := r.store.AppendEvent(ctx, r.runID, "worker", &event.VerifyEvent{Action: event.VerifyReplace, By: "boss"}); err != nil {
		t.Fatal(err)
	}
	all, _ := r.store.GetEvents(ctx, r.runID, "worker", db.EventFilter{})
	ag := r.workerAgent()
	later := db.EventRecord{EventID: "later", Step: 5, Generation: 1,
		Event: &event.VerifyEvent{Action: event.VerifyReplace, By: "operator"}}
	_, msgs := ag.buildMessages(ctx, []db.EventRecord{{EventID: "task", Event: &event.UserEvent{Content: "task"}}, later})
	if len(msgs) != 2 || !strings.Contains(msgs[1].Content, "v3 supersedes v2") {
		t.Errorf("the note after a compaction = %+v (stream: %d events)", msgs, len(all))
	}
}

// hookLLM runs hook during its first call, as an event landing while the
// model generates.
type hookLLM struct {
	*llm.MockProvider
	once sync.Once
	hook func()
}

func (h *hookLLM) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	h.once.Do(h.hook)
	return h.MockProvider.Call(ctx, req)
}

// Setting verification on an agent parked in await_event doesn't wake it: a
// VerifyEvent is a record. The agent reads the note at its next turn, which
// a real event starts.
func TestVerify_SetDoesNotWakeAwaitingAgent(t *testing.T) {
	r := newVerifyRig(t, script("PASS: fine"))
	aLLM := turns(toolTurn("await_event", `{"timeout":30}`), concludeTurn("It is 42 m."))
	var ag *EventLoopAgent
	await := coordination.AwaitEvent(&coordination.Deps{Store: r.store, RunID: r.runID, Registry: r.registry},
		"worker", func() *session.Handle { return ag.Handle() })
	ag = r.worker(aLLM, nil, await)
	done := r.startAgent(ag)
	waitFor(t, "worker awaiting", func() bool { return r.status("worker") == db.SessionAwaiting })

	r.set(setBy("boss", "", 3))
	time.Sleep(200 * time.Millisecond)
	if r.status("worker") != db.SessionAwaiting || aLLM.CallCount() != 1 {
		t.Fatalf("worker %s after %d calls: setting verification woke it", r.status("worker"), aLLM.CallCount())
	}
	r.message("answer now")
	r.wait(done)
	if !strings.Contains(lastUserContent(aLLM.Recorded()[1]), "Verification v1: your conclusion will be checked by an independent verifier") {
		t.Error("the turn after the wake didn't show the verification note")
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
}

// A VerifyEvent landing while the model generates its conclude call isn't
// input the call would conclude over: the result goes to the verifier, with
// no extra turn.
func TestVerify_SetDuringGenerationNoExtraTurn(t *testing.T) {
	r := newVerifyRig(t, script("PASS: fine"))
	aLLM := &hookLLM{MockProvider: turns(concludeTurn("It is 42 m.")), hook: func() {
		if _, err := verify.Set(context.Background(), r.store, r.registry, r.runID, "worker", setBy("boss", "", 3)); err != nil {
			t.Error(err)
		}
	}}
	r.wait(r.start(aLLM, nil))
	if aLLM.CallCount() != 1 {
		t.Errorf("agent calls = %d, want 1", aLLM.CallCount())
	}
	task, _ := r.store.GetSession(context.Background(), r.runID, r.verifier())
	if !strings.Contains(task.Task, "Candidate conclusion (v1, candidate 1):\nIt is 42 m.\n") {
		t.Errorf("verifier's candidate isn't the result generated alongside the change:\n%s", task.Task)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 {
		t.Errorf("child_result = %+v, want the candidate, verified", cr)
	}
}

// Input that lands while the agent generates its conclude call comes first:
// the call returns saying what came, before any check, so the result made
// before it reaches no verifier and costs no round. Here the operator's
// message turns verification on with new criteria, as a message with verify
// does; the answer that takes it into account is the one verified.
func TestVerify_MessageDuringConcludeGenerationComesFirst(t *testing.T) {
	vLLM := script("PASS: has feet")
	r := newVerifyRig(t, vLLM)
	aLLM := &hookLLM{MockProvider: turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m (138 ft).")), hook: func() {
		ctx := context.Background()
		if _, err := verify.Set(ctx, r.store, r.registry, r.runID, "worker", &event.VerifyEvent{Action: event.VerifySet,
			By: event.ByOperator, Criteria: "the height is also given in feet", MaxRounds: 1}); err != nil {
			t.Error(err)
		}
		if _, err := r.store.AppendEvent(ctx, r.runID, "worker", &event.UserEvent{Content: "Give it in feet too."}); err != nil {
			t.Error(err)
		}
	}}
	r.wait(r.start(aLLM, nil))
	want := verify.Overtaken("a message from the operator", verify.State{On: true})
	if got := callResults(r.store, r.runID); len(got) != 2 || got[0] != want {
		t.Errorf("conclude results = %q, want first %q", got, want)
	}
	vs := r.verifiers()
	if len(vs) != 1 || vLLM.CallCount() != 1 {
		t.Fatalf("verifiers %v after %d verifier calls, want one call of one", vs, vLLM.CallCount())
	}
	task, _ := r.store.GetSession(context.Background(), r.runID, vs[0])
	if strings.Contains(task.Task, "It is 42 m.\n") || !strings.Contains(task.Task, "It is 42 m (138 ft).") ||
		!strings.Contains(task.Task, "Give it in feet too.") {
		t.Errorf("verifier task:\n%s\nwant the answer to the message, and the message as the request", task.Task)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m (138 ft)." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the answer to the message, verified", cr)
	}
}

// A message that lands while the agent generates a new conclusion, its
// earlier one with the verifier, leaves that one where it is: nothing new goes
// to the verifier, and conclude() then waits for, and delivers, the pending
// one.
func TestVerify_MessageDuringConcludeGenerationLeavesThePending(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: 42 m is right"), gate: gate})
	aLLM := &nthCallLLM{MockProvider: turns(concludeTurn("It is 42 m."), concludeTurn("It is 41 m."), concludeTurn("")),
		n: 2, hook: func() { r.message("one more thing") }}
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	r.message("status?")
	r.waiting(2)
	st := r.state()
	if got := callResults(r.store, r.runID)[1]; got != verify.Overtaken("a message from boss", st) ||
		!strings.Contains(got, "your conclusion (v1, candidate 1; round 1) is still with `"+v+"`") {
		t.Errorf("second conclude result = %q, want it to say the pending conclusion stays", got)
	}
	if n := len(r.messages(v)); n != 0 || !slices.Equal(r.actions(), []string{"set", "verifying"}) || st.Candidate != "It is 42 m." {
		t.Errorf("candidates sent since = %d, history %v, candidate %q: want the first one alone", n, r.actions(), st.Candidate)
	}
	gate <- struct{}{}
	r.wait(done)
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the pending candidate, verified", cr)
	}
}

// A verifier whose conclude call a new candidate overtakes is told its verdict
// was on the earlier one and doesn't count, never invited to deliver it again;
// given again straight away, it is told so once more, and only a verdict it
// gives after that counts for the new candidate.
func TestVerify_VerdictOnAnEarlierCandidateDoesntCount(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(
		concludeTurn("PASS: 42 is right"), // formed for candidate 1
		concludeTurn("PASS: 42 is right"), // given again
		concludeTurn("PASS: 42 m, with its unit"),
	), gate: gate}}
	r := newVerifyRig(t, vLLM)
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
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want candidate 2, verified on the verifier's third verdict", cr)
	}
}

// The same verdict given after a turn of work, once the verifier was told
// its verdict was on the earlier candidate, is its verdict on the new one: it
// checked something.
func TestVerify_SameVerdictAfterCheckingCounts(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(concludeTurn("PASS: 42 is right"), toolTurn("check", `{}`),
		concludeTurn("PASS: 42 is right")), gate: gate}}
	r := newVerifyRig(t, vLLM)
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
	want := []string{verify.VerdictOnEarlier("a new candidate"), "Concluded."}
	if got := concludeResultsOf(r.store, r.runID, v); !slices.Equal(got, want) {
		t.Errorf("the verifier's conclude results = %q, want %q", got, want)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want candidate 2, verified", cr)
	}
}

// A verifier that keeps giving its verdict on the earlier candidate stops
// without a verdict, as a crash does, rather than hold the agent or pass what
// it never checked: not a round, and the agent's conclude() re-engages it.
func TestVerify_VerifierThatKeepsRedeliveringStops(t *testing.T) {
	gate := make(chan struct{}, 8)
	again := concludeTurn("PASS: 42 is right")
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(again, again, again, again,
		concludeTurn("PASS: 42 m, with its unit")), gate: gate}}
	r := newVerifyRig(t, vLLM)
	done := r.start(turns(concludeTurn("It is 42."), concludeTurn("It is 42 m."), concludeTurn("")), setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	waitFor(t, "the verifier's first call", func() bool { return vLLM.entered.Load() == 1 })
	r.message("give the unit")
	waitFor(t, "candidate 2 with the verifier", func() bool { return len(r.messages(v)) == 1 })
	for range 5 {
		gate <- struct{}{}
	}
	r.wait(done)
	got := callResults(r.store, r.runID)
	if len(got) != 3 || !strings.HasPrefix(got[1], "Your verifier `"+v+"` stopped (crashed") ||
		!strings.Contains(got[1], errRedelivering.Error()) {
		t.Errorf("worker's conclude results = %q, want the second told the verifier stopped, redelivering", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified ||
		vLLM.CallCount() != 5 {
		t.Errorf("child_result = %+v after %d verifier calls, want candidate 2 verified by the fifth", cr, vLLM.CallCount())
	}
	if st := r.state(); st.Rejections != 0 {
		t.Errorf("rejections = %d, want none: a stop isn't a round", st.Rejections)
	}
}

// A message giving a verifier the same candidate again (re-engaging it after
// a stop) is no new candidate: the verdict its conclude call gives over it is
// on that candidate, so it is answered to deliver it again, and it counts.
func TestVerify_SameCandidateAgainIsNoNewOne(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: turns(concludeTurn("PASS: 42 m"), concludeTurn("PASS: 42 m")),
		gate: gate}}
	r := newVerifyRig(t, vLLM)
	done := r.start(turns(concludeTurn("It is 42 m."), concludeTurn("")), setBy("boss", "", 1))
	r.waiting(0)
	v := r.verifier()
	waitFor(t, "the verifier's first call", func() bool { return vLLM.entered.Load() == 1 })
	if err := session.CancelSession(r.store, r.registry, r.runID, v, "operator stopped it"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the verifier revived", func() bool { return r.status(v) == db.SessionOngoing })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	got := concludeResultsOf(r.store, r.runID, v)
	if len(got) != 2 || !strings.HasPrefix(got[0], "Not concluded yet: this arrived meanwhile: ") ||
		!strings.HasSuffix(got[0], resultAgain) || got[1] != "Concluded." {
		t.Errorf("the verifier's conclude results = %q, want it asked to deliver its verdict again, then concluded", got)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the candidate, verified", cr)
	}
}

// A verifier's verdict call left open by a restart, with a new candidate
// arrived since, is answered the same way when it is resumed: its verdict was
// on the earlier candidate.
func TestVerify_ResumedVerdictCallOvertakenByACandidate(t *testing.T) {
	withJobs(t)
	store, runID, registry := testSetup(t)
	ctx := context.Background()
	task := verify.VerifierTask("worker", "How tall is it?", "", "It is 42.", verify.Label{Version: 1, Candidate: 1}, false)
	for _, s := range []db.SessionRecord{
		{RunID: runID, SessionID: "worker", AgentType: "standard_agent", Status: db.SessionAwaiting},
		{RunID: runID, SessionID: "checker", ParentID: "worker", AgentType: "standard_agent", Task: task,
			Status: db.SessionOngoing, Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
	} {
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendEventAtStep(ctx, runID, "checker", 0, &event.UserEvent{Content: task}); err != nil {
		t.Fatal(err)
	}
	seedResumeStep(t, store, runID, "checker")
	call := concludeTurn("PASS: 42 is right").ToolCalls[0]
	if err := store.AppendEventAtStep(ctx, runID, "checker", 1, &event.AssistantEvent{
		ToolCalls: []event.ToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, runID, "checker", &event.UserEvent{Content: verify.Delivery("worker", 1, "It is 42 m.",
		verify.Label{Version: 1, Candidate: 2}, false),
		Check: event.CheckVerify}); err != nil {
		t.Fatal(err)
	}
	checker := newT(testCfg{RunID: runID, SessionID: "checker", ParentID: "worker", Task: task, SystemPrompt: "verifier",
		LLM: turns(concludeTurn("PASS: 42 m, with its unit")), Store: store, Registry: registry, Workspace: plain.New("/tmp"),
		Tools: []*tool.Tool{conclude.New()}})
	runToEnd(t, checker)
	want := []string{verify.VerdictOnEarlier("a new candidate"), "Concluded."}
	if got := concludeResultsOf(store, runID, "checker"); !slices.Equal(got, want) {
		t.Errorf("the verifier's conclude results = %q, want %q", got, want)
	}
}

// A loop's verifying tick that lands while the agent generates its conclude
// call (the previous tick's answer) comes first, like any message: that
// answer goes to no verifier, and the tick's version checks the answer to the
// tick.
func TestVerify_VerifyingTickDuringConcludeGenerationComesFirst(t *testing.T) {
	vLLM := script("PASS: answers tick 2")
	r := newVerifyRig(t, vLLM)
	aLLM := &hookLLM{MockProvider: turns(concludeTurn("Tick 1: all quiet."), concludeTurn("Tick 2: all quiet.")),
		hook: func() {
			err := r.store.AppendEvents(context.Background(), r.runID, "worker",
				db.SelfEvent{Event: &event.VerifyEvent{Action: event.VerifySet, By: event.ByLoop("tidy-loop"), MaxRounds: 3}, Step: -1},
				db.SelfEvent{Event: &event.UserEvent{Content: "[loop tidy-loop · every hour · tick 2] Check the queue.",
					Loop: &event.LoopTick{ID: "tidy-loop", N: 2, Version: 1}}, Step: -1})
			if err != nil {
				t.Error(err)
			}
		}}
	r.wait(r.start(aLLM, nil))
	want := verify.Overtaken("a tick of loop tidy-loop", verify.State{On: true})
	if got := callResults(r.store, r.runID); len(got) != 2 || got[0] != want {
		t.Errorf("conclude results = %q, want first %q", got, want)
	}
	vs := r.verifiers()
	if len(vs) != 1 || vLLM.CallCount() != 1 {
		t.Fatalf("verifiers %v after %d verifier calls, want one call of one", vs, vLLM.CallCount())
	}
	task, _ := r.store.GetSession(context.Background(), r.runID, vs[0])
	if strings.Contains(task.Task, "Tick 1") || !strings.Contains(task.Task, "Tick 2: all quiet.") {
		t.Errorf("verifier task:\n%s\nwant the answer to tick 2 alone", task.Task)
	}
	if cr := r.result(); cr.Content != "Tick 2: all quiet." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the answer to tick 2, verified", cr)
	}
}

// concludeResultsOf are the results of a session's conclude calls, in order.
func concludeResultsOf(store db.Store, runID, sid string) []string {
	events, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	calls := map[string]bool{}
	var out []string
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *event.AssistantEvent:
			if tc, ok := ev.ConcludeCall(); ok {
				calls[tc.ID] = true
			}
		case *event.ToolResultEvent:
			if calls[ev.ToolCallID] {
				out = append(out, ev.Content)
			}
		}
	}
	return out
}

// enteredLLM counts the calls that reached it, before its gate.
type enteredLLM struct {
	*gatedLLM
	entered atomic.Int32
}

func (e *enteredLLM) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	e.entered.Add(1)
	return e.gatedLLM.Call(ctx, req)
}

// nthCallLLM runs hook during its nth call.
type nthCallLLM struct {
	*llm.MockProvider
	n     int
	calls atomic.Int32
	hook  func()
}

func (h *nthCallLLM) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if int(h.calls.Add(1)) == h.n {
		h.hook()
	}
	return h.MockProvider.Call(ctx, req)
}

// toolTurn is a scripted turn that calls one tool.
func toolTurn(name, args string) llm.Response {
	return llm.Response{StopReason: "tool_use", ToolCalls: []llm.ToolCall{{ID: name + "-1", Name: name, Arguments: args}}}
}

// untilVerdict is a tool that lets the verifier answer (a gate token) and
// returns once it has concluded: the verdict lands while the agent works.
func (r *verifyRig) untilVerdict(gate chan struct{}) *tool.Tool {
	return &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			gate <- struct{}{}
			// Its goroutine gone too, not just its terminal write: a message
			// that lands before the goroutine unregisters only bumps it, and
			// revives nothing.
			waitFor(r.t, "the verifier's verdict", func() bool {
				vs := r.verifiers()
				return len(vs) > 0 && r.status(vs[len(vs)-1]) == db.SessionConcluded && !r.registry.IsRegistered(vs[len(vs)-1])
			})
			return &tool.Result{Content: "worked"}, nil
		}}
}

// interrupted starts the worker, waits for its first conclude call to wait,
// and ends that wait with a message from its parent, so it goes back to work.
func (r *verifyRig) interrupted(aLLM llm.Provider, v *event.VerifyEvent, tools ...*tool.Tool) <-chan error {
	r.t.Helper()
	done := r.start(aLLM, v, tools...)
	r.waiting(0)
	r.message("keep at it")
	return done
}

// A pass that reaches the agent while it works is told at its next turn, in
// a verify message where the verifier's outcome landed (the outcome's record
// itself isn't shown, nor the verifier's text); conclude() then delivers the
// verified candidate.
func TestVerify_PassWhileWorkingIsTold(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: 42 m is right"), gate: gate})
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("work", `{}`), concludeTurn(""))
	r.wait(r.interrupted(aLLM, setBy("boss", "", 3), r.untilVerdict(gate)))
	v := r.verifier()
	cr := r.result()
	if cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the candidate, verified", cr)
	}
	passed := harness(verify.Passed(v, cand(1, 1)))
	if msgs := r.messages("worker"); !slices.Contains(msgs, passed) {
		t.Errorf("verify messages = %q, want %q", msgs, passed)
	}
	req := aLLM.Recorded()[2]
	told := 0
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "42 m is right") {
			t.Errorf("the agent read the verifier's own text: %q", m.Content)
		}
		if m.Content == passed {
			told++
		}
	}
	if last := req.Messages[len(req.Messages)-1]; told != 1 || last.Content != passed {
		t.Errorf("the turn after the verdict read the pass %d times, last %q; want once, last", told, last.Content)
	}
	if o := r.messageOutcomes()[0]; o == nil || withoutAt(o) != (event.VerifyOutcome{Verifier: v, Verdict: event.VerdictPass,
		Findings: "42 m is right", Round: 1, MaxRounds: 3}) {
		t.Errorf("the message's outcome = %+v", o)
	}
	// Told by the message, the pass isn't told again by the call that delivers
	// it: one verdict bubble.
	if n := r.toldOutcomes(); n != 1 {
		t.Errorf("told outcomes = %d, want 1", n)
	}
	r.noVerifierResults()
}

// toldOutcomes counts what in the worker's stream tells a verifier's outcome
// (call results and messages carrying one).
func (r *verifyRig) toldOutcomes() int {
	n := 0
	for _, o := range append(r.callOutcomes(), r.messageOutcomes()...) {
		if o != nil {
			n++
		}
	}
	return n
}

// A rejection that reaches the agent while it works is told, and counts its
// round, at the next turn; the new candidate goes back to the same verifier.
func TestVerify_RejectionWhileWorkingIsTold(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &gatedLLM{MockProvider: script("FAIL: no unit", "PASS"), gate: gate}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42."), toolTurn("work", `{}`), concludeTurn(""), concludeTurn("It is 42 m."))
	go func() { // the second verdict, once the new candidate is with the verifier
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if vs := r.verifiers(); len(vs) == 1 && len(r.messages(vs[0])) >= 1 { // >=: it may be sent twice
				break
			}
		}
		gate <- struct{}{}
	}()
	r.wait(r.interrupted(aLLM, setBy("boss", "", 3), r.untilVerdict(gate)))
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	// Told, the rejection is settled: conclude() then has nothing to wait for.
	if got := callResults(r.store, r.runID); len(got) != 3 || got[1] != nothingPending {
		t.Errorf("conclude call results = %q, want conclude() after the rejection to be an error", got)
	}
	if !hasPrefixIn(r.messages("worker"), "[system] `"+r.verifier()+"` rejected your conclusion (v1, candidate 1; round 1 of 3):\n\nno unit\n\n") {
		t.Errorf("verify messages = %q, want the findings in the rejection", r.messages("worker"))
	}
	if o := r.messageOutcomes()[0]; o == nil || o.Verdict != event.VerdictFail || o.Findings != "no unit" || o.Round != 1 ||
		o.MaxRounds != 3 {
		t.Errorf("the message's outcome = %+v", o)
	}
	if len(r.verifiers()) != 1 || !strings.Contains(lastUserContent(vLLM.Recorded()[1]), "Candidate conclusion (v1, candidate 2):\nIt is 42 m.") {
		t.Errorf("the new candidate didn't go back to the same verifier")
	}
}

// The last allowed rejection reaching the agent while it works ends the
// verification there: it is told, and conclude() delivers its candidate, not
// verified.
func TestVerify_ExhaustionWhileWorkingIsTold(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("FAIL: no unit"), gate: gate})
	aLLM := turns(concludeTurn("It is 42."), toolTurn("work", `{}`), concludeTurn(""))
	r.wait(r.interrupted(aLLM, setBy("boss", "", 1), r.untilVerdict(gate)))
	cr := r.result()
	if cr.Content != "It is 42." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
		!strings.Contains(cr.Notes[0].Text, "no unit") {
		t.Errorf("child_result = %+v, want the candidate, not verified", cr)
	}
	if !hasPrefixIn(r.messages("worker"), "[system] Verification v1 ended: not verified after 1 rejection (latest findings: no unit). Call conclude()") {
		t.Errorf("verify messages = %q", r.messages("worker"))
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "exhausted"}) {
		t.Errorf("history = %v", got)
	}
	if n := r.toldOutcomes(); n != 1 {
		t.Errorf("told outcomes = %d, want 1: the end told by message isn't told again by the call", n)
	}
}

// Once the end of the verification is told (the last allowed rejection,
// while the agent works), a conclusion with a new result concludes too, not
// verified, as the message promised: it doesn't buy another round. The same
// holds after a restart, from the stream.
func TestVerify_NewResultAfterEndIsNotVerified(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "after a restart"}[restart], func(t *testing.T) {
			gate := make(chan struct{}, 2)
			vLLM := &gatedLLM{MockProvider: script("FAIL: no unit", "PASS: 42 m"), gate: gate}
			r := newVerifyRig(t, vLLM)
			go func() { // a second round, if one were started, would pass
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					if vs := r.verifiers(); len(vs) == 1 && len(r.messages(vs[0])) == 1 {
						gate <- struct{}{}
						return
					}
				}
			}()
			last := concludeTurn("It is 42 m.")
			first := turns(concludeTurn("It is 42."), toolTurn("work", `{}`), last)
			if restart {
				// The first life stops right after it is told; the next one
				// concludes with the new result.
				first = turns(concludeTurn("It is 42."), toolTurn("work", `{}`), llm.Response{StopReason: "refusal",
					Refusal: &llm.Refusal{Category: "test"}})
			}
			done := r.interrupted(first, setBy("boss", "", 1), r.untilVerdict(gate))
			if restart {
				if err := <-done; !errors.Is(err, errProviderRefused) {
					t.Fatalf("first life = %v, want the refusal that stops it", err)
				}
				if err := r.store.UpdateSessionStatus(context.Background(), r.runID, "worker", db.SessionOngoing); err != nil {
					t.Fatal(err)
				}
				done = r.start(turns(last), nil)
			}
			r.wait(done)
			cr := r.result()
			if cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
				!strings.Contains(cr.Notes[0].Text, "after 1 rejection") {
				t.Errorf("child_result = %+v, want the new result, not verified after 1 rejection", cr)
			}
			if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "exhausted"}) {
				t.Errorf("history = %v, want no second round", got)
			}
			if n := len(r.messages(r.verifier())); n != 0 {
				t.Errorf("the verifier got %d more candidates, want none", n)
			}
		})
	}
}

// Once the agent was told its verification ended, a verdict the verifier
// gives after that doesn't count: here its parent's message re-engages the
// verifier, which passes on a second look, and conclude() still delivers the
// candidate not verified, with the rejection it was told.
func TestVerify_VerdictAfterTheEndDoesNotCount(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("FAIL: no unit", "PASS: re-checked as asked; 42 is fine"), gate: gate})
	aLLM := turns(concludeTurn("It is 42."), toolTurn("work", `{}`), toolTurn("recheck", `{}`), concludeTurn(""),
		concludeTurn(""))
	r.wait(r.interrupted(aLLM, setBy("boss", "", 1), r.untilVerdict(gate), r.recheck(gate)))
	cr := r.result()
	if cr.Content != "It is 42." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
		!strings.Contains(cr.Notes[0].Text, "no unit") {
		t.Errorf("child_result = %+v, want the candidate, not verified, with the findings it was told", cr)
	}
	if msgs := r.messages("worker"); !hasPrefixIn(msgs, "[system] Verification v1 ended: not verified") ||
		hasPrefixIn(msgs, "[system] `"+r.verifier()+"` passed") {
		t.Errorf("verify messages = %q, want the end and no pass", msgs)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "exhausted"}) {
		t.Errorf("history = %v", got)
	}
}

// recheck is a tool that has the worker's parent ask its verifier to look
// again (a gate token lets it answer) and returns once it has: the verifier
// gives a second verdict on the same candidate.
func (r *verifyRig) recheck(gate chan struct{}) *tool.Tool {
	return &tool.Tool{Name: "recheck", Description: "recheck", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			v := r.verifiers()[0]
			if _, err := r.store.AppendEvent(context.Background(), r.runID, v, &event.MessageEvent{
				Content: "Please look again: 42 is right.", Sender: "boss", SenderType: event.SenderTypeAgent}); err != nil {
				return nil, err
			}
			gate <- struct{}{}
			waitFor(r.t, "the re-engaged verifier's second verdict", func() bool {
				return r.status(v) == db.SessionConcluded && r.assistantTurns(v) >= 2
			})
			return &tool.Result{Content: "rechecked"}, nil
		}}
}

// assistantTurns counts sid's turns.
func (r *verifyRig) assistantTurns(sid string) int {
	events, _ := r.store.GetEvents(context.Background(), r.runID, sid, db.EventFilter{})
	n := 0
	for _, e := range events {
		if _, ok := e.Event.(*event.AssistantEvent); ok {
			n++
		}
	}
	return n
}

// A change to an agent's verification made during its bootstrap (its
// parent's send_message, say, once the session exists) is newer than the
// request it was started with, and stands: the start's request isn't recorded
// over it.
func TestVerify_ChangeDuringBootstrapStands(t *testing.T) {
	r := newVerifyRig(t, script("PASS"))
	newer := setBy("boss", "the newer criteria", 2)
	ag := r.worker(turns(concludeTurn("It is 42 m."), concludeTurn("")), setBy("boss", "the start's criteria", 1))
	ag.cfg.InitialRecall = func(ctx context.Context, _ string) string {
		if _, err := verify.Set(ctx, r.store, r.registry, r.runID, "worker", newer); err != nil {
			r.t.Error(err)
		}
		return ""
	}
	r.wait(r.startAgent(ag))
	st := r.state()
	if len(st.History) == 0 || st.History[0].Criteria != "the newer criteria" ||
		slices.ContainsFunc(st.History, func(e verify.Entry) bool { return e.Criteria == "the start's criteria" }) {
		t.Errorf("history = %+v, want the change made during the bootstrap, and not the start's request", st.History)
	}
}

// A newer candidate reaches the verifier while it still works on the older
// one: its verdict on the older one doesn't count (it isn't even given: the
// newer one sends it back for another turn), and its verdict on the newer one
// does.
func TestVerify_NewerCandidateSupersedesOlder(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("PASS: 42 is fine", "FAIL: 41 is wrong"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42."), concludeTurn("It is 41."))
	done := r.start(aLLM, setBy("boss", "", 1))
	r.waiting(0)
	// The verifier's first call is in flight, its context without the newer
	// candidate, before the agent submits it.
	waitFor(t, "the verifier's first call", func() bool { return vLLM.entered.Load() == 1 })
	r.message("keep at it")
	waitFor(t, "the newer candidate delivered", func() bool {
		vs := r.verifiers()
		return len(vs) == 1 && len(r.messages(vs[0])) >= 1 && r.status("worker") == db.SessionAwaiting
	})
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	cr := r.result()
	if cr.Content != "It is 41." || len(cr.Notes) != 1 || !strings.Contains(cr.Notes[0].Text, "41 is wrong") {
		t.Errorf("child_result = %+v, want the newer candidate, not verified, with its findings", cr)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "exhausted"}) {
		t.Errorf("history = %v (a stale pass must not count)", got)
	}
}

// A verifier that keeps stopping without a verdict isn't a round, but after
// MaxStops in a row the call concludes, not verified, with the last reason.
func TestVerify_RepeatedStopsEnd(t *testing.T) {
	refuse := llm.Response{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}}
	r := newVerifyRig(t, &llm.MockProvider{Model: "m", Responses: []llm.Response{refuse, refuse, refuse}})
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn(""), concludeTurn(""), concludeTurn("unused"))
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	cr := r.result()
	if cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified ||
		!strings.Contains(cr.Notes[0].Text, "the verifier stopped 3 times without a verdict; last: crashed: provider refused") {
		t.Errorf("child_result = %+v", cr)
	}
	if aLLM.CallCount() != 3 || len(r.verifiers()) != 1 {
		t.Errorf("agent calls = %d, verifiers = %v; want 3 and one verifier re-engaged", aLLM.CallCount(), r.verifiers())
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "verifying", "exhausted"}) {
		t.Errorf("history = %v", got)
	}
	// Each stop is told, though they read alike: they are different stops.
	if o := r.callOutcomes(); len(o) != 3 || o[0] == nil || o[1] == nil || o[2] == nil || o[0].At.Equal(o[1].At) {
		t.Errorf("the calls' outcomes = %+v, want each stop told", o)
	}
}

// A cancelled verifier (not by a replace or clear) isn't a round either: the
// call returns, and conclude() revives it with the candidate.
func TestVerify_CancelledVerifierReengaged(t *testing.T) {
	// Two turns may be needed: cancelled mid-call, the revived verifier redoes
	// that call first, then reads the candidate that arrived meanwhile.
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS", "PASS"), gate: gate})
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn(""))
	done := r.start(aLLM, setBy("boss", "", 1))
	r.waiting(0)
	v := r.verifier()
	if err := session.CancelSession(r.store, r.registry, r.runID, v, "operator stopped it"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the verifier revived", func() bool { return r.status(v) == db.SessionOngoing })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	if got := callResults(r.store, r.runID); got[0] != verify.Stopped(v, verify.Label{Version: 1, Candidate: 1}, "cancelled: operator stopped it") {
		t.Errorf("result of the waiting call = %q", got[0])
	}
	if cr := r.result(); !slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: v + " (round 1)"}) {
		t.Errorf("child_result notes = %+v, want verified by the revived %s in round 1", cr.Notes, v)
	}
}

// A candidate that reaches no running verifier (its respawn fails) doesn't
// hold the agent forever: after one more try it concludes, not verified.
func TestVerify_UnrevivableVerifierConcludes(t *testing.T) {
	vLLM := script("FAIL: no unit", "PASS")
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42."), concludeTurn("It is 42 m."))
	r.noRespawn.Store("*", true) // the first start is spawn_agent's; only revivals fail
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	cr := r.result()
	if cr.Content != "It is 42 m." || len(cr.Notes) != 1 || !strings.Contains(cr.Notes[0].Text, "didn't take up the candidate") {
		t.Errorf("child_result = %+v", cr)
	}
	if vLLM.CallCount() != 1 {
		t.Errorf("verifier calls = %d, want 1 (never revived)", vLLM.CallCount())
	}
}

// A candidate that lands while the verifier's goroutine is still on its way
// out (it bumps that goroutine and revives nothing) is sent again once the
// goroutine is gone, and then read.
func TestVerify_CandidateCaughtByExitingVerifierResent(t *testing.T) {
	vLLM := script("FAIL: no unit", "PASS")
	r := newVerifyRig(t, vLLM)
	// The stand-in below holds the verifier's slot until the second candidate
	// has bounced off it: committed to the verifier's stream and notified while
	// the slot is held (the store notifies after the commit). Released earlier,
	// the notification would revive the verifier, which would read the
	// candidate without the resend this test is about.
	var holding atomic.Bool
	bounced := make(chan struct{})
	var bounce sync.Once
	r.store.SetCommitListener(func(runID, sid string, evt event.Event) {
		r.notify(runID, sid, evt)
		if u, ok := evt.(*event.UserEvent); ok && u.Check == event.CheckVerify && holding.Load() {
			bounce.Do(func() { close(bounced) })
		}
	})
	var once sync.Once
	done := r.start(&exitingVerifierLLM{inner: turns(concludeTurn("It is 42."), concludeTurn("It is 42 m.")), onSecond: func() {
		once.Do(func() {
			// Stand in for its goroutine on the way out: hold its slot (once the
			// real one has gone), as an exiting goroutine would, until the
			// second candidate bounces off it.
			vs := r.verifiers()
			if len(vs) != 1 {
				t.Errorf("verifiers = %v", vs)
				return
			}
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if r.registry.Register(vs[0], session.NewHandle(func() {})) == nil {
					holding.Store(true)
					go func() {
						select {
						case <-bounced:
						case <-time.After(5 * time.Second):
							t.Error("the second candidate never reached the verifier")
						}
						r.registry.Unregister(vs[0])
					}()
					return
				}
			}
			t.Error("the verifier's goroutine never exited")
		})
	}}, setBy("boss", "", 3))
	r.wait(done)
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	v := r.verifier()
	if n := len(r.messages(v)); n != 2 {
		t.Errorf("verifier got %d candidate messages, want 2 (sent again after the exiting goroutine)", n)
	}
}

// exitingVerifierLLM runs onSecond during the agent's second call, which
// generates the candidate that will reach the "exiting" verifier.
type exitingVerifierLLM struct {
	inner    *llm.MockProvider
	onSecond func()
	calls    int
	mu       sync.Mutex
}

func (e *exitingVerifierLLM) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	e.mu.Unlock()
	if n == 2 {
		e.onSecond()
	}
	return e.inner.Call(ctx, req)
}
func (e *exitingVerifierLLM) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return e.inner.Stream(ctx, req)
}
func (e *exitingVerifierLLM) ModelID() string { return "m" }
func (e *exitingVerifierLLM) MaxTokens() int  { return 200000 }

// stopWhileWaiting runs the worker until its conclude call waits, then stops
// it as a process stop would: the status stays awaiting and the call open.
func (r *verifyRig) stopWhileWaiting() {
	r.t.Helper()
	done := r.start(turns(concludeTurn("It is 42.")), setBy("boss", "", 3))
	r.waiting(0)
	r.registry.Interrupt("worker")
	r.wait(done)
	waitFor(r.t, "the worker's goroutine gone", func() bool { return !r.registry.IsRegistered("worker") })
	events, _ := r.store.GetEvents(context.Background(), r.runID, "worker", db.EventFilter{})
	if _, _, open := db.OpenConclude(events); !open || r.status("worker") != db.SessionAwaiting {
		r.t.Fatalf("after the stop: worker %s, call open %v; want awaiting, open", r.status("worker"), open)
	}
}

// noCrashMarker fails if a conclude call was mended as interrupted.
func (r *verifyRig) noCrashMarker() {
	r.t.Helper()
	for _, res := range callResults(r.store, r.runID) {
		if strings.Contains(res, "interrupted by process crash") {
			r.t.Errorf("the open conclude call was mended as interrupted: %q", res)
		}
	}
}

// After a restart while the call waited (its goroutine gone, the verifier
// still at work), the next life resumes the wait without a model call: the
// open call isn't mended as interrupted, and the pass concludes it.
func TestVerify_RestartResumesTheWait(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS"), gate: gate})
	r.stopWhileWaiting()
	aLLM := turns()
	done := r.start(aLLM, nil)
	r.waiting(0)
	gate <- struct{}{}
	r.wait(done)
	if aLLM.CallCount() != 0 {
		t.Errorf("agent calls after the restart = %d, want 0: the resumed call settles it", aLLM.CallCount())
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "passed"}) || len(r.verifiers()) != 1 {
		t.Errorf("history = %v, verifiers = %v", got, r.verifiers())
	}
	if cr := r.result(); cr.Content != "It is 42." {
		t.Errorf("child_result = %q, want the candidate", cr.Content)
	}
	r.noCrashMarker()
}

// A verdict that lands while the process is down is settled by the resumed
// call at once: a rejection returns the findings as the call's result, and
// the agent's next turn reads them, its history valid.
func TestVerify_RestartSettlesAVerdictThatLanded(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("FAIL: no unit", "PASS"), gate: gate})
	r.stopWhileWaiting()
	gate <- struct{}{}
	v := r.verifier()
	waitFor(t, "the verdict, while the worker is down", func() bool { return r.status(v) == db.SessionConcluded })
	gate <- struct{}{}
	aLLM := turns(concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, nil))
	if aLLM.CallCount() != 1 {
		t.Fatalf("agent calls after the restart = %d, want 1", aLLM.CallCount())
	}
	req := aLLM.Recorded()[0]
	assertPaired(t, req)
	if !strings.Contains(lastToolResult(req), "rejected your conclusion (v1, candidate 1; round 1 of 3):\n\nno unit") {
		t.Errorf("the resumed call's result = %q, want the findings", lastToolResult(req))
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
	r.noCrashMarker()
}

// A session that concluded through a verified conclude call, revived by a
// follow-up, sends the provider a history in which every tool call has its
// result; the follow-up's own conclusion goes through (the pass ended the
// verification).
func TestVerify_RevivedAfterConclusionHistoryIsValid(t *testing.T) {
	r := newVerifyRig(t, script("PASS: fine"))
	r.wait(r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3)))
	if _, err := r.store.AppendEvent(context.Background(), r.runID, "worker", &event.UserEvent{Content: "And in feet?"}); err != nil {
		t.Fatal(err)
	}
	aLLM := turns(concludeTurn("About 138 ft."))
	r.wait(r.start(aLLM, nil))
	req := aLLM.Recorded()[0]
	assertPaired(t, req)
	if last := req.Messages[len(req.Messages)-1]; last.Role != llm.RoleUser || last.Content != "And in feet?" {
		t.Errorf("last message = %+v, want the follow-up", last)
	}
	if cr := r.result(); cr.Content != "About 138 ft." || len(cr.Notes) != 0 {
		t.Errorf("child_result = %+v, want the follow-up's result, unverified", cr)
	}
}

// A conclusion attempt while the process stops doesn't conclude the agent
// "not verified": a failure to start a verifier then says nothing about the
// verifier, and the attempt is left for the recovery.
func TestVerify_StoppingProcessLeavesAttempt(t *testing.T) {
	r := newVerifyRig(t, script("PASS"))
	ctx := context.Background()
	if err := r.store.CreateSession(ctx, db.SessionRecord{RunID: r.runID, SessionID: "worker", ParentID: "boss",
		AgentType: "standard_agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AppendEvent(ctx, r.runID, "worker", setBy("boss", "", 3)); err != nil {
		t.Fatal(err)
	}
	ag := r.workerAgent()
	at := attempt{result: "It is 42 m.", call: &event.ToolCall{ID: "c1", Name: event.ConcludeTool}}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if out, err := ag.conclusionCheck(cancelled, nil, at); err == nil || out.record != nil {
		t.Errorf("cancelled attempt = %+v, %v; want an error and no record", out, err)
	}
	r.registry.CancelAll() // the registry closes: no verifier can start
	if out, err := ag.conclusionCheck(ctx, nil, at); err == nil || out.record != nil {
		t.Errorf("attempt in a stopping process = %+v, %v; want an error and no record", out, err)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set"}) {
		t.Errorf("history = %v", got)
	}
}

// Set and Clear wait for the session's verification lock, which the
// conclusion check holds while it starts and records a verifier.
func TestVerify_ChangesSerializeWithTheCheck(t *testing.T) {
	r := newVerifyRig(t, script())
	if err := r.store.CreateSession(context.Background(), db.SessionRecord{RunID: r.runID, SessionID: "worker",
		ParentID: "boss", AgentType: "standard_agent", Status: db.SessionAwaiting}); err != nil {
		t.Fatal(err)
	}
	unlock := verify.Lock(r.runID, "worker")
	set := make(chan error, 1)
	go func() {
		_, err := verify.Set(context.Background(), r.store, r.registry, r.runID, "worker", setBy("boss", "", 3))
		set <- err
	}()
	select {
	case err := <-set:
		t.Fatalf("Set didn't wait for the lock (it returned %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-set; err != nil {
		t.Fatal(err)
	}

	ag := r.workerAgent()
	unlock = verify.Lock(r.runID, "worker")
	check := make(chan error, 1)
	go func() {
		_, err := ag.conclusionCheck(context.Background(), nil, attempt{result: "It is 42 m."})
		check <- err
	}()
	select {
	case err := <-check:
		t.Fatalf("the conclusion check didn't wait for the lock (it returned %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-check; err != nil {
		t.Fatal(err)
	}
}

// A sub-agent's verification outcome isn't logged at info: those lines speak
// for the run (the root), which a harness keys on.
func TestVerify_SubAgentOutcomeNotAtInfo(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	r := newVerifyRig(t, script("PASS"))
	r.wait(r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3)))
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "verification passed") {
		t.Errorf("a sub-agent's outcome was logged at info:\n%s", buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// strandVerifier sets up a worker whose latest candidate (offered by a
// conclude call at step 1) reached its verifier dusky-gecko as it concluded
// on an older one, so the verifier never read it: concluded, not running, the
// candidate unanswered.
func (r *verifyRig) strandVerifier() {
	r.t.Helper()
	ctx := context.Background()
	r.noRespawn.Store("dusky-gecko", true) // while the stranded candidate is set up
	for _, s := range []db.SessionRecord{
		{SessionID: "worker", ParentID: "boss", AgentType: "standard_agent", Status: db.SessionOngoing},
		{SessionID: "dusky-gecko", ParentID: "worker", AgentType: testVerifierType, Status: db.SessionConcluded,
			Task: "verify: It is 42.", Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
	} {
		s.RunID = r.runID
		if err := r.store.CreateSession(ctx, s); err != nil {
			r.t.Fatal(err)
		}
	}
	appendAt := func(sid string, step int, evs ...event.Event) {
		for _, e := range evs {
			if err := r.store.AppendEventAtStep(ctx, r.runID, sid, step, e); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	call := concludeTurn("It is 42 m.").ToolCalls[0]
	appendAt("worker", 1, setBy("boss", "", 3),
		&event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}}},
		&event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness, Verifier: "dusky-gecko", Round: 1})
	appendAt("dusky-gecko", 0, &event.UserEvent{Content: "verify: It is 42."})
	appendAt("dusky-gecko", 1, &event.AssistantEvent{Content: "FAIL: no unit"})
	appendAt("dusky-gecko", 2, &event.UserEvent{Content: verify.Delivery("worker", 1, "It is 42 m.",
		verify.Label{Version: 1, Candidate: 1}, false), Check: event.CheckVerify})
	for range 2 { // each concluded at step 1, so is at step 2 (where the candidate landed)
		for _, sid := range []string{"worker", "dusky-gecko"} {
			if _, err := r.store.AdvanceStep(ctx, r.runID, sid); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	r.noRespawn.Delete("dusky-gecko")
}

func (r *verifyRig) workerAgent() *EventLoopAgent {
	ag := r.worker(script(), nil)
	ag.handle = session.NewHandle(func() {})
	return ag
}

// A call woken by its verifier's result for an older candidate, when the
// latest one was never read, sends the latest one again, which revives the
// verifier, and says the result was for an earlier conclusion: its rejection
// isn't a round, but its findings are told.
func TestVerify_WakeOnStaleResultResends(t *testing.T) {
	vLLM := script("PASS: 42 m is right")
	r := newVerifyRig(t, vLLM)
	r.strandVerifier()
	out, err := r.workerAgent().settle(context.Background(), nil)
	want := verify.Earlier("dusky-gecko", false, "no unit", true) + "\n\n" + verify.Superseded("dusky-gecko", cand(1, 1))
	if err != nil || out.kind != holds || out.text != want || out.told == nil || out.told.Round != 0 || !out.told.Earlier ||
		out.told.Verdict != event.VerdictFail || len(out.records) != 0 {
		t.Fatalf("settle = %+v, %v; want the earlier rejection told, no round counted", out, err)
	}
	if n := len(r.messages("dusky-gecko")); n != 2 {
		t.Errorf("verifier has %d candidate messages, want 2 (sent again)", n)
	}
	waitFor(t, "the verifier's answer", func() bool { return vLLM.CallCount() == 1 && r.status("dusky-gecko") == db.SessionConcluded })
}

// conclude() while the candidate sits unread at a verifier that isn't
// running sends it again (rather than waiting on nothing), then waits.
func TestVerify_WaitResendsStrandedCandidate(t *testing.T) {
	vLLM := script("PASS: 42 m is right")
	r := newVerifyRig(t, vLLM)
	r.strandVerifier()
	out, err := r.workerAgent().conclusionCheck(context.Background(), nil,
		attempt{call: &event.ToolCall{ID: "c2", Name: event.ConcludeTool}, step: 2})
	if err != nil || out.kind != waits || out.verifier != "dusky-gecko" {
		t.Errorf("check = %+v, %v; want a wait on dusky-gecko", out, err)
	}
	if n := len(r.messages("dusky-gecko")); n != 2 {
		t.Errorf("verifier has %d candidate messages, want 2 (sent again)", n)
	}
	waitFor(t, "the verifier's answer", func() bool { return vLLM.CallCount() == 1 && r.status("dusky-gecko") == db.SessionConcluded })
}

// The third stop in a row, found at an attempt to conclude rather than in a
// wait, ends the verification there: the attempt concludes, not verified.
func TestVerify_RepeatedStopsEndAtAttempt(t *testing.T) {
	r := newVerifyRig(t, script())
	r.strandVerifier()
	ctx := context.Background()
	if err := r.store.UpdateSessionStatus(ctx, r.runID, "dusky-gecko", db.SessionCrashed); err != nil {
		t.Fatal(err)
	}
	for range verify.MaxStops {
		if _, err := r.store.AppendEvent(ctx, r.runID, "worker", &event.ChildResultEvent{ChildSessionID: "dusky-gecko",
			Verdict: db.SessionCrashed, Content: "provider refused the turn"}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.workerAgent().conclusionCheck(ctx, nil, attempt{call: &event.ToolCall{ID: "c2", Name: event.ConcludeTool}, step: 2})
	if err != nil || out.kind != concludes || out.result != "It is 42 m." || out.record == nil ||
		out.record.Action != event.VerifyExhausted || len(out.notes) != 1 ||
		!strings.Contains(out.notes[0].Text, "stopped 3 times without a verdict; last: crashed: provider refused the turn") ||
		out.record.Reason != out.notes[0].Text {
		t.Errorf("check = %+v, %v; want the candidate concluded, not verified", out, err)
	}
}

// An end told for the verifier's stops stands too: a pass it gives after that
// (re-engaged by someone's message) doesn't count, and the attempt concludes,
// not verified, for the stops it was told.
func TestVerify_StopsEndToldStandsAfterAVerdict(t *testing.T) {
	r := newVerifyRig(t, script())
	r.strandVerifier()
	ctx := context.Background()
	for range verify.MaxStops {
		if _, err := r.store.AppendEvent(ctx, r.runID, "worker", &event.ChildResultEvent{ChildSessionID: "dusky-gecko",
			Verdict: db.SessionCrashed, Content: "provider refused the turn"}); err != nil {
			t.Fatal(err)
		}
	}
	ended := verify.Ended(1, verify.StopsReason(verify.MaxStops, "crashed: provider refused the turn"))
	told := &event.VerifyOutcome{Verifier: "dusky-gecko", Verdict: db.SessionCrashed, Findings: "provider refused the turn"}
	secondLook := "PASS: fine on a second look"
	for _, w := range []struct {
		sid string
		e   event.Event
	}{
		{"worker", verifyMessage(ended, told)},
		{"dusky-gecko", &event.AssistantEvent{Content: secondLook}},
		{"worker", event.VerifierOutcome("dusky-gecko", secondLook, "", time.Now())},
	} {
		if _, err := r.store.AppendEvent(ctx, r.runID, w.sid, w.e); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.workerAgent().conclusionCheck(ctx, nil, attempt{call: &event.ToolCall{ID: "c2", Name: event.ConcludeTool}, step: 2})
	if err != nil || out.kind != concludes || out.record == nil || out.record.Action != event.VerifyExhausted ||
		len(out.notes) != 1 || !strings.Contains(out.notes[0].Text, "stopped 3 times without a verdict; last: crashed: provider refused the turn") ||
		out.told == nil || *out.told != *told {
		t.Errorf("check = %+v, %v; want the candidate concluded, not verified, for the stops it was told", out, err)
	}
}

// A conclude call never concludes over input the agent hasn't read: here a
// pass is in, but a message came meanwhile, so the call returns saying both,
// and conclude() then delivers the verified candidate.
func TestVerify_PassWithUnreadInputHolds(t *testing.T) {
	r := newVerifyRig(t, script())
	r.strandVerifier()
	ctx := context.Background()
	if err := r.store.AppendEventAtStep(ctx, r.runID, "dusky-gecko", 2, &event.AssistantEvent{Content: "PASS: right"}); err != nil {
		t.Fatal(err)
	}
	r.message("one more thing")
	ag := r.workerAgent()
	open, _ := r.store.GetEvents(ctx, r.runID, "worker", db.EventFilter{})
	call, step, ok := db.OpenConclude(open)
	if !ok {
		t.Fatal("no open conclude call")
	}
	done, err := ag.runConcludeCall(ctx, nil, call, step)
	if err != nil || done || r.status("worker") == db.SessionConcluded {
		t.Fatalf("runConcludeCall = %v, %v (worker %s); want it held", done, err, r.status("worker"))
	}
	want := "Not concluded yet: this arrived meanwhile: a message from boss. " + verify.Passed("dusky-gecko", cand(1, 1))
	if got := callResults(r.store, r.runID); len(got) != 1 || got[0] != want {
		t.Errorf("call result = %q, want %q", got, want)
	}
	if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil { // the message is read now
		t.Fatal(err)
	}
	done, err = ag.runConcludeCall(ctx, nil, event.ToolCall{ID: "c2", Name: event.ConcludeTool, Arguments: "{}"}, 2)
	if err != nil || !done {
		t.Fatalf("conclude() after reading it = %v, %v; want it concluded", done, err)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v", cr)
	}
}

// A verifier whose goroutine fails before creating its session (so it will
// never answer, nor write a child_result) doesn't hold the agent: it
// concludes, not verified.
func TestVerify_VerifierThatNeverStartsConcludes(t *testing.T) {
	r := newVerifyRig(t, script())
	verifierAgentType = brokenVerifierType
	r.wait(r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3)))
	cr := r.result()
	if len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteNotVerified || !strings.Contains(cr.Notes[0].Text, "didn't start") {
		t.Errorf("child_result notes = %+v", cr.Notes)
	}
}

// A store failure while starting or re-engaging the verifier says nothing
// about the verifier: the attempt is left for the recovery, not concluded.
func TestVerify_StoreErrorLeavesAttempt(t *testing.T) {
	r := newVerifyRig(t, script())
	out, err := r.workerAgent().unstarted(context.Background(), verify.State{}, "It is 42 m.", fmt.Errorf("%w: disk I/O error", db.ErrStore))
	if err == nil || out.record != nil {
		t.Errorf("unstarted on a store error = %+v, %v; want the error and no conclusion", out, err)
	}
}

// conclude() on the pending candidate passes the end-of-run reminder too: work
// started after an early return (here a background job) is reminded of
// before the pass could conclude over it.
func TestVerify_WaitIsRemindedOfNewWork(t *testing.T) {
	gate := make(chan struct{})
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS"), gate: gate})
	var started atomic.Bool
	orig := scanProcs
	scanProcs = func(string) proctree.Snapshot {
		if !started.Load() {
			return proctree.Snapshot{Supported: true}
		}
		return proctree.Snapshot{Supported: true, Roots: []*proctree.Process{{PID: 48213, PGID: 48213,
			SessionID: "worker", Elapsed: 5, Cmdline: "./bench.sh"}}}
	}
	t.Cleanup(func() { scanProcs = orig })
	start := &tool.Tool{Name: "start", Description: "start a job", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			started.Store(true)
			return &tool.Result{Content: "started"}, nil
		}}
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("start", `{}`), concludeTurn(""), concludeTurn(""))
	done := r.start(aLLM, setBy("boss", "", 3), start)
	// After the reminder, the last conclude() waits again, for the pass.
	r.waiting(0)
	r.message("any news?")
	r.waiting(2) // reminded, then waiting again
	close(gate)
	r.wait(done)
	results := callResults(r.store, r.runID)
	if len(results) != 3 || !strings.HasPrefix(results[0], "Not concluded yet: your conclusion (v1, candidate 1; round 1) is still with") ||
		!strings.HasPrefix(results[1], reminderCallPrefix) || results[2] != verify.Verified(r.verifier(), cand(1, 1)) {
		t.Errorf("conclude call results = %q, want the interrupted wait, then the reminder", results)
	}
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 2 {
		t.Errorf("child_result = %+v, want the candidate, verified, noting the job", cr)
	}
}

// A pass marks one candidate verified; it doesn't switch verification off: a
// new result after it goes back to the verifier.
func TestVerify_NewResultAfterPassGoesBack(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &gatedLLM{MockProvider: script("PASS: 42 m", "PASS: 42 m, rounded"), gate: gate}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("work", `{}`), concludeTurn("It is about 42 m."))
	go func() { // the second verdict, once the new candidate is with the verifier
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if vs := r.verifiers(); len(vs) == 1 && len(r.messages(vs[0])) >= 1 { // >=: it may be sent twice
				break
			}
		}
		gate <- struct{}{}
	}()
	r.wait(r.interrupted(aLLM, setBy("boss", "", 3), r.untilVerdict(gate)))
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "verifying", "passed"}) {
		t.Errorf("history = %v, want the new result verified too", got)
	}
	if cr := r.result(); cr.Content != "It is about 42 m." {
		t.Errorf("child_result = %q, want the new result, after its own pass", cr.Content)
	}
}

// The note about a verdict that arrived outside a conclude call is written
// once, even if the turn it precedes is resumed.
func TestVerify_NoteVerdictOnce(t *testing.T) {
	r := newVerifyRig(t, script())
	r.strandVerifier()
	ctx := context.Background()
	if err := r.store.AppendEventAtStep(ctx, r.runID, "dusky-gecko", 2, &event.AssistantEvent{Content: "PASS: right"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AppendEvent(ctx, r.runID, "worker", &event.ChildResultEvent{ChildSessionID: "dusky-gecko",
		Verdict: db.SessionConcluded, Content: "PASS: right"}); err != nil {
		t.Fatal(err)
	}
	ag := r.workerAgent()
	for range 2 {
		if err := ag.noteVerdict(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	if msgs := r.messages("worker"); len(msgs) != 1 || msgs[0] != harness(verify.Passed("dusky-gecko", cand(1, 1))) {
		t.Errorf("verify messages = %q, want the pass, once", msgs)
	}
}

// A wait resumed after a restart wakes on anything, a Notice included (here
// the verifier's cancellation), as the wait before the restart would have:
// the resumed session doesn't sit waiting for operator input.
func TestVerify_RestartedWaitWakesOnAStop(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS", "PASS"), gate: gate})
	r.stopWhileWaiting()
	// As if the process stopped before the call parked, so that awaiting below
	// is the resumed call's own.
	if err := r.store.UpdateSessionStatus(context.Background(), r.runID, "worker", db.SessionOngoing); err != nil {
		t.Fatal(err)
	}
	aLLM := turns(concludeTurn(""))
	done := r.start(aLLM, nil)
	r.waiting(0)
	v := r.verifier()
	if err := session.CancelSession(r.store, r.registry, r.runID, v, "operator stopped it"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resumed call answered", func() bool { return len(callResults(r.store, r.runID)) == 1 })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	if got := callResults(r.store, r.runID); got[0] != verify.Stopped(v, verify.Label{Version: 1, Candidate: 1}, "cancelled: operator stopped it") {
		t.Errorf("result of the resumed call = %q", got[0])
	}
	if cr := r.result(); cr.Content != "It is 42." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v", cr)
	}
	r.noCrashMarker()
}

// A verifier's result the checks already acted on (here a stop that landed
// while the conclude() call was generated, after which the call re-engages
// the verifier) doesn't end the wait that follows; the pass then concludes.
func TestVerify_ActedOnResultDoesntEndTheWait(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS", "PASS"), gate: gate})
	var v string
	aLLM := &exitingVerifierLLM{inner: turns(concludeTurn("It is 42 m."), concludeTurn("")), onSecond: func() {
		if err := session.CancelSession(r.store, r.registry, r.runID, v, "operator stopped it"); err != nil {
			t.Error(err)
		}
	}}
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	v = r.verifier()
	r.message("any news?")
	r.waiting(1)
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	if got := callResults(r.store, r.runID); len(got) != 2 || got[1] != verify.Verified(v, cand(1, 1)) {
		t.Errorf("conclude call results = %q, want the interrupted wait, then the pass", got)
	}
}

// A rejection the call returned settles the candidate: conclude() after it
// has nothing to wait for (an error), and the round is on record at once.
func TestVerify_NothingPendingAfterARejection(t *testing.T) {
	r := newVerifyRig(t, script("FAIL: no unit", "PASS"))
	aLLM := turns(concludeTurn("It is 42."), concludeTurn(""), concludeTurn("It is 42 m."))
	r.wait(r.start(aLLM, setBy("boss", "", 3)))
	if got := callResults(r.store, r.runID); len(got) != 3 || got[1] != nothingPending {
		t.Errorf("conclude call results = %q, want conclude() after the rejection to be the nothing-pending error", got)
	}
	if got := r.actions(); !slices.Equal(got, []string{"set", "verifying", "rejected", "verifying", "passed"}) {
		t.Errorf("history = %v", got)
	}
}

// A session revived as awaiting with its conclude call open (a process stop
// in the wait) resumes the call at once, rather than waiting for operator
// input first: here the verifier's crash, a Notice, is what the call settles.
func TestVerify_RestartedAwaitingSessionResumesTheCall(t *testing.T) {
	gate := make(chan struct{}, 3)
	refuse := llm.Response{StopReason: "refusal", Refusal: &llm.Refusal{Category: "test"}}
	r := newVerifyRig(t, &gatedLLM{MockProvider: &llm.MockProvider{Model: "m", Responses: []llm.Response{refuse,
		{Content: "PASS", StopReason: "end_turn"}, {Content: "PASS", StopReason: "end_turn"}}}, gate: gate})
	r.stopWhileWaiting()
	done := r.start(turns(concludeTurn("")), nil)
	for range 3 {
		gate <- struct{}{}
	}
	r.wait(done)
	if cr := r.result(); cr.Content != "It is 42." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the candidate, verified after the verifier's crash", cr)
	}
	r.noCrashMarker()
}

// A session cancelled while its conclude call waited (the cancel stops its
// verifier too), then revived by a message: the open call is answered first,
// as withdrawn by the cancel, and the cancelled verifier is left alone until
// the agent concludes again.
func TestVerify_CancelledMidWaitThenRevived(t *testing.T) {
	gate := make(chan struct{}, 2)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS", "PASS"), gate: gate})
	done := r.start(turns(concludeTurn("It is 42 m.")), setBy("boss", "", 3))
	r.waiting(0)
	v := r.verifier()
	if err := session.CancelSession(r.store, r.registry, r.runID, "worker", "not needed"); err != nil {
		t.Fatal(err)
	}
	r.wait(done)
	if _, err := r.store.AppendEvent(context.Background(), r.runID, "worker", &event.UserEvent{Content: "Please go on."}); err != nil {
		t.Fatal(err)
	}
	var atTurn []string
	var verifierAtTurn string
	aLLM := &hookLLM{MockProvider: turns(concludeTurn("It is 42 m.")), hook: func() {
		atTurn, verifierAtTurn = callResults(r.store, r.runID), r.status(v)
	}}
	done = r.start(aLLM, nil)
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)
	if len(atTurn) != 1 || atTurn[0] != cancelledCall("not needed", true, false) {
		t.Errorf("results before the revived turn = %q, want the open call withdrawn by the cancel", atTurn)
	}
	if verifierAtTurn != db.SessionCancelled {
		t.Errorf("verifier was %s at the revived turn, want still cancelled", verifierAtTurn)
	}
	assertPaired(t, aLLM.Recorded()[0])
	if cr := r.result(); cr.Content != "It is 42 m." || len(cr.Notes) != 1 || cr.Notes[0].Kind != event.NoteVerified {
		t.Errorf("child_result = %+v, want the candidate verified after the agent concluded again", cr)
	}
}

// A pass that lands while the agent's restarted process was down is
// delivered by the resumed call at once, without a model call: the verifier's
// result is no input the call would conclude over.
func TestVerify_RestartDeliversAPassThatLanded(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: right"), gate: gate})
	r.stopWhileWaiting()
	gate <- struct{}{}
	v := r.verifier()
	waitFor(t, "the verdict, while the worker is down", func() bool { return r.status(v) == db.SessionConcluded })
	aLLM := turns()
	r.wait(r.start(aLLM, nil))
	if aLLM.CallCount() != 0 {
		t.Errorf("agent calls after the restart = %d, want 0", aLLM.CallCount())
	}
	if got := callResults(r.store, r.runID); len(got) != 1 || got[0] != verify.Verified(v, cand(1, 1)) {
		t.Errorf("conclude call results = %q, want the pass", got)
	}
}

// A message that lands with the pass, in the same wake, still holds the
// conclusion: the call returns saying the verifier passed it and what came,
// and conclude() then delivers it.
func TestVerify_PassWithAMessageInTheSameWake(t *testing.T) {
	gate := make(chan struct{}, 1)
	r := newVerifyRig(t, &gatedLLM{MockProvider: script("PASS: right"), gate: gate})
	var once sync.Once
	r.store.SetCommitListener(func(runID, sid string, evt event.Event) {
		if ve, ok := evt.(*event.VerifyEvent); ok && sid == "worker" && ve.Outcome != nil && ve.Outcome.Verdict == event.VerdictPass {
			once.Do(func() { r.message("one more thing") }) // committed before the worker can read the verdict
		}
		r.notify(runID, sid, evt)
	})
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn(""))
	done := r.start(aLLM, setBy("boss", "", 3))
	r.waiting(0)
	gate <- struct{}{}
	r.wait(done)
	v := r.verifier()
	want := "Not concluded yet: this arrived meanwhile: a message from boss. " + verify.Passed(v, cand(1, 1))
	if got := callResults(r.store, r.runID); len(got) != 2 || got[0] != want || got[1] != verify.Verified(v, cand(1, 1)) {
		t.Errorf("conclude call results = %q, want the held pass, then the delivery", got)
	}
}

// A verdict that lands after the version changed, before the agent gave the
// new version a candidate, is on a withdrawn candidate: the agent's next read
// of its verifier (the note at its next turn, a settle) records it as stale,
// once, and tells it nothing.
func TestVerify_LateVerdictRecordedStale(t *testing.T) {
	r := newVerifyRig(t, script())
	ctx := context.Background()
	for _, s := range []db.SessionRecord{
		{SessionID: "worker", ParentID: "boss", AgentType: "standard_agent", Status: db.SessionOngoing},
		{SessionID: "late-lynx", ParentID: "worker", AgentType: testVerifierType, Status: db.SessionConcluded,
			Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
	} {
		s.RunID = r.runID
		if err := r.store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	lynx, _ := r.store.GetSession(ctx, r.runID, "late-lynx")
	for step, evs := range [][]event.Event{
		{setBy("boss", "", 3), &event.AssistantEvent{Content: "It is 42."},
			&event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness, Verifier: "late-lynx", Round: 1}},
		{&event.VerifyEvent{Action: event.VerifyReplace, By: event.ByOperator, MaxRounds: 3},
			&event.VerifyEvent{Action: event.VerifyWithdrawn, By: event.ByHarness, Verifier: "late-lynx", Round: 1},
			event.VerifierOutcome("late-lynx", "FAIL: no unit", "", lynx.StatusChangedAt)},
	} {
		if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if err := r.store.AppendEventAtStep(ctx, r.runID, "worker", step+1, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	ag := r.workerAgent()
	for range 2 {
		if err := ag.noteVerdict(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	if st := r.state(); len(st.Late) != 0 || !st.History[4].Stale {
		t.Errorf("after the note: late %v, the verdict stale %v; want it recorded by the note", st.Late, st.History[4].Stale)
	}
	out, err := ag.settle(ctx, nil)
	if err != nil || out.text != verify.Withdrawn(verify.Label{Version: 1, Candidate: 1}, 2) {
		t.Errorf("settle = %+v, %v", out, err)
	}
	st := r.state()
	var stale []verify.Entry
	for _, e := range st.History {
		if e.Action == event.VerifyStale {
			stale = append(stale, e)
		}
	}
	if len(stale) != 1 || stale[0].Reason != "for v1, candidate 1, which v2 superseded" || len(st.Late) != 0 ||
		withoutAt(stale[0].Outcome) != (event.VerifyOutcome{Verifier: "late-lynx", Verdict: event.VerdictFail, Findings: "no unit"}) {
		t.Errorf("stale records = %+v, late = %v; want the one verdict, once", stale, st.Late)
	}
	if !st.History[4].Stale || st.History[4].Action != event.VerifyVerdict {
		t.Errorf("the verdict isn't marked stale: %+v", st.History[4])
	}
	// Told once, as information: no round counted.
	want := harness(verify.StaleNote("late-lynx", false, "no unit", verify.Label{Version: 1, Candidate: 1}, 2))
	if msgs := r.messages("worker"); !slices.Equal(msgs, []string{want}) {
		t.Errorf("the agent was told %q, want %q once", msgs, want)
	}
	if st.Rejections != 0 {
		t.Errorf("rejections = %d, want none", st.Rejections)
	}
}

// A restarted conclude call is superseded by a set, replace or clear after it
// only if its candidate didn't go to the verifier after that change: a change
// that landed before the attempt took the verification lock made the
// candidate the new version's, and the resumed call waits on for it.
func TestSupersededSince(t *testing.T) {
	call := concludeTurn("It is 42.").ToolCalls[0]
	turn := &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}}}
	replace := &event.VerifyEvent{Action: event.VerifyReplace, By: event.ByOperator, MaxRounds: 3}
	verifying := &event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness, Verifier: "v", Round: 1}
	for _, c := range []struct {
		name       string
		evs        []event.Event
		superseded bool
	}{
		{"a replace after the call", []event.Event{setBy("boss", "", 3), turn, verifying, replace}, true},
		{"the candidate went to the new version", []event.Event{setBy("boss", "", 3), turn, replace, verifying}, false},
		{"a clear after the call", []event.Event{setBy("boss", "", 3), turn, verifying,
			&event.VerifyEvent{Action: event.VerifyClear, By: "boss"}}, true},
		{"no change", []event.Event{setBy("boss", "", 3), turn, verifying}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newVerifyRig(t, script())
			ctx := context.Background()
			if err := r.store.CreateSession(ctx, db.SessionRecord{RunID: r.runID, SessionID: "worker", ParentID: "boss",
				AgentType: "standard_agent", Status: db.SessionAwaiting}); err != nil {
				t.Fatal(err)
			}
			for _, e := range c.evs {
				if _, err := r.store.AppendEvent(ctx, r.runID, "worker", e); err != nil {
					t.Fatal(err)
				}
			}
			text, superseded, err := r.workerAgent().supersededSince(ctx, call.ID)
			if err != nil || superseded != c.superseded {
				t.Fatalf("superseded = %v (%q), %v; want %v", superseded, text, err, c.superseded)
			}
			if superseded && !strings.HasPrefix(text, "Not concluded: ") {
				t.Errorf("answer = %q", text)
			}
		})
	}
}

// A new version, or a clear, between deciding a pass and concluding withdraws
// the candidate: the pass, of a version that is over, neither delivers it nor
// leaves the agent told to deliver it.
func TestVerify_VersionChangeBeforeTheConclusion(t *testing.T) {
	for _, cleared := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleared=%v", cleared), func(t *testing.T) {
			r := newVerifyRig(t, script())
			ctx := context.Background()
			for _, s := range []db.SessionRecord{
				{SessionID: "worker", ParentID: "boss", AgentType: "standard_agent", Status: db.SessionOngoing},
				{SessionID: "lynx", ParentID: "worker", AgentType: testVerifierType, Status: db.SessionOngoing,
					Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
			} {
				s.RunID = r.runID
				if err := r.store.CreateSession(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.store.AdvanceStep(ctx, r.runID, "worker"); err != nil {
				t.Fatal(err)
			}
			for _, e := range []event.Event{setBy("boss", "", 3), &event.AssistantEvent{Content: "It is 42."},
				&event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness, Verifier: "lynx", Round: 1}} {
				if _, err := r.store.AppendEvent(ctx, r.runID, "worker", e); err != nil {
					t.Fatal(err)
				}
			}
			// A hand-made verifier: no real one may answer for it, revived by
			// this message.
			r.noRespawn.Store("lynx", true)
			if _, err := r.store.AppendEvent(ctx, r.runID, "lynx", &event.UserEvent{Content: "verify: It is 42."}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.store.AdvanceStep(ctx, r.runID, "lynx"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.store.AppendEvent(ctx, r.runID, "lynx", &event.AssistantEvent{Content: "PASS: fine"}); err != nil {
				t.Fatal(err)
			}
			_ = r.store.UpdateSessionStatus(ctx, r.runID, "lynx", db.SessionConcluded)

			ag := r.workerAgent()
			out, err := ag.settle(ctx, nil)
			if err != nil || out.kind != concludes {
				t.Fatalf("settle = %+v, %v; want the pass", out, err)
			}
			if cleared {
				if _, err := verify.Clear(ctx, r.store, r.registry, r.runID, "worker", "boss"); err != nil {
					t.Fatal(err)
				}
			} else {
				r.set(setBy(event.ByOperator, "cites a source", 3))
			}
			out, concluded, err := ag.finish(ctx, nil, out, attempt{}, 2, map[string]bool{})
			want := verify.Withdrawn(verify.Label{Version: 1}, 2)
			if cleared {
				want = verify.Withdrawn(verify.Label{Version: 1}, 0)
			}
			if err != nil || concluded || out.kind != holds || out.text != want || out.told != nil {
				t.Errorf("finish = %+v, concluded %v, %v; want the withdrawal, %q", out, concluded, err, want)
			}
			if s := r.status("worker"); s == db.SessionConcluded {
				t.Error("the superseded candidate was delivered")
			}
			if got := r.actions(); slices.Contains(got, "passed") {
				t.Errorf("history = %v: a pass recorded for a version that is over", got)
			}
		})
	}
}

// A version on another model retires the verifier: it is cancelled, not told
// to stand by, and the withdrawal still names it. Its stop is neither told nor
// counted; the next conclusion starts a new verifier, on v2's request.
func TestVerify_ModelChangeStartsANewVerifier(t *testing.T) {
	gate := make(chan struct{}, 1)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(concludeTurn("It is 42 m."), concludeTurn("It is 42 m (source: the survey)."))
	ag := r.worker(aLLM, setBy("boss", "", 3))
	ag.env.ResolveLLM = func(_ context.Context, m string) (string, llm.Provider, error) { return m, nil, nil }
	done := r.startAgent(ag)
	r.waiting(0)
	old := r.verifier()
	r.firstCallInFlight(vLLM)

	e := setBy(event.ByOperator, "the answer cites a source", 3)
	e.Model = "other:model"
	r.set(e)
	waitFor(t, "a second verifier", func() bool { return len(r.verifiers()) == 2 })
	gate <- struct{}{}
	r.wait(done)

	vs := r.verifiers()
	if s := r.status(old); s != db.SessionCancelled || len(r.withdrawals(old)) != 0 {
		t.Errorf("old verifier %s: %s, withdrawals %q; want cancelled, not told", old, s, r.withdrawals(old))
	}
	for _, h := range r.steps() {
		if h.Action == event.VerifyWithdrawn && h.Verifier != old {
			t.Errorf("the withdrawn record names %q, want %s", h.Verifier, old)
		}
	}
	task, _ := r.store.GetSession(context.Background(), r.runID, vs[1])
	if !strings.Contains(task.Task, "Request (v2)") || !strings.Contains(task.Task, "Candidate conclusion (v2, candidate 1)") {
		t.Errorf("the new verifier's task:\n%s", task.Task)
	}
	for _, o := range append(r.messageOutcomes(), r.callOutcomes()...) {
		if o != nil && o.Verifier == old {
			t.Errorf("the old verifier's outcome was told: %+v", o)
		}
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." ||
		!slices.Contains(cr.Notes, event.ResultNote{Kind: event.NoteVerified, Text: vs[1] + " (round 1)"}) {
		t.Errorf("child_result = %+v, want v2's candidate verified by the new verifier", cr)
	}
}

// After a clear, a set reuses the kept verifier: it stood by after the
// withdrawal, and v2's first candidate revives it with v2's request.
func TestVerify_ClearThenSetReusesTheVerifier(t *testing.T) {
	gate := make(chan struct{}, 3)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("FAIL: no source", "Standing by.", "PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	reset := &tool.Tool{Name: "work", Description: "work", ParamType: &struct{}{},
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			gate <- struct{}{}
			gate <- struct{}{}
			// Concluded and gone: a candidate that lands while its goroutine is
			// still leaving only bumps it, and is sent again.
			waitFor(t, "the verifier standing by", func() bool {
				v := r.verifier()
				return r.status(v) == db.SessionConcluded && !r.registry.IsRegistered(v)
			})
			r.set(setBy(event.ByOperator, "the answer cites a source", 3))
			gate <- struct{}{}
			return &tool.Result{Content: "worked"}, nil
		}}
	aLLM := turns(concludeTurn("It is 42 m."), toolTurn("work", `{}`), concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, setBy("boss", "", 3), reset)
	r.waiting(0)
	v := r.verifier()
	r.firstCallInFlight(vLLM)
	if _, err := verify.Clear(context.Background(), r.store, r.registry, r.runID, "worker", "boss"); err != nil {
		t.Fatal(err)
	}
	r.wait(done)

	if vs := r.verifiers(); len(vs) != 1 {
		t.Errorf("verifiers = %v, want the one, kept", vs)
	}
	if w := r.withdrawals(v); len(w) != 1 || w[0] != verify.Withdrawal("worker", verify.Label{Version: 1, Candidate: 1}, 0) {
		t.Errorf("withdrawals = %q", w)
	}
	if msgs := r.messages(v); len(msgs) != 1 || !strings.Contains(msgs[0], "has a new version: v2") ||
		!strings.Contains(msgs[0], "Candidate conclusion (v2, candidate 1)") {
		t.Errorf("the verifier's candidate messages = %q", msgs)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." || len(cr.Notes) != 1 {
		t.Errorf("child_result = %+v, want v2's candidate, verified", cr)
	}
}

// A reply the harness waits on (its candidate) is withdrawn by a replace like
// a call: the harness's answer says so, and the agent's next conclusion is
// v2's first candidate.
func TestVerify_ReplyWaitWithdrawnByAReplace(t *testing.T) {
	gate := make(chan struct{}, 2)
	vLLM := &enteredLLM{gatedLLM: &gatedLLM{MockProvider: script("FAIL: no source", "PASS: cites one"), gate: gate}}
	r := newVerifyRig(t, vLLM)
	aLLM := turns(reply("It is 42 m."), concludeTurn("It is 42 m (source: the survey)."))
	done := r.start(aLLM, setBy("boss", "", 3))
	waitFor(t, "the harness waiting on the reply", func() bool { return r.status("worker") == db.SessionAwaiting })
	v := r.verifier()
	r.firstCallInFlight(vLLM)
	r.set(setBy(event.ByOperator, "the answer cites a source", 3))
	waitFor(t, "v2's candidate with the verifier", func() bool { return len(r.messages(v)) == 1 })
	gate <- struct{}{}
	gate <- struct{}{}
	r.wait(done)

	want := harness(verify.Withdrawn(verify.Label{Version: 1, Candidate: 1}, 2))
	if msgs := r.messages("worker"); !slices.Contains(msgs, want) {
		t.Errorf("verify messages = %q, want %q", msgs, want)
	}
	if cr := r.result(); cr.Content != "It is 42 m (source: the survey)." {
		t.Errorf("child_result = %q, want v2's candidate", cr.Content)
	}
}

// compactedStream gives sid's stream events at steps 0 to n-1, then advances
// to step n and compacts through n-1: its context then holds none of them
// but step 0's.
func compactedStream(t *testing.T, r *verifyRig, sid, parent string, n int, evts map[int][]event.Event) func() []db.EventRecord {
	t.Helper()
	ctx := context.Background()
	if err := r.store.CreateSession(ctx, db.SessionRecord{RunID: r.runID, SessionID: sid, ParentID: parent,
		AgentType: "standard_agent", Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < n; step++ {
		for _, e := range evts[step] {
			if _, err := r.store.AppendEvent(ctx, r.runID, sid, e); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.store.AdvanceStep(ctx, r.runID, sid); err != nil {
			t.Fatal(err)
		}
	}
	callStep := n
	read := func() []db.EventRecord {
		evs, err := r.store.GetEvents(ctx, r.runID, sid, db.EventFilter{CurrentContextOnly: true, EndStep: &callStep})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	if _, err := r.store.CompactContext(ctx, r.runID, sid, n-1, "summary of the earlier turns"); err != nil {
		t.Fatal(err)
	}
	return read
}

// What the verifier is told the agent answers is its latest request, though a
// compaction folded the message it came in into a summary: what the agent was
// asked is read from its whole stream, not its context.
func TestVerify_RequestSurvivesCompaction(t *testing.T) {
	r := newVerifyRig(t, script())
	read := compactedStream(t, r, "worker", "boss", 3, map[int][]event.Event{
		0: {&event.UserEvent{Content: "How tall is it?"}},
		2: {&event.UserEvent{Content: "Now give it in feet, not metres."}},
	})
	ag := r.workerAgent()
	hist, err := ag.requestHistory(context.Background(), read())
	if err != nil {
		t.Fatal(err)
	}
	if got := ag.currentRequest(hist); !strings.Contains(got, "Now give it in feet, not metres.") {
		t.Errorf("after compaction the request is %q; want the operator's follow-up", got)
	}
}

// A kept verifier whose context was compacted still knows its latest
// candidate, and that it was withdrawn.
func TestVerify_VerifierCandidateSurvivesCompaction(t *testing.T) {
	r := newVerifyRig(t, script())
	read := compactedStream(t, r, "v", "worker", 4, map[int][]event.Event{
		0: {&event.UserEvent{Content: "candidate 1"}},
		1: {&event.UserEvent{Content: "candidate 2", Check: event.CheckVerify}},
		3: {&event.UserEvent{Content: "candidate 2 is withdrawn", Check: event.CheckWithdrawn}},
	})
	ag := newT(testCfg{RunID: r.runID, SessionID: "v", ParentID: "worker", Store: r.store})
	hist, err := ag.requestHistory(context.Background(), read())
	if err != nil {
		t.Fatal(err)
	}
	if got := latestCandidate(hist); got != "candidate 2" {
		t.Errorf("latest candidate %q; want candidate 2", got)
	}
	if !standsBy(hist) {
		t.Error("not standing by after its candidate was withdrawn")
	}
}

// A new version that comes with a message, landing after a conclude call's
// first look for input but before its checks take the verification lock,
// overtakes the call: its result, which never read the message, isn't given
// to a verifier as the new version's candidate. The next conclusion is, with
// the new request. (The process scan of the leftovers check runs in that gap,
// which is where the message lands here.)
func TestVerify_VersionWithMessageDuringTheChecksOvertakes(t *testing.T) {
	r := newVerifyRig(t, script("PASS: 138 ft is right"))
	var once sync.Once
	orig := scanProcs
	scanProcs = func(string) proctree.Snapshot {
		once.Do(func() {
			if _, err := verify.SetThen(context.Background(), r.store, r.registry, r.runID, "worker",
				setBy("boss", "", 3), func() { r.message("Now give it in feet.") }); err != nil {
				t.Error(err)
			}
		})
		return proctree.Snapshot{Supported: true}
	}
	t.Cleanup(func() { scanProcs = orig })
	done := r.start(turns(concludeTurn("It is 42 m."), concludeTurn("It is 138 ft.")), setBy("boss", "", 3))
	r.wait(done)

	results := callResults(r.store, r.runID)
	if len(results) != 2 || !strings.HasPrefix(results[0],
		"Not concluded yet: this arrived meanwhile: a message from boss. Nothing went to the verifier") {
		t.Errorf("conclude call results = %q; want the first overtaken by the message", results)
	}
	for _, s := range r.steps() {
		if s.Action == event.VerifyVerifying && s.Candidate == "It is 42 m." {
			t.Errorf("the overtaken result went to the verifier: %+v", s)
		}
	}
	events, err := r.store.GetEvents(context.Background(), r.runID, r.verifier(), db.EventFilter{})
	if err != nil || len(events) == 0 {
		t.Fatalf("verifier stream: %d events, %v", len(events), err)
	}
	if task := lastUserContentOf(events); !strings.Contains(task, "Now give it in feet.") {
		t.Errorf("the verifier's task doesn't name the new request:\n%s", task)
	}
	if cr := r.result(); cr.Content != "It is 138 ft." {
		t.Errorf("child_result = %+v; want the answer to the new request", cr)
	}
}

// lastUserContentOf is the content of the latest user message in events.
func lastUserContentOf(events []db.EventRecord) string {
	for i := len(events) - 1; i >= 0; i-- {
		if u, ok := events[i].Event.(*event.UserEvent); ok {
			return u.Content
		}
	}
	return ""
}
