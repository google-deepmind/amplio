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

package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/session"
)

type stubAgent struct{}

func (stubAgent) Run(context.Context) error { return nil }
func (stubAgent) SessionID() string         { return "" }

func init() {
	agent.Register("verify_test_agent", func(*agent.Env, *agent.Config) (agent.Agent, error) { return stubAgent{}, nil }, agent.Traits{})
	agent.Register("verify_test_chat", func(*agent.Env, *agent.Config) (agent.Agent, error) { return stubAgent{}, nil },
		agent.Traits{Interactive: true})
	agent.Register("verify_test_verifier", func(*agent.Env, *agent.Config) (agent.Agent, error) { return stubAgent{}, nil },
		agent.Traits{Verifier: true})
}

func newStore(t *testing.T) (db.Store, string) {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := db.NewRunID()
	if err := store.CreateRun(context.Background(), db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	return store, runID
}

func addSession(t *testing.T, store db.Store, runID, sid, parent, agentType, status string, meta map[string]any) {
	t.Helper()
	if err := store.CreateSession(context.Background(), db.SessionRecord{RunID: runID, SessionID: sid, ParentID: parent,
		AgentType: agentType, Status: status, Metadata: meta}); err != nil {
		t.Fatal(err)
	}
}

func appendAll(t *testing.T, store db.Store, runID, sid string, evs ...event.Event) {
	t.Helper()
	for _, e := range evs {
		if _, err := store.AppendEvent(context.Background(), runID, sid, e); err != nil {
			t.Fatal(err)
		}
	}
}

func recs(evs ...event.Event) []db.EventRecord {
	out := make([]db.EventRecord, len(evs))
	for i, e := range evs {
		out[i] = db.EventRecord{Event: e}
	}
	return out
}

func ve(action string, verifier string, round int) *event.VerifyEvent {
	return &event.VerifyEvent{Action: action, Verifier: verifier, Round: round, MaxRounds: 3, By: event.ByHarness}
}

func final(text string) *event.AssistantEvent { return &event.AssistantEvent{Content: text} }

// The state follows the stream: rounds are the recorded rejections, a
// candidate is the final turn before it went to the verifier, and set or
// replace starts a new version afresh (keeping the verifier) while clear,
// passed and exhausted end it.
func TestDerive(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss", Criteria: "tests pass", MaxRounds: 2}
	st := Derive(recs(set, final("v1"), ve("verifying", "v", 1), ve("rejected", "v", 1), final("v2"), ve("verifying", "v", 2)))
	if !st.On || st.Verifier != "v" || st.Rejections != 1 || !st.Pending || st.Candidate != "v2" || st.Status() != StatusVerifying ||
		st.Round() != 2 || st.Exhausted() {
		t.Errorf("mid-verification state = %+v", st)
	}
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), ve("rejected", "v", 1), final("v2"),
		ve("verifying", "v", 2), ve("rejected", "v", 2)))
	if !st.Exhausted() || st.Pending || st.Status() != StatusOn {
		t.Errorf("after two rejections of max 2: %+v", st)
	}
	// A rejection counts a round of a candidate with the verifier only: one
	// written after a replace that raced it belongs to the request before.
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), &event.VerifyEvent{Action: event.VerifyReplace,
		By: "operator", MaxRounds: 3}, ve("rejected", "v", 1)))
	if st.Rejections != 0 || st.Round() != 1 {
		t.Errorf("a rejection after the replace counted: rejections %d, round %d", st.Rejections, st.Round())
	}
	replaced := Derive(recs(set, final("v1"), ve("verifying", "v", 1), ve("rejected", "v", 1),
		&event.VerifyEvent{Action: event.VerifyReplace, By: "operator", MaxRounds: 3}))
	if !replaced.On || replaced.Verifier != "v" || replaced.Rejections != 0 || replaced.Request.By != "operator" ||
		len(replaced.History) != 4 || replaced.Version != 2 || replaced.Candidates != 0 || replaced.Pending {
		t.Errorf("replace didn't start a new version afresh, with the same verifier: %+v", replaced)
	}
	for action, status := range map[string]string{
		event.VerifyClear: StatusOff, event.VerifyPassed: StatusPassed, event.VerifyExhausted: StatusNotVerified,
	} {
		st := Derive(recs(set, final("v1"), ve("verifying", "v", 1), ve(action, "v", 1)))
		if st.On || st.Pending || st.Status() != status {
			t.Errorf("after %s: on=%v pending=%v status=%s, want off and %s", action, st.On, st.Pending, st.Status(), status)
		}
	}
	if st := Derive(recs(final("hi"))); st.On || st.Status() != StatusOff || st.Round() != 0 {
		t.Errorf("never set: %+v", st)
	}

	// A new verifier's stop can land before the verifying event that names it;
	// stops of another verifier don't count, and a rejection resets them.
	crash := func(child string) *event.ChildResultEvent {
		return &event.ChildResultEvent{ChildSessionID: child, Verdict: db.SessionCrashed, Content: "provider\nrefused"}
	}
	st = Derive(recs(set, final("v1"), crash("v"), ve("verifying", "v", 1), crash("old"), crash("v"), crash("v")))
	if st.Stops != 3 || !st.TooManyStops() || st.LastStop != "crashed: provider refused" {
		t.Errorf("stops = %d (%q), want 3, the first before its verifying event", st.Stops, st.LastStop)
	}
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), crash("v"), crash("v"), ve("rejected", "v", 1), crash("v")))
	if st.Stops != 1 {
		t.Errorf("stops after a rejection = %d, want 1", st.Stops)
	}

	// Verifiers post their outcome as a record of their own; a stop counts the
	// same way, and the record is history but no step of the verification.
	stop := func(child string) *event.VerifyEvent {
		return event.VerifierOutcome(child, "provider\nrefused", db.SessionCrashed, time.Time{})
	}
	st = Derive(recs(set, final("v1"), stop("v"), ve("verifying", "v", 1), stop("old"), crash("v"), stop("v")))
	if st.Stops != 3 || st.LastStop != "crashed: provider refused" || st.Last.Action != event.VerifyVerifying ||
		len(st.History) != 5 || !st.Pending {
		t.Errorf("stops from outcome records = %d (%q), last %s, history %d; want 3, the record no step",
			st.Stops, st.LastStop, st.Last.Action, len(st.History))
	}
	// A verdict ends a run of stops, counted or not (a rejection of a
	// superseded candidate isn't a round).
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), stop("v"), stop("v"),
		event.VerifierOutcome("v", "FAIL: no unit", "", time.Time{}), ve("verifying", "v", 1), stop("v")))
	if st.Stops != 1 || st.Rejections != 0 {
		t.Errorf("stops after a verdict = %d (rejections %d), want 1 (0)", st.Stops, st.Rejections)
	}
	verdict := event.VerifierOutcome("v", "PASS", "", time.Time{})
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), verdict))
	if st.Stops != 0 || !st.Pending || st.Status() != StatusVerifying {
		t.Errorf("a verdict's record changed the state: %+v", st)
	}
	st = Derive(recs(set, final("v1"), ve("verifying", "v", 1), ve(event.VerifyPassed, "v", 1), verdict))
	if st.Status() != StatusPassed || st.Round() != 1 {
		t.Errorf("a late record changed how it ended: %s round %d", st.Status(), st.Round())
	}
}

// concluding is a turn calling conclude ("" for no result).
func concluding(result string) *event.AssistantEvent {
	args := "{}"
	if result != "" {
		args = `{"result":"` + result + `"}`
	}
	return &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "c-" + result, Name: event.ConcludeTool, Arguments: args}}}
}

// A candidate is what the attempt before its verifying event offers: a
// conclude call's result, or a reply's text. conclude() without a result
// gives the pending candidate again, and turns between (a reply the harness
// nudged instead of taking) don't change it.
func TestDerive_CandidateFromConcludeCalls(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss"}
	nudged := &event.UserEvent{Content: "[system] call conclude", Check: event.CheckConclude}
	for _, c := range []struct {
		name string
		evs  []event.Event
		want string
	}{
		{"conclude(result)", []event.Event{set, concluding("42 m"), ve("verifying", "v", 1)}, "42 m"},
		{"conclude() again after a stop", []event.Event{set, concluding("42 m"), ve("verifying", "v", 1),
			final("still waiting"), nudged, concluding(""), ve("verifying", "v", 1)}, "42 m"},
		{"a new result", []event.Event{set, concluding("42"), ve("verifying", "v", 1), ve("rejected", "v", 1),
			concluding("42 m"), ve("verifying", "v", 2)}, "42 m"},
		{"a reply", []event.Event{set, final("42 m"), ve("verifying", "v", 1)}, "42 m"},
	} {
		if st := Derive(recs(c.evs...)); st.Candidate != c.want || !st.Pending {
			t.Errorf("%s: candidate %q (pending %v), want %q", c.name, st.Candidate, st.Pending, c.want)
		}
	}
}

// The candidate is the one the verifying record carries: the harness gives
// the pending candidate again on conclude(), or on a reply it takes as one,
// and a pass then delivers that, not the reply before the record. A record
// without a candidate falls back to what the attempt before it offers.
func TestDerive_CandidateFromTheRecord(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss"}
	giving := func(candidate string) *event.VerifyEvent {
		e := ve("verifying", "v", 1)
		e.Candidate = candidate
		return e
	}
	crash := &event.ChildResultEvent{ChildSessionID: "v", Verdict: db.SessionCrashed, Content: "refused"}
	nudged := &event.UserEvent{Content: "[system] call conclude", Check: event.CheckConclude}
	st := Derive(recs(set, final("reply 1"), giving("reply 1"), crash, final("reply 2"), nudged, final("reply 3"),
		nudged, final("Still waiting for the verifier."), giving("reply 1")))
	if st.Candidate != "reply 1" || !st.Pending {
		t.Errorf("candidate after a reply taken as conclude() = %q (pending %v), want the one the record gave", st.Candidate, st.Pending)
	}
	if st := Derive(recs(set, final("reply 1"), ve("verifying", "v", 1))); st.Candidate != "reply 1" {
		t.Errorf("candidate of a record without one = %q, want what the attempt before it offered", st.Candidate)
	}
}

// A message giving a verifier a candidate gives the same one as an earlier
// message (or its task) when label and text match, whole or truncated alike;
// another candidate, the same text under another label, or one that only
// starts the same, is a new one.
func TestSameCandidate(t *testing.T) {
	long := strings.Repeat("x", maxCandidateRunes+10)
	v1c1, v1c2, v2c1 := Label{1, 1}, Label{1, 2}, Label{2, 1}
	task := VerifierTask("worker", "How tall?", "", "It is 42 m.", v1c1, false)
	for _, c := range []struct {
		prev, msg string
		want      bool
	}{
		{task, Delivery("worker", 1, "It is 42 m.", v1c1, false), true},
		{VerifierTask("worker", "How tall?", "", "It is 42 m.", v1c1, true), Delivery("worker", 1, "It is 42 m.", v1c1, true), true},
		{Delivery("worker", 2, long, v1c2, true), Delivery("worker", 2, long, v1c2, true), true},
		{NewVersion("worker", "How tall?", "cites a source", "It is 42 m.", v2c1, true),
			NewVersion("worker", "How tall?", "cites a source", "It is 42 m.", v2c1, true), true},
		{task, Delivery("worker", 1, "It is 42 m (source: the survey).", v1c2, false), false},
		{task, Delivery("worker", 1, "It is 42", v1c1, false), false},
		{task, NewVersion("worker", "How tall?", "cites a source", "It is 42 m.", v2c1, false), false},
	} {
		if got := SameCandidate(c.prev, c.msg); got != c.want {
			t.Errorf("SameCandidate(%.50q, %.80q) = %v, want %v", c.prev, c.msg, got, c.want)
		}
	}
}

// A verifier is reading the latest candidate once a turn of its begins after
// the candidate arrived (its step moved past the candidate's), or, running,
// when its latest turn is a working one. Running with a final latest turn (it
// may be concluding with it) isn't reading, nor is having concluded before
// the candidate.
func TestInspect_Reading(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	candidate := &event.UserEvent{Content: "[system] verify candidate 2", Check: event.CheckVerify}
	work := &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "t1", Name: "bash", Arguments: "{}"}}}
	reading := func(sid string) bool {
		t.Helper()
		v, err := Inspect(ctx, store, runID, sid)
		if err != nil {
			t.Fatal(err)
		}
		return v.Reading
	}
	for sid, latest := range map[string]*event.AssistantEvent{"finishing": final("PASS: fine"), "working": work} {
		addSession(t, store, runID, sid, "worker", "verify_test_agent", db.SessionOngoing, nil)
		appendAll(t, store, runID, sid, &event.UserEvent{Content: "verify candidate 1"})
		if _, err := store.AdvanceStep(ctx, runID, sid); err != nil {
			t.Fatal(err)
		}
		if sid == "finishing" && !reading(sid) {
			t.Errorf("%s: running, with no turn yet: want it reading its task", sid)
		}
		appendAll(t, store, runID, sid, latest, candidate)
	}
	if reading("finishing") {
		t.Error("finishing: running, its latest turn final, the candidate unread: want not reading")
	}
	if !reading("working") {
		t.Error("working: running, its latest turn a working one: want reading at its next turn")
	}
	if _, err := store.AdvanceStep(ctx, runID, "finishing"); err != nil {
		t.Fatal(err)
	}
	if !reading("finishing") {
		t.Error("finishing: a turn began after the candidate: want reading")
	}
	if err := store.UpdateSessionStatus(ctx, runID, "working", db.SessionConcluded); err != nil {
		t.Fatal(err)
	}
	if v, _ := Inspect(ctx, store, runID, "working"); v.Reading || !v.Owed {
		t.Errorf("concluded before the candidate: %+v, want owed, not reading", v)
	}
}

// A verifier that concludes through conclude gives its verdict in the call's
// result.
func TestInspect_VerdictInAConcludeCall(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	addSession(t, store, runID, "v", "worker", "verify_test_agent", db.SessionConcluded, nil)
	appendAll(t, store, runID, "v", &event.UserEvent{Content: "verify candidate 1"})
	if _, err := store.AdvanceStep(ctx, runID, "v"); err != nil {
		t.Fatal(err)
	}
	appendAll(t, store, runID, "v", concluding("FAIL: no units"))
	if v, err := Inspect(ctx, store, runID, "v"); err != nil || !v.Given || v.Pass || v.Findings != "no units" {
		t.Errorf("Inspect = %+v, %v; want the call's FAIL", v, err)
	}
}

// The verifier agent type's verdict is typed: read as given, never parsed
// from text, even when its findings start like the other verdict. Its reply
// (the fallback after the nudges) is no verdict, a FAIL. Only a verifier of a
// type without the typed verdict (a standard agent) has its reply read by the
// first line.
func TestInspect_TypedVerdict(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	wavering := "PASS: Wait, let me reconsider. FAIL: primes.txt does not exist."
	for _, c := range []struct {
		name      string
		agentType string
		turn      *event.AssistantEvent
		pass      bool
		findings  string
	}{
		{"typed FAIL", "verify_test_verifier", &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "c",
			Name: event.ConcludeTool, Arguments: `{"verdict":"FAIL","findings":"` + wavering + `"}`}}}, false, wavering},
		{"typed pass, any case", "verify_test_verifier", &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "c",
			Name: event.ConcludeTool, Arguments: `{"verdict":"pass","findings":"FAIL would be wrong: 3/3 green"}`}}}, true,
			"FAIL would be wrong: 3/3 green"},
		{"typed FAIL over a stray result", "verify_test_verifier", &event.AssistantEvent{ToolCalls: []event.ToolCall{{ID: "c",
			Name: event.ConcludeTool, Arguments: `{"verdict":"FAIL","findings":"no file","result":"PASS: all good"}`}}}, false,
			"no file"},
		{"a typed verifier's reply", "verify_test_verifier", final(wavering), false, event.NoVerdict + ": " + wavering},
		{"a standard verifier's reply", "verify_test_agent", final(wavering), true,
			"Wait, let me reconsider. FAIL: primes.txt does not exist."},
	} {
		sid := strings.ReplaceAll(c.name, " ", "-")
		addSession(t, store, runID, sid, "worker", c.agentType, db.SessionConcluded, nil)
		appendAll(t, store, runID, sid, &event.UserEvent{Content: "verify candidate 1"})
		if _, err := store.AdvanceStep(ctx, runID, sid); err != nil {
			t.Fatal(err)
		}
		appendAll(t, store, runID, sid, c.turn)
		if v, err := Inspect(ctx, store, runID, sid); err != nil || !v.Given || v.Pass != c.pass || v.Findings != c.findings {
			t.Errorf("%s: Inspect = %+v, %v; want pass=%v, %q", c.name, v, err, c.pass, c.findings)
		}
	}
}

// A verdict counts for the latest candidate only if the verifier concluded
// after receiving it; a verifier that stopped first is reported as stopped.
func TestEvaluate(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	st := State{On: true, Pending: true, Verifier: "v", Request: &event.VerifyEvent{MaxRounds: 3}}
	addSession(t, store, runID, "v", "worker", "verify_test_agent", db.SessionOngoing, nil)
	appendAll(t, store, runID, "v", &event.UserEvent{Content: "verify candidate 1"}) // its task, at step 0
	if _, err := store.AdvanceStep(ctx, runID, "v"); err != nil {
		t.Fatal(err)
	}
	if v, _ := Evaluate(ctx, store, runID, st); v.Given || v.Stopped != "" {
		t.Errorf("verifier still working: %+v", v)
	}

	appendAll(t, store, runID, "v", final("FAIL: no units"))
	if err := store.UpdateSessionStatus(ctx, runID, "v", db.SessionConcluded); err != nil {
		t.Fatal(err)
	}
	if v, _ := Evaluate(ctx, store, runID, st); !v.Given || v.Pass || v.Findings != "no units" {
		t.Errorf("concluded after the candidate: %+v, want a FAIL given", v)
	}

	// A newer candidate reached it after that verdict: the verdict is stale.
	appendAll(t, store, runID, "v", &event.UserEvent{Content: "[system] candidate 2", Check: event.CheckVerify})
	if v, _ := Evaluate(ctx, store, runID, st); v.Given {
		t.Errorf("verdict for an older candidate given for the newer one: %+v", v)
	}
	// An operator message to the verifier is not a candidate.
	appendAll(t, store, runID, "v", final("PASS"), &event.UserEvent{Content: "are you sure?"})
	if v, _ := Evaluate(ctx, store, runID, st); !v.Given || !v.Pass {
		t.Errorf("verdict after the latest candidate: %+v, want a PASS given", v)
	}

	if err := store.UpdateSessionStatus(ctx, runID, "v", db.SessionCrashed); err != nil {
		t.Fatal(err)
	}
	if v, _ := Evaluate(ctx, store, runID, st); v.Given || v.Stopped != db.SessionCrashed {
		t.Errorf("crashed verifier: %+v", v)
	}
	if v, _ := Evaluate(ctx, store, runID, State{On: true, Pending: true, Verifier: "nobody"}); !v.Missing {
		t.Errorf("missing verifier: %+v", v)
	}
	if v, _ := Evaluate(ctx, store, runID, State{On: true, Verifier: "v"}); v != (Verdict{}) {
		t.Errorf("nothing pending, yet %+v", v)
	}
}

// IsEnded knows Ended's message, numbered or from before versions were, and
// nothing else.
func TestIsEnded(t *testing.T) {
	ended := Ended(12, RoundsReason(1, "no unit"))
	for msg, want := range map[string]bool{
		ended:               true,
		"[system] " + ended: true,
		"[system] Verification ended: not verified after 1 rejection (latest findings: no unit).": true,
		"Verification v ended: not verified":                                                      false,
		"Verification v2 passed":                                                                  false,
		"[system] Verification passed. Call …":                                                    false,
		"Your verification v1 ended: not verified":                                                false,
	} {
		if got := IsEnded(msg); got != want {
			t.Errorf("IsEnded(%q) = %v, want %v", msg, got, want)
		}
	}
}

// A request refuses a field it doesn't have, also inside another body (a
// message's, a tool call's), so a misspelt one isn't silently dropped.
func TestRequestRefusesUnknownFields(t *testing.T) {
	var r Request
	if err := json.Unmarshal([]byte(`{"criteria":"tests pass","model":"m","max_rounds":2}`), &r); err != nil ||
		r.Criteria != "tests pass" || r.Model != "m" || r.MaxRounds == nil || *r.MaxRounds != 2 {
		t.Errorf("Unmarshal = %+v, %v", r, err)
	}
	if err := json.Unmarshal([]byte(`{"criterion":"tests pass"}`), &r); err == nil || !strings.Contains(err.Error(), "criterion") {
		t.Errorf("unknown field: %v", err)
	}
	var body struct {
		Content string   `json:"content"`
		Verify  *Request `json:"verify"`
	}
	if err := json.Unmarshal([]byte(`{"content":"x","extra":1,"verify":{"max_round":1}}`), &body); err == nil ||
		!strings.Contains(err.Error(), "max_round") {
		t.Errorf("unknown field inside a body: %v", err)
	}
}

// Requests take the defaults for what they leave out, refuse a negative
// round limit, and resolve a model to its spec (or refuse it).
func TestRequestResolve(t *testing.T) {
	ctx := context.Background()
	d := config.VerifyDefaults{Model: "#judge", MaxRounds: 3}
	resolve := func(_ context.Context, m string) (string, llm.Provider, error) {
		if m == "#judge" || m == "vertex-claude:x" {
			return "vertex-claude:x", nil, nil
		}
		return "", nil, errors.New("unknown model " + m)
	}
	e, err := Request{}.Resolve(ctx, "boss", d, resolve)
	if err != nil || e.Action != event.VerifySet || e.By != "boss" || e.Model != "vertex-claude:x" || e.MaxRounds != 3 || e.Criteria != "" {
		t.Errorf("defaults: %+v, %v", e, err)
	}
	zero := 0
	e, err = Request{Criteria: " tests pass ", MaxRounds: &zero}.Resolve(ctx, "operator", config.VerifyDefaults{MaxRounds: 3}, nil)
	if err != nil || e.Criteria != "tests pass" || e.MaxRounds != 0 || e.Model != "" {
		t.Errorf("explicit 0 (unbounded) and no model: %+v, %v", e, err)
	}
	neg := -1
	if _, err := (Request{MaxRounds: &neg}).Resolve(ctx, "operator", d, resolve); err == nil {
		t.Error("negative max_rounds accepted")
	}
	if _, err := (Request{Model: "gpt-9"}).Resolve(ctx, "operator", d, resolve); err == nil || !strings.Contains(err.Error(), "unknown model gpt-9") {
		t.Errorf("bad model: %v", err)
	}
	if _, err := (Request{Model: "x"}).Resolve(ctx, "operator", d, nil); err == nil {
		t.Error("a model accepted with no resolver")
	}
}

// withdrawals are the harness's withdrawal messages in verifier v1's stream.
func withdrawals(t *testing.T, store db.Store, runID string) []string {
	t.Helper()
	events, _ := store.GetEvents(context.Background(), runID, "v1", db.EventFilter{})
	var out []string
	for _, e := range events {
		if u, ok := e.Event.(*event.UserEvent); ok && u.Check == event.CheckWithdrawn {
			out = append(out, u.Content)
		}
	}
	return out
}

// Every set or replace starts a new version, and returns its number. A
// replace while the verifier checks a candidate doesn't cancel it: the
// candidate is withdrawn (a record that wakes a waiting agent), and the
// verifier is told to stop and wait for the next one. A clear withdraws the
// candidate the same way and keeps the verifier; clearing what is off writes
// nothing. Interactive agents and verifiers can't be verified.
func TestSetAndClear(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	reg := session.NewRegistry()
	addSession(t, store, runID, "worker", "boss", "verify_test_agent", db.SessionAwaiting, nil)
	addSession(t, store, runID, "v1", "worker", "verify_test_agent", db.SessionOngoing, map[string]any{MetadataVerifierFor: "worker"})

	if n, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", MaxRounds: 3}); err != nil || n != 1 {
		t.Fatalf("Set = %d, %v; want v1", n, err)
	}
	appendAll(t, store, runID, "worker", final("v1"), ve(event.VerifyVerifying, "v1", 1))
	if n, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "operator", Criteria: "cite sources", MaxRounds: 3}); err != nil || n != 2 {
		t.Fatalf("Set = %d, %v; want v2", n, err)
	}
	st, _ := Load(ctx, store, runID, "worker")
	if len(st.History) != 4 || st.History[2].Action != event.VerifyReplace || st.History[3].Action != event.VerifyWithdrawn ||
		st.History[3].Reason != "v1, candidate 1 was superseded by v2" || st.History[3].Label != (Label{1, 1}) ||
		st.Request.Criteria != "cite sources" {
		t.Errorf("history = %+v", st.History)
	}
	if st.Version != 2 || st.Verifier != "v1" || st.Pending || st.Withdrawn != (Label{1, 1}) || st.Last.Action != event.VerifyReplace {
		t.Errorf("after the replace: %+v; want v2, the same verifier, nothing pending", st)
	}
	if s, _ := store.GetSession(ctx, runID, "v1"); s.Status != db.SessionOngoing {
		t.Errorf("verifier is %s after the replace, want it kept", s.Status)
	}
	if w := withdrawals(t, store, runID); len(w) != 1 || w[0] != Withdrawal("worker", Label{1, 1}, 2) ||
		!strings.HasPrefix(w[0], "[system] v1 is withdrawn; stop and wait for the next candidate.") {
		t.Errorf("the verifier's withdrawal messages = %q", w)
	}
	if db.IsRecordOnly(st.History[3].VerifyEvent) || db.IsInput(st.History[3].VerifyEvent) {
		t.Error("the withdrawn record must wake a live agent, and revive nothing")
	}
	// Nothing is pending now, so a late verdict of v1's would revive no crashed agent.
	if AwaitsVerdict(ctx, store, runID, "worker", "v1") {
		t.Error("a withdrawn candidate still awaits its verdict")
	}

	// The same verifier gets v2's candidate; a clear withdraws it and keeps it.
	appendAll(t, store, runID, "worker", final("v2"), ve(event.VerifyVerifying, "v1", 1))
	if cleared, err := Clear(ctx, store, reg, runID, "worker", "boss"); err != nil || !cleared {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	if s, _ := store.GetSession(ctx, runID, "v1"); s.Status != db.SessionOngoing {
		t.Errorf("verifier is %s after a clear, want it kept", s.Status)
	}
	if w := withdrawals(t, store, runID); len(w) != 2 || w[1] != Withdrawal("worker", Label{2, 1}, 0) {
		t.Errorf("the verifier's withdrawal messages = %q", w)
	}
	if cleared, err := Clear(ctx, store, reg, runID, "worker", "boss"); err != nil || cleared {
		t.Errorf("clearing what is off = %v, %v; want false", cleared, err)
	}
	st, _ = Load(ctx, store, runID, "worker")
	if len(st.History) != 7 || st.Status() != StatusOff || st.History[6].Reason != "verification was turned off, which withdrew v2, candidate 1" {
		t.Errorf("history = %d events (%+v), status %s; want 7 (set, verifying, replace, withdrawn, verifying, clear, "+
			"withdrawn) and off", len(st.History), st.History, st.Status())
	}

	// A verifier that is done (concluded) isn't messaged: that would revive it.
	// Its verdict on the withdrawn candidate, which the agent hadn't acted on,
	// is recorded as stale.
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		final("v3"), ve(event.VerifyVerifying, "v1", 1))
	if _, err := store.AdvanceStep(ctx, runID, "v1"); err != nil {
		t.Fatal(err)
	}
	appendAll(t, store, runID, "v1", final("PASS: v3 holds"))
	_ = store.UpdateSessionStatus(ctx, runID, "v1", db.SessionConcluded)
	v1, _ := store.GetSession(ctx, runID, "v1")
	appendAll(t, store, runID, "worker", event.VerifierOutcome("v1", "PASS: v3 holds", "", v1.StatusChangedAt))
	if n, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", MaxRounds: 3}); err != nil || n != 4 {
		t.Fatalf("Set = %d, %v; want v4", n, err)
	}
	if w := withdrawals(t, store, runID); len(w) != 2 {
		t.Errorf("a concluded verifier was told of a withdrawal: %q", w)
	}
	st, _ = Load(ctx, store, runID, "worker")
	if h := st.History; h[len(h)-1].Action != event.VerifyStale || h[len(h)-1].Reason != "for v3, candidate 1, which v4 superseded" ||
		*h[len(h)-1].Outcome != (event.VerifyOutcome{Verifier: "v1", Verdict: event.VerdictPass, Findings: "v3 holds", At: v1.StatusChangedAt}) ||
		!h[len(h)-4].Stale || h[len(h)-4].Action != event.VerifyVerdict {
		t.Errorf("the verdict in before v4 started: %+v", h[len(h)-4:])
	}

	// Nor is one that stopped (a message would revive it).
	appendAll(t, store, runID, "worker", final("v4"), ve(event.VerifyVerifying, "v1", 1))
	_ = store.UpdateSessionStatus(ctx, runID, "v1", db.SessionCrashed)
	if n, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", MaxRounds: 3}); err != nil || n != 5 {
		t.Fatalf("Set = %d, %v; want v5", n, err)
	}
	if w := withdrawals(t, store, runID); len(w) != 2 {
		t.Errorf("a crashed verifier was told of a withdrawal: %q", w)
	}

	// A version on another model retires the verifier with the same
	// bookkeeping: its verdict the agent hadn't acted on is recorded stale, and
	// the withdrawal names it.
	addSession(t, store, runID, "v0", "worker", "verify_test_agent", db.SessionOngoing, map[string]any{MetadataVerifierFor: "worker"})
	appendAll(t, store, runID, "v0", final("PASS: fine"))
	_ = store.UpdateSessionStatus(ctx, runID, "v0", db.SessionConcluded)
	v0, _ := store.GetSession(ctx, runID, "v0")
	appendAll(t, store, runID, "worker", final("v5"), ve(event.VerifyVerifying, "v0", 1),
		event.VerifierOutcome("v0", "PASS: fine", "", v0.StatusChangedAt))
	if _, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", Model: "another:model", MaxRounds: 3}); err != nil {
		t.Fatal(err)
	}
	st, _ = Load(ctx, store, runID, "worker")
	if h := st.History; h[len(h)-1].Action != event.VerifyStale || h[len(h)-1].Verifier != "v0" ||
		h[len(h)-2].Action != event.VerifyWithdrawn || h[len(h)-2].Verifier != "v0" || !h[len(h)-4].Stale {
		t.Errorf("a model change's bookkeeping: %+v", h[len(h)-4:])
	}
	if lab, stale := st.StaleLabel(st.History[len(st.History)-4].Outcome); !stale || lab != (Label{5, 1}) {
		t.Errorf("StaleLabel = %v, %v; want v5, candidate 1, stale", lab, stale)
	}
	if s, _ := store.GetSession(ctx, runID, "v0"); s.Status != db.SessionConcluded || st.Verifier != "" {
		t.Errorf("v0 is %s, the state's verifier %q; want it retired", s.Status, st.Verifier)
	}

	// So does a version on another model while the verifier still works:
	// cancelled, and the next conclusion starts a new one.
	_ = store.UpdateSessionStatus(ctx, runID, "v1", db.SessionOngoing)
	appendAll(t, store, runID, "worker", final("v6"), ve(event.VerifyVerifying, "v1", 1))
	if _, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", Model: "other:model", MaxRounds: 3}); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.GetSession(ctx, runID, "v1"); s.Status != db.SessionCancelled {
		t.Errorf("verifier is %s after a version on another model, want cancelled", s.Status)
	}
	if st, _ := Load(ctx, store, runID, "worker"); st.Verifier != "" || st.Version != 7 {
		t.Errorf("after a version on another model: verifier %q, v%d; want none, v7", st.Verifier, st.Version)
	}
	events, _ := store.GetEvents(ctx, runID, "worker", db.EventFilter{})
	for _, e := range events {
		if _, ok := e.Event.(*event.ChildResultEvent); ok {
			t.Errorf("worker got a child_result from its verifier: %+v", e.Event)
		}
	}

	addSession(t, store, runID, "bot", "", "verify_test_chat", db.SessionIdle, nil)
	if _, err := Set(ctx, store, reg, runID, "bot", &event.VerifyEvent{}); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Errorf("verification set on an interactive agent: %v", err)
	}
	if _, err := Set(ctx, store, reg, runID, "v1", &event.VerifyEvent{}); err == nil || !strings.Contains(err.Error(), "verifier") {
		t.Errorf("verification set on a verifier: %v", err)
	}
	if _, err := Set(ctx, store, reg, runID, "nobody", &event.VerifyEvent{}); err == nil {
		t.Error("verification set on a missing session")
	}
}

// Versions and candidates: each set or replace is a new version, each new
// candidate given to the verifier in it the next number; a re-engagement with
// the same candidate after a stop isn't a new one. The verifier is kept across
// versions, unless one asks for another model; its stops while it has no
// candidate (the version changed) don't count.
func TestDerive_Versions(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3}
	replace := &event.VerifyEvent{Action: event.VerifyReplace, By: "operator", MaxRounds: 3}
	stop := event.VerifierOutcome("v", "provider refused", db.SessionCrashed, time.Time{})
	st := Derive(recs(set, final("a"), ve("verifying", "v", 1), stop, concluding(""), ve("verifying", "v", 1),
		concluding("b"), ve("verifying", "v", 1)))
	if st.Version != 1 || st.Candidates != 2 || st.Label().String() != "v1, candidate 2" || st.Next().String() != "v1, candidate 3" {
		t.Errorf("v1: %+v; want candidate 2 (the re-engagement after the stop isn't a new one)", st)
	}
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, stop, stop, stop))
	if st.Version != 2 || st.Verifier != "v" || st.VerifierSince != 1 || st.Pending || st.Withdrawn != (Label{1, 1}) ||
		st.Stops != 0 || st.Label() != (Label{Version: 2}) {
		t.Errorf("after the replace: %+v; want v2 with the same verifier, v1's candidate withdrawn, no stops counted", st)
	}
	if h := st.History; h[0].Label != (Label{Version: 1}) || h[1].Label != (Label{1, 1}) || h[2].Label != (Label{Version: 2}) {
		t.Errorf("labels = %v, %v, %v", h[0].Label, h[1].Label, h[2].Label)
	}
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, final("b"), ve("verifying", "v", 1), stop))
	if st.Label() != (Label{2, 1}) || st.Stops != 1 || st.VerifierSince != 1 {
		t.Errorf("v2's first candidate: %+v; want v2, candidate 1, its stop counted", st)
	}
	other := &event.VerifyEvent{Action: event.VerifyReplace, By: "operator", Model: "other:model", MaxRounds: 3}
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), other))
	if st.Verifier != "" {
		t.Errorf("a version on another model kept the verifier %q", st.Verifier)
	}
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), other, final("b"), ve("verifying", "w", 1)))
	if st.Verifier != "w" || st.VerifierSince != 2 {
		t.Errorf("the new verifier: %q since v%d; want w since v2", st.Verifier, st.VerifierSince)
	}
	cleared := Derive(recs(set, final("a"), ve("verifying", "v", 1), &event.VerifyEvent{Action: event.VerifyClear, By: "boss"}))
	if cleared.On || cleared.Version != 1 || cleared.Verifier != "v" || cleared.Withdrawn != (Label{1, 1}) {
		t.Errorf("after a clear: %+v", cleared)
	}
	if again := Derive(recs(set, final("a"), ve("verifying", "v", 1), ve("passed", "v", 1), set)); again.Version != 2 ||
		again.Verifier != "v" || again.Withdrawn != (Label{}) {
		t.Errorf("set again after a pass: %+v; want v2, the same verifier, nothing withdrawn", again)
	}
	// A verifier that ended its version itself (stops, or it couldn't start)
	// isn't kept; one whose rejections ran out is.
	stops := &event.VerifyEvent{Action: event.VerifyExhausted, By: event.ByHarness, Verifier: "v",
		Reason: "the verifier stopped 3 times without a verdict; last: crashed: boom"}
	if st := Derive(recs(set, final("a"), ve("verifying", "v", 1), stops, set)); st.Verifier != "" {
		t.Errorf("a verifier that stopped too often was kept: %q", st.Verifier)
	}
	if st := Derive(recs(set, final("a"), ve("verifying", "v", 1), ve("exhausted", "v", 3), set)); st.Verifier != "v" {
		t.Errorf("a verifier whose rejections ran out wasn't kept: %q", st.Verifier)
	}
	// What withdrew a candidate: a clear, even if a set followed.
	cleared = Derive(recs(set, final("a"), ve("verifying", "v", 1), &event.VerifyEvent{Action: event.VerifyClear, By: "boss"}, set))
	if cleared.WithdrawnBy(Label{1, 1}) != 0 || cleared.WithdrawnBy(Label{2, 1}) != 2 {
		t.Errorf("withdrawn by %d / %d; want 0 (the clear) and the current version", cleared.WithdrawnBy(Label{1, 1}),
			cleared.WithdrawnBy(Label{2, 1}))
	}
}

// A verdict that arrives after the version changed, before a candidate of
// the new version went to the verifier, is late: on a withdrawn candidate.
// A stale record marks the latest verdict before it that it carries; acting
// on a verdict (a rejection, a verdict told by message) takes it off the list.
func TestDerive_LateVerdicts(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3}
	replace := &event.VerifyEvent{Action: event.VerifyReplace, By: "operator", MaxRounds: 3}
	fail := event.VerifierOutcome("v", "FAIL: no unit", "", time.Time{})
	stale := &event.VerifyEvent{Action: event.VerifyStale, By: event.ByHarness, Verifier: "v",
		Outcome: &event.VerifyOutcome{Verifier: "v", Verdict: event.VerdictFail, Findings: "no unit"}, Reason: "for v1, candidate 1, which v2 superseded"}
	st := Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, fail))
	if len(st.Late) != 1 || st.History[st.Late[0]].Label != (Label{1, 1}) {
		t.Errorf("late = %v; want the verdict, on v1, candidate 1", st.Late)
	}
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, fail, stale))
	if len(st.Late) != 0 || !st.History[3].Stale || st.History[4].Label != (Label{1, 1}) || st.Last.Action != event.VerifyReplace {
		t.Errorf("after the stale record: %+v", st)
	}
	// One that came before the change, recorded stale by the change.
	st = Derive(recs(set, final("a"), ve("verifying", "v", 1), fail, replace, stale))
	if len(st.Late) != 0 || !st.History[2].Stale || st.History[4].Label != (Label{1, 1}) {
		t.Errorf("stale record for a verdict before the change: %+v", st)
	}
	if st := Derive(recs(set, final("a"), ve("verifying", "v", 1), fail)); len(st.Late) != 0 {
		t.Errorf("a verdict on the current version is late: %v", st.Late)
	}
	if st := Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, final("b"), ve("verifying", "v", 1), fail)); len(st.Late) != 0 {
		t.Errorf("a verdict after v2's first candidate is late: %v", st.Late)
	}
	toldPass := &event.UserEvent{Content: "[system] passed", Check: event.CheckVerify, Outcome: &event.VerifyOutcome{Verifier: "v", Verdict: event.VerdictPass}}
	// A fast verifier's verdict on v2's first candidate, before the record that sent it.
	for name, settle := range map[string]event.Event{"rejected": ve("rejected", "v", 1), "told": toldPass} {
		evs := recs(set, final("a"), ve("verifying", "v", 1), replace, final("b"), fail, ve("verifying", "v", 1), settle)
		if st := Derive(evs); len(st.Late) != 0 {
			t.Errorf("%s: late %v; want it acted on", name, st.Late)
		}
	}
	toldStop := &event.UserEvent{Check: event.CheckVerify, Outcome: &event.VerifyOutcome{Verifier: "v", Verdict: db.SessionCrashed}}
	if st := Derive(recs(set, final("a"), ve("verifying", "v", 1), replace, fail, toldStop)); len(st.Late) != 1 {
		t.Errorf("a told stop took a late verdict off: %v", st.Late)
	}
	cleared := Derive(recs(set, final("a"), ve("verifying", "v", 1), &event.VerifyEvent{Action: event.VerifyClear, By: "boss"}, fail))
	if len(cleared.Late) != 1 {
		t.Errorf("a verdict after a clear isn't late: %v", cleared.Late)
	}
}

// A stale record's reason names the candidate and what withdrew it.
func TestStaleReason(t *testing.T) {
	if got := Stale(Label{1, 2}, 3); got != "for v1, candidate 2, which v3 superseded" {
		t.Errorf("superseded: %q", got)
	}
	if got := Stale(Label{2, 1}, 0); got != "for v2, candidate 1, withdrawn when verification was turned off" {
		t.Errorf("cleared: %q", got)
	}
}

// Describe counts a verdict the agent hasn't acted on yet, and reports the
// request that ended when verification is over.
func TestDescribe(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	addSession(t, store, runID, "worker", "boss", "verify_test_agent", db.SessionAwaiting, nil)
	addSession(t, store, runID, "v", "worker", "verify_test_agent", db.SessionOngoing, map[string]any{MetadataVerifierFor: "worker"})
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", Criteria: "has units", MaxRounds: 2},
		final("42"), ve(event.VerifyVerifying, "v", 1))
	appendAll(t, store, runID, "v", &event.UserEvent{Content: "verify 42"})
	r, err := Describe(ctx, store, runID, "worker")
	if err != nil || r.State != StatusVerifying || r.Round != 1 || r.Verifier != "v" || r.Criteria != "has units" ||
		r.MaxRounds != 2 || len(r.History) != 2 || r.Version != 1 || r.Candidate != 1 {
		t.Errorf("verifying: %+v, %v", r, err)
	}
	if c := r.History[1]; c.Version != 1 || c.Candidate != 1 {
		t.Errorf("the verifying entry is about v%d, candidate %d; want v1, candidate 1", c.Version, c.Candidate)
	}
	appendAll(t, store, runID, "v", final("FAIL: no unit"))
	_ = store.UpdateSessionStatus(ctx, runID, "v", db.SessionConcluded)
	// The verifier's outcome record is in the history, with what it said.
	appendAll(t, store, runID, "worker", event.VerifierOutcome("v", "FAIL: no unit", "", time.Time{}))
	r, _ = Describe(ctx, store, runID, "worker")
	if r.State != StatusOn || r.Round != 2 {
		t.Errorf("after an unsettled rejection: %s round %d, want on round 2", r.State, r.Round)
	}
	if c := r.History[len(r.History)-1]; c.Action != event.VerifyVerdict || c.Outcome == nil ||
		*c.Outcome != (event.VerifyOutcome{Verifier: "v", Verdict: event.VerdictFail, Findings: "no unit"}) {
		t.Errorf("the outcome's history entry = %+v", c)
	}
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifyPassed, By: event.ByHarness, Verifier: "v", Round: 2})
	if r, _ := Describe(ctx, store, runID, "worker"); r.State != StatusPassed || r.Round != 2 || r.Criteria != "has units" || r.Verifier != "v" {
		t.Errorf("passed: %+v", r)
	}
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		&event.VerifyEvent{Action: event.VerifyClear, By: "boss"})
	if r, _ := Describe(ctx, store, runID, "worker"); r.State != StatusOff || r.Verifier != "" || r.Criteria != "" ||
		len(r.History) != 6 || r.Version != 2 || r.History[4].Version != 2 {
		t.Errorf("cleared: %+v", r)
	}
	// A late verdict (after a change, before the next candidate) is stale in
	// the history even before the agent's next read records it.
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		final("43"), ve(event.VerifyVerifying, "v", 1), &event.VerifyEvent{Action: event.VerifyReplace, By: "boss", MaxRounds: 3},
		event.VerifierOutcome("v", "FAIL: late", "", time.Time{}))
	if r, _ := Describe(ctx, store, runID, "worker"); !r.History[len(r.History)-1].Stale {
		t.Errorf("a late verdict isn't stale in the history: %+v", r.History[len(r.History)-1])
	}
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifyClear, By: "boss"})
	// A stale verdict is marked in the history.
	appendAll(t, store, runID, "worker", event.VerifierOutcome("v", "PASS", "", time.Time{}),
		&event.VerifyEvent{Action: event.VerifyStale, By: event.ByHarness, Verifier: "v",
			Outcome: &event.VerifyOutcome{Verifier: "v", Verdict: event.VerdictPass}, Reason: "for v1, ..."})
	if r, _ := Describe(ctx, store, runID, "worker"); !r.History[len(r.History)-2].Stale ||
		r.History[len(r.History)-1].Action != event.VerifyStale || r.History[len(r.History)-1].Version != 3 {
		t.Errorf("stale verdict: %+v", r.History[len(r.History)-2:])
	}
	// An end other than the rounds carries its reason into the history.
	why := "the verifier could not be started: no model"
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		&event.VerifyEvent{Action: event.VerifyExhausted, By: event.ByHarness, Reason: why})
	if r, _ := Describe(ctx, store, runID, "worker"); r.State != StatusNotVerified || r.History[len(r.History)-1].Reason != why {
		t.Errorf("exhausted: %+v", r)
	}
}

// The parent's notes read "label: text", and long findings are shortened to
// one line.
func TestNotes(t *testing.T) {
	if n := VerifiedNote("quick-rabbit", 2); n.Kind != event.NoteVerified || n.Text != "quick-rabbit (round 2)" {
		t.Errorf("verified note = %+v", n)
	}
	n := NotVerifiedNote(3, "the build\nis broken "+strings.Repeat("x", 1000))
	if n.Kind != event.NoteNotVerified || !strings.HasPrefix(n.Text, "after 3 rejections. Latest findings: the build is broken x") ||
		len([]rune(n.Text)) > maxFindingsRunes+50 {
		t.Errorf("not-verified note = %q", n.Text)
	}
	cr := &event.ChildResultEvent{ChildSessionID: "w", Verdict: "concluded", Notes: []event.ResultNote{VerifiedNote("quick-rabbit", 2)}}
	if !strings.Contains(cr.ToText(), "· verified by: quick-rabbit (round 2) ====") {
		t.Errorf("header = %q", cr.ToText())
	}
}

// A version a loop's tick started is named by the tick's label, so the agent
// reads a note for it only when the loop gave criteria: one line of them.
func TestNote_ByLoop(t *testing.T) {
	if got := Note(event.LoopVerify{MaxRounds: 3}.Event("2"), 4); got != "" {
		t.Errorf("set by a loop without criteria = %q, want no note", got)
	}
	replace := event.LoopVerify{MaxRounds: 3}.Event("2")
	replace.Action = event.VerifyReplace
	if got := Note(replace, 5); got != "" {
		t.Errorf("replace by a loop without criteria = %q, want no note", got)
	}
	replace.Criteria = "the tests pass"
	if got, want := Note(replace, 5), "[system] Verification v5 checks your conclusion against: the tests pass."; got != want {
		t.Errorf("replace by a loop = %q, want %q", got, want)
	}
	if got := Note(&event.VerifyEvent{Action: event.VerifyClear, By: event.ByLoop("2")}, 5); !strings.HasPrefix(got, "[system] The operator's loop 2 turned verification off") {
		t.Errorf("clear by a loop = %q", got)
	}
}

// Tick records a loop's change with its tick in one write: a new version with
// a request, a clear of the loop's own version (only its own) without one, or
// nothing; a failed write records nothing.
func TestTick(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	addSession(t, store, runID, "worker", "", "verify_test_agent", db.SessionConcluded, nil)
	var writes []string
	write := func(change *event.VerifyEvent, version int) error {
		evs := []event.Event{&event.UserEvent{Content: "tick", Loop: &event.LoopTick{ID: "2", N: 1, Version: version}}}
		desc := fmt.Sprintf("tick v%d", version)
		if change != nil {
			evs = append([]event.Event{change}, evs...)
			desc = change.Action + " by " + change.By + ", " + desc
		}
		writes = append(writes, desc)
		return store.AppendEventsIf(ctx, runID, "worker", nil, nil, evs...)
	}
	if n, err := Tick(ctx, store, nil, runID, "worker", "2", &event.LoopVerify{MaxRounds: 2}, write); err != nil || n != 1 {
		t.Fatalf("Tick with a request = %d, %v", n, err)
	}
	if n, err := Tick(ctx, store, nil, runID, "worker", "2", nil, write); err != nil || n != 0 {
		t.Fatalf("Tick without = %d, %v", n, err)
	}
	if _, err := Tick(ctx, store, nil, runID, "worker", "2", nil, write); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(ctx, store, nil, runID, "worker", &event.VerifyEvent{By: "operator", MaxRounds: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := Tick(ctx, store, nil, runID, "worker", "2", nil, write); err != nil {
		t.Fatal(err)
	}
	want := "set by loop 2, tick v1 | clear by loop 2, tick v0 | tick v0 | tick v0"
	if got := strings.Join(writes, " | "); got != want {
		t.Errorf("writes %q\nwant %q", got, want)
	}
	if st, _ := Load(ctx, store, runID, "worker"); !st.On || st.Request.By != event.ByOperator {
		t.Errorf("the operator's version was touched: %+v", st.Request)
	}
	failing := func(*event.VerifyEvent, int) error { return errors.New("disk on fire") }
	if n, err := Tick(ctx, store, nil, runID, "worker", "2", &event.LoopVerify{}, failing); err == nil || n != 0 {
		t.Errorf("Tick with a failed write = %d, %v", n, err)
	}
	if st, _ := Load(ctx, store, runID, "worker"); st.Version != 2 {
		t.Errorf("a failed write started a version: v%d", st.Version)
	}
}

// A loop's request is resolved when the loop is set, as a request is.
func TestRequest_ForLoop(t *testing.T) {
	ctx := context.Background()
	d := config.VerifyDefaults{MaxRounds: 3, Model: "#judge"}
	resolve := func(_ context.Context, model string) (string, llm.Provider, error) {
		if model != "#judge" {
			return "", nil, fmt.Errorf("unknown model %q", model)
		}
		return "vertex:judge", nil, nil
	}
	v, err := Request{Criteria: " the tests pass "}.ForLoop(ctx, d, resolve)
	if err != nil || *v != (event.LoopVerify{Criteria: "the tests pass", Model: "vertex:judge", MaxRounds: 3}) {
		t.Errorf("ForLoop = %+v, %v", v, err)
	}
	if _, err := (Request{Model: "nope"}).ForLoop(ctx, d, resolve); err == nil {
		t.Error("an unknown model was accepted")
	}
}

// A session's lock entry lives only while it is held or waited for.
func TestLockEntriesDropped(t *testing.T) {
	unlock := Lock("run", "worker")
	locksMu.Lock()
	held := locks["run/worker"] != nil
	locksMu.Unlock()
	unlock()
	locksMu.Lock()
	defer locksMu.Unlock()
	if !held || len(locks) != 0 {
		t.Errorf("held = %v, entries after unlock = %d; want true and 0", held, len(locks))
	}
}

// failGet fails every GetSession, as a broken database does.
type failGet struct{ db.Store }

func (failGet) GetSession(context.Context, string, string) (*db.SessionRecord, error) {
	return nil, fmt.Errorf("%w: database is locked", db.ErrStore)
}

// A failed read of the verifier is an error, not a missing verifier (which
// would start a second one); only a session that doesn't exist is missing.
func TestInspectReadErrorIsNotMissing(t *testing.T) {
	store, runID := newStore(t)
	if v, err := Inspect(context.Background(), store, runID, "nobody"); err != nil || !v.Missing {
		t.Errorf("a session that doesn't exist = %+v, %v; want missing", v, err)
	}
	if v, err := Inspect(context.Background(), failGet{store}, runID, "anyone"); err == nil || v.Missing {
		t.Errorf("a failed read = %+v, %v; want an error", v, err)
	}
}

// Load reads a stream's verification by type, since a verifying record
// carries its candidate. One written before records carried it takes the
// candidate from the turn before it, so a stream with one is read whole.
func TestLoad_ReadsByTypeUnlessARecordLacksItsCandidate(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	set := func() *event.VerifyEvent {
		return &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3}
	}

	addSession(t, store, runID, "worker", "boss", "verify_test_agent", db.SessionAwaiting, nil)
	carried := ve(event.VerifyVerifying, "v", 1)
	carried.Candidate = "42 m"
	appendAll(t, store, runID, "worker", set(), final("42 m"), carried)
	if st, err := Load(ctx, store, runID, "worker"); err != nil || !st.Pending || st.Candidate != "42 m" {
		t.Errorf("Load = %+v, %v; want 42 m pending", st, err)
	}

	addSession(t, store, runID, "older", "boss", "verify_test_agent", db.SessionAwaiting, nil)
	appendAll(t, store, runID, "older", set(), final("41 m"), ve(event.VerifyVerifying, "v", 1))
	if st, err := Load(ctx, store, runID, "older"); err != nil || !st.Pending || st.Candidate != "41 m" {
		t.Errorf("Load of an older stream = %+v, %v; want 41 m, from its turn", st, err)
	}
}

// SetThen and ClearThen run then under the verification lock, after the
// change and the withdrawal it writes: a conclude call the withdrawal wakes
// settles under that lock, so it finds the message then writes in place.
func TestSetThenAndClearThen_RunUnderTheLock(t *testing.T) {
	store, runID := newStore(t)
	ctx := context.Background()
	reg := session.NewRegistry()
	addSession(t, store, runID, "worker", "boss", "verify_test_agent", db.SessionAwaiting, nil)
	addSession(t, store, runID, "v1", "worker", "verify_test_agent", db.SessionOngoing, map[string]any{MetadataVerifierFor: "worker"})
	if _, err := Set(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", MaxRounds: 3}); err != nil {
		t.Fatal(err)
	}
	pending := func() {
		t.Helper()
		v := ve(event.VerifyVerifying, "v1", 1)
		v.Candidate = "42 m"
		appendAll(t, store, runID, "worker", final("42 m"), v)
	}
	// then: whether the lock was free while it ran, and a message.
	then := func(free *bool, msg string) func() {
		return func() {
			got := make(chan struct{})
			go func() { defer Lock(runID, "worker")(); close(got) }()
			select {
			case <-got:
				*free = true
			case <-time.After(50 * time.Millisecond):
			}
			appendAll(t, store, runID, "worker", &event.MessageEvent{Content: msg, Sender: "boss", SenderType: event.SenderTypeAgent})
		}
	}
	// after reports whether the message msg comes after the latest withdrawal.
	after := func(msg string) bool {
		evs, err := store.GetEvents(ctx, runID, "worker", db.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		withdrawn := -1
		for i, r := range evs {
			switch e := r.Event.(type) {
			case *event.VerifyEvent:
				if e.Action == event.VerifyWithdrawn {
					withdrawn = i
				}
			case *event.MessageEvent:
				if e.Content == msg {
					return withdrawn >= 0 && i > withdrawn
				}
			}
		}
		return false
	}

	pending()
	var free bool
	if _, err := SetThen(ctx, store, reg, runID, "worker", &event.VerifyEvent{By: "boss", MaxRounds: 3},
		then(&free, "also in feet")); err != nil {
		t.Fatal(err)
	}
	if free || !after("also in feet") {
		t.Errorf("SetThen: lock free during then %v, message after the withdrawal %v", free, after("also in feet"))
	}

	pending()
	free = false
	if cleared, err := ClearThen(ctx, store, reg, runID, "worker", "boss", then(&free, "never mind")); err != nil || !cleared {
		t.Fatalf("ClearThen = %v, %v", cleared, err)
	}
	if free || !after("never mind") {
		t.Errorf("ClearThen: lock free during then %v, message after the withdrawal %v", free, after("never mind"))
	}
}
