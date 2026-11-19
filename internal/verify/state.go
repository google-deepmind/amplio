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
	"fmt"
	"slices"
	"strings"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
)

// State is a session's verification, derived from its stream.
type State struct {
	On bool
	// Request is the set or replace in force (nil when off).
	Request *event.VerifyEvent
	// Version counts the sets and replaces so far: each starts a new version
	// (v1, v2, …), and the latest is the current one while verification is on.
	Version int
	// Verifier is the agent's verifier, once one was started. It is kept
	// across versions (it keeps what it learned), unless a version asks for
	// another model; then it is "" until the next one starts.
	Verifier string
	// VerifierSince is the version the verifier was started in: one started
	// earlier hasn't seen the current version's request yet.
	VerifierSince int
	// Rejections counts the rounds so far in this version: the verifier's
	// rejections of the candidate that was the latest when it gave them.
	Rejections int
	// Pending: the latest candidate of this version is with the verifier, no
	// verdict recorded.
	Pending bool
	// Candidate is the latest candidate given to the verifier in this version,
	// and Candidates their number: the latest is "candidate Candidates".
	Candidate  string
	Candidates int
	// Withdrawn names the latest candidate a newer version or a clear withdrew
	// before it had a verdict (zero if none).
	Withdrawn Label
	// Stops counts the verifier's stops without a verdict (crashed,
	// cancelled) since its last rejection; LastStop is the latest one's
	// verdict and reason.
	Stops    int
	LastStop string
	// EndTold is what the agent was told when the verification of the latest
	// candidate ended without a pass (the outcome on Ended's message: the last
	// allowed rejection, or the verifier's last allowed stop). Its next
	// conclusion then goes through not verified, whatever the verifier says
	// since. Nil until then.
	EndTold *event.VerifyOutcome
	// Last is the latest VerifyEvent other than a verifier's verdict record and
	// the withdrawn and stale records (nil if there is none); after a
	// verification ends it says how.
	Last *event.VerifyEvent
	// History is every VerifyEvent, oldest first, with the candidate or
	// version each one is about.
	History []Entry
	// Late are the verifiers' verdicts (PASS or FAIL) that arrived after the
	// latest version change, before a candidate of the new version went to
	// the verifier: verdicts on a withdrawn candidate, which the harness
	// records as stale. Indexes into History, oldest first; a verdict leaves
	// when it is recorded stale, or acted on (a fast verifier's verdict on the
	// version's first candidate can land before the record that sent it).
	Late []int

	verifierModel string // the request's model when Verifier was started
	sent          Label  // the latest candidate given to a verifier
	standby       string // the kept verifier, with no candidate since the version changed
	changed       bool   // the version changed (or ended by a clear), no candidate sent since
	withdrawnBy   int    // the version that withdrew Withdrawn, 0 for a clear
	retired       bool   // the verifier ended its version itself (stops, or it couldn't start)
}

// Entry is one VerifyEvent in a State's History.
type Entry struct {
	*event.VerifyEvent
	// Label is what it is about: the version a set, replace or clear started
	// or ended, the candidate a harness record is about, and for a verdict
	// the candidate given to a verifier last before it arrived.
	Label Label
	// Stale: a verdict the harness recorded as stale (a later stale record
	// carrying it).
	Stale bool
}

// Label names a candidate by its version and its number within it ("v2,
// candidate 1"), or a version alone (Candidate 0).
type Label struct{ Version, Candidate int }

func (l Label) String() string {
	switch {
	case l.Version == 0:
		return ""
	case l.Candidate == 0:
		return fmt.Sprintf("v%d", l.Version)
	}
	return fmt.Sprintf("v%d, candidate %d", l.Version, l.Candidate)
}

// CurrentVersion is the version in force: 0 while verification is off.
func (s State) CurrentVersion() int {
	if s.On {
		return s.Version
	}
	return 0
}

// StaleLabel reports whether o, a verifier's verdict in the stream, is stale
// (recorded so, or late: about to be), and the candidate it was on.
func (s State) StaleLabel(o *event.VerifyOutcome) (Label, bool) {
	for h := len(s.History) - 1; h >= 0; h-- {
		if e := s.History[h]; e.Action == event.VerifyVerdict && e.Outcome != nil && *e.Outcome == *o {
			return e.Label, e.Stale || slices.Contains(s.Late, h)
		}
	}
	return Label{}, false
}

// WithdrawnBy is the version that withdrew candidate lab, or 0 if
// verification was turned off: what made a verdict on it stale.
func (s State) WithdrawnBy(lab Label) int {
	if lab == s.Withdrawn {
		return s.withdrawnBy
	}
	return s.CurrentVersion()
}

// Label is the latest candidate of the current version.
func (s State) Label() Label { return Label{s.Version, s.Candidates} }

// Next is the label of a new candidate in the current version.
func (s State) Next() Label { return Label{s.Version, s.Candidates + 1} }

// States as the API reports them.
const (
	StatusOff         = "off"          // never set, or cleared
	StatusOn          = "on"           // set; no candidate with the verifier
	StatusVerifying   = "verifying"    // a candidate is with the verifier
	StatusPassed      = "passed"       // the verifier passed the latest candidate
	StatusNotVerified = "not_verified" // the rounds ran out
)

// Derive reads a session's verification from its events, oldest first (the
// whole stream: a compaction must not lose it). A candidate is the one the
// verifying event that gave it to the verifier carries (see candidateOf).
func Derive(events []db.EventRecord) State {
	var st State
	var attempt *event.AssistantEvent
	// Stops are counted per child, since one can land before the verifying
	// event that names its verifier (a new verifier failing at once).
	type stops struct {
		n    int
		last string
	}
	stopped := map[string]stops{}
	countStop := func(child, text string) {
		// The kept verifier has no candidate until the next one is sent: a stop
		// then isn't a failure to verify anything.
		if st.On && child != st.standby {
			stopped[child] = stops{stopped[child].n + 1, text}
		}
	}
	for _, rec := range events {
		switch e := rec.Event.(type) {
		case *event.AssistantEvent:
			attempt = e
		case *event.UserEvent:
			// A verdict told by message (written after its record) was acted on;
			// one told as information wasn't.
			if o := e.Outcome; o != nil && !o.Stopped() && !o.Earlier {
				st.Late = nil
			}
			if e.Check == event.CheckVerify && e.Outcome != nil && IsEnded(e.Content) {
				st.EndTold = e.Outcome
			}
		case *event.ChildResultEvent:
			// How a verifier's stop arrived before verifiers posted their outcome.
			if e.Verdict == db.SessionCrashed || e.Verdict == db.SessionCancelled {
				countStop(e.ChildSessionID, stopText(e.Verdict, e.Content))
			}
		case *event.VerifyEvent:
			st.apply(e, attempt, countStop, func(child string) { delete(stopped, child) }, func() { clear(stopped) })
		}
	}
	if st.On {
		st.Stops, st.LastStop = stopped[st.Verifier].n, stopped[st.Verifier].last
	}
	return st
}

// candidateOf is the candidate verifying record e gave the verifier: the one
// it carries, recorded by the harness, which knows what it gave (a reply taken
// as conclude() gives the pending candidate again, not its own text). A record
// without one, written before records carried their candidate, gave what the
// attempt before it offered (event.AssistantEvent.Offered); ok is false when
// that attempt offers none (conclude() gave the pending candidate again).
func candidateOf(e *event.VerifyEvent, attempt *event.AssistantEvent) (string, bool) {
	if e.Candidate != "" {
		return e.Candidate, true
	}
	if attempt == nil {
		return "", false
	}
	return attempt.Offered()
}

// apply folds one VerifyEvent into the state. countStop counts a verifier's
// stop, endStops ends a verifier's run of them, resetStops all of them.
func (st *State) apply(e *event.VerifyEvent, attempt *event.AssistantEvent, countStop func(child, text string),
	endStops func(child string), resetStops func()) {
	entry := Entry{VerifyEvent: e}
	switch e.Action {
	case event.VerifyVerdict:
		// The verifier's own record: history, but no step of the verification
		// (Last stays the harness's or requester's).
		entry.Label = st.sent
		switch o := e.Outcome; {
		case o == nil:
		case o.Stopped():
			countStop(o.Verifier, stopText(o.Verdict, o.Findings))
		default:
			if st.On {
				endStops(o.Verifier) // a verdict ends a run of stops
			}
			if st.changed {
				st.Late = append(st.Late, len(st.History))
			}
		}
		st.History = append(st.History, entry)
		return
	case event.VerifyStale:
		if h := st.staleVerdict(e.Outcome); h >= 0 {
			st.History[h].Stale = true
			entry.Label = st.History[h].Label
			st.Late = slices.DeleteFunc(st.Late, func(i int) bool { return i == h })
		}
		st.History = append(st.History, entry)
		return
	case event.VerifyWithdrawn:
		entry.Label = st.Withdrawn
		st.History = append(st.History, entry)
		return
	}
	st.Last = e
	switch e.Action {
	case event.VerifySet, event.VerifyReplace:
		if st.Pending {
			st.Withdrawn, st.withdrawnBy = st.Label(), st.Version+1
		}
		keep := st.keepsVerifier(e)
		*st = State{On: true, Request: e, Version: st.Version + 1, Verifier: st.Verifier, VerifierSince: st.VerifierSince,
			Withdrawn: st.Withdrawn, Last: e, History: st.History, Late: st.Late,
			verifierModel: st.verifierModel, sent: st.sent, changed: true, withdrawnBy: st.withdrawnBy}
		if keep {
			st.standby = st.Verifier
		} else {
			st.Verifier, st.VerifierSince = "", 0
		}
		resetStops()
		entry.Label = Label{Version: st.Version}
	case event.VerifyClear:
		if st.Pending {
			st.Withdrawn, st.withdrawnBy = st.Label(), 0
		}
		entry.Label = Label{Version: st.Version}
		st.On, st.Request, st.Pending, st.standby, st.changed = false, nil, false, st.Verifier, true
	case event.VerifyVerifying:
		candidate := st.Candidate
		if c, ok := candidateOf(e, attempt); ok {
			candidate = c
		}
		if !st.Pending || strings.TrimSpace(candidate) != strings.TrimSpace(st.Candidate) {
			st.Candidates++
		}
		if e.Verifier != st.Verifier {
			st.VerifierSince, st.retired = st.Version, false
			if st.Request != nil {
				st.verifierModel = st.Request.Model
			}
		}
		st.Verifier, st.Pending, st.Candidate, st.standby, st.changed = e.Verifier, true, candidate, "", false
		st.EndTold = nil
		st.sent = st.Label()
		entry.Label = st.sent
	case event.VerifyRejected:
		entry.Label = st.Label()
		// Only of a candidate with the verifier: one written after a replace or
		// clear that raced it belongs to the request before.
		if st.Pending {
			st.Pending, st.Rejections, st.Late = false, e.Round, nil
			resetStops()
		}
	case event.VerifyPassed, event.VerifyExhausted:
		entry.Label = st.Label()
		st.On, st.Request, st.Pending, st.Late = false, nil, false, nil
		// An end with a reason isn't the rounds: the verifier stopped too often
		// or couldn't start, so the next version doesn't keep it.
		st.retired = e.Action == event.VerifyExhausted && e.Reason != ""
	}
	st.History = append(st.History, entry)
}

// staleVerdict finds the verdict a stale record carrying o is about: the
// latest one before it not yet marked, the same as o if there is one, else
// the latest of o's verifier. -1 if there is none.
func (st *State) staleVerdict(o *event.VerifyOutcome) int {
	if o == nil {
		return -1
	}
	fallback := -1
	for h := len(st.History) - 1; h >= 0; h-- {
		v := st.History[h]
		if v.Action != event.VerifyVerdict || v.Stale || v.Outcome == nil || v.Outcome.Verifier != o.Verifier || v.Outcome.Stopped() {
			continue
		}
		if *v.Outcome == *o {
			return h
		}
		if fallback < 0 {
			fallback = h
		}
	}
	return fallback
}

// keepsVerifier reports whether a new version (set or replace e) keeps the
// verifier: it runs on the model e asks for, and didn't end the last version
// itself.
func (st *State) keepsVerifier(e *event.VerifyEvent) bool {
	return st.Verifier != "" && !st.retired && e.Model == st.verifierModel
}

func stopText(status, reason string) string {
	return status + ": " + strings.Join(strings.Fields(reason), " ")
}

// AwaitsVerdict reports whether a session's verification has a candidate
// with verifier, waiting for its verdict.
func AwaitsVerdict(ctx context.Context, store db.Store, runID, sessionID, verifier string) bool {
	st, err := Load(ctx, store, runID, sessionID)
	return err == nil && st.On && st.Pending && st.Verifier == verifier
}

// Revives reports whether evt, in sessionID's stream, is its verifier's
// verdict on the candidate it has pending: the one outcome record that brings
// back a crashed agent, whose revived turn has it to read.
func Revives(ctx context.Context, store db.Store, runID, sessionID string, evt event.Event) bool {
	v, ok := evt.(*event.VerifyEvent)
	return ok && v.Action == event.VerifyVerdict && v.Outcome != nil && !v.Outcome.Stopped() &&
		AwaitsVerdict(ctx, store, runID, sessionID, v.Outcome.Verifier)
}

// Load derives a session's verification from its stream: from the events
// Derive reads (its verification records, and the messages and sub-agent
// results that tell of its verifier's outcomes), read by type, not from the
// whole stream. A verifying record from before records carried their
// candidate (see candidateOf) also needs the agent's turn before it, so a
// stream with one is read whole.
func Load(ctx context.Context, store db.Store, runID, sessionID string) (State, error) {
	events, err := store.GetEvents(ctx, runID, sessionID, db.EventFilter{Types: loadTypes})
	if err != nil {
		return State{}, fmt.Errorf("read %s's verification: %w", sessionID, err)
	}
	if slices.ContainsFunc(events, withoutCandidate) {
		if events, err = store.GetEvents(ctx, runID, sessionID, db.EventFilter{}); err != nil {
			return State{}, fmt.Errorf("read %s's verification: %w", sessionID, err)
		}
	}
	return Derive(events), nil
}

// loadTypes are the types of the events Derive reads, but for the turns it
// reads only for a verifying record without its candidate.
var loadTypes = db.EventTypes(&event.VerifyEvent{}, &event.UserEvent{}, &event.ChildResultEvent{})

// withoutCandidate reports whether r is a verifying record without the
// candidate it gave, which Derive finds in the turn before it.
func withoutCandidate(r db.EventRecord) bool {
	v, ok := r.Event.(*event.VerifyEvent)
	return ok && v.Action == event.VerifyVerifying && v.Candidate == ""
}

// Status is the state's name for the API.
func (s State) Status() string {
	switch {
	case s.On && s.Pending:
		return StatusVerifying
	case s.On:
		return StatusOn
	case s.Last != nil && s.Last.Action == event.VerifyPassed:
		return StatusPassed
	case s.Last != nil && s.Last.Action == event.VerifyExhausted:
		return StatusNotVerified
	}
	return StatusOff
}

// Round is the round in progress while verification is on, else the round it
// ended in (0 if it ended without one).
func (s State) Round() int {
	if s.On {
		return s.Rejections + 1
	}
	if s.Last != nil {
		return s.Last.Round
	}
	return 0
}

// Exhausted reports whether the rejections so far end the verification.
func (s State) Exhausted() bool {
	return s.Request != nil && s.Request.MaxRounds > 0 && s.Rejections >= s.Request.MaxRounds
}

// MaxStops bounds a verifier's consecutive stops without a verdict: it isn't
// a round, but one that keeps crashing (a model the provider rejects, say)
// must not hold the agent forever.
const MaxStops = 3

// TooManyStops reports whether the verifier has stopped MaxStops times in a
// row without a verdict.
func (s State) TooManyStops() bool { return s.Stops >= MaxStops }

// Verdict is what the verifier has said about the latest candidate.
type Verdict struct {
	// Given: it finished with the latest candidate, i.e. it concluded after
	// receiving it. A verdict on an earlier candidate is not given for this one.
	Given bool
	// Pass and Findings are its verdict: on the latest candidate when Given,
	// on an earlier one when Owed.
	Pass     bool
	Findings string // its text after the verdict word (all of it if it gave none)
	// At is when it concluded or stopped, for a verdict or a stop (see
	// event.VerifyOutcome.At).
	At time.Time
	// Stopped is the verifier's status if it stopped before finishing with the
	// latest candidate (crashed or cancelled); it is then re-engaged.
	Stopped string
	// Owed: it is concluded, but its last final turn came before the latest
	// candidate, which it hasn't read yet: either a goroutine is about to (the
	// candidate revived it), or the candidate reached it while it was
	// concluding and nothing will (see the event loop's engage).
	Owed bool
	// Started: it took a turn after receiving the latest candidate.
	Started bool
	// Reading: it is reading the latest candidate, or will at its next turn
	// without being sent it again: a turn of its began after the candidate
	// arrived (its step moved past the candidate's), or it is running and its
	// latest turn is a working one. A running verifier whose latest turn is a
	// final one may be concluding with it, and miss the candidate.
	Reading bool
	// Missing: the verifier session doesn't exist (it was never created).
	Missing bool
}

// Evaluate reads the verifier's stream for its verdict on the latest
// candidate. The candidates it received are its task (the first) and the
// harness messages that followed (Check verify); a later candidate always
// arrives as one of those, and one arriving while it generates its final turn
// sends it back for another, so a final turn after the latest candidate is a
// verdict on it.
func Evaluate(ctx context.Context, store db.Store, runID string, st State) (Verdict, error) {
	if !st.On || !st.Pending || st.Verifier == "" {
		return Verdict{}, nil
	}
	return Inspect(ctx, store, runID, st.Verifier)
}

// Inspect reads a verifier's stream for where it stands with the latest
// candidate it received (see Evaluate).
func Inspect(ctx context.Context, store db.Store, runID, verifier string) (Verdict, error) {
	sess, err := store.GetSession(ctx, runID, verifier)
	if db.IsNotFound(err) {
		return Verdict{Missing: true}, nil
	}
	if err != nil {
		return Verdict{}, fmt.Errorf("read verifier %s: %w", verifier, err)
	}
	// Its candidates and turns, read by type: its tool results are most of its
	// stream, and say nothing here.
	events, err := store.GetEvents(ctx, runID, verifier, db.EventFilter{
		Types: db.EventTypes(&event.UserEvent{}, &event.AssistantEvent{})})
	if err != nil {
		return Verdict{}, fmt.Errorf("read verifier %s: %w", verifier, err)
	}
	// final is its latest turn offering a result: for a concluded verifier,
	// the one it concluded with.
	lastCandidate, final, turn := -1, -1, -1
	var finalTurn *event.AssistantEvent
	for i, rec := range events {
		switch e := rec.Event.(type) {
		case *event.UserEvent:
			if (rec.Step == 0 && !e.IsHarness()) || e.Check == event.CheckVerify {
				lastCandidate = i
			}
		case *event.AssistantEvent:
			turn = i
			if _, ok := e.Offered(); ok {
				final, finalTurn = i, e
			}
		}
	}
	started := turn > lastCandidate
	running := sess.Status == db.SessionOngoing || sess.Status == db.SessionAwaiting
	reading := started || lastCandidate >= 0 && sess.CurrentStep > events[lastCandidate].Step ||
		running && (turn < 0 || turn > final)
	typed := agent.IsVerifier(sess.AgentType)
	switch {
	case sess.Status == db.SessionConcluded && final > lastCandidate:
		pass, findings := verdictOf(finalTurn, typed)
		return Verdict{Given: true, Pass: pass, Findings: findings, Started: true, Reading: true, At: sess.StatusChangedAt}, nil
	case sess.Status == db.SessionConcluded:
		v := Verdict{Owed: true, Started: started, Reading: reading, At: sess.StatusChangedAt}
		if finalTurn != nil {
			v.Pass, v.Findings = verdictOf(finalTurn, typed)
		}
		return v, nil
	case sess.Status == db.SessionCrashed || sess.Status == db.SessionCancelled:
		return Verdict{Stopped: sess.Status, Started: started, Reading: reading, At: sess.StatusChangedAt}, nil
	}
	return Verdict{Started: started, Reading: reading}, nil
}

// Outcome is the verdict as verifier's outcome record carries it.
func (v Verdict) Outcome(verifier string) *event.VerifyOutcome {
	o := &event.VerifyOutcome{Verifier: verifier, Verdict: event.VerdictFail, Findings: v.Findings, At: v.At}
	if v.Pass {
		o.Verdict = event.VerdictPass
	}
	return o
}

// verdictOf reads the verdict of a verifier's final turn: the typed verdict of
// a verifier's conclude call as given. A verifier of the verifier type that
// replied instead (its fallback after the nudges) gave no verdict: a FAIL,
// whatever the reply says. A verifier of a type without the typed verdict (a
// standard agent, started before the type existed) gave its verdict in the
// first line of its result.
func verdictOf(turn *event.AssistantEvent, typed bool) (pass bool, findings string) {
	if tc, ok := turn.ConcludeCall(); ok {
		if pass, findings, ok := event.VerdictArgs(tc.Arguments); ok {
			return pass, findings
		}
	}
	result, _ := turn.Offered()
	if typed {
		return false, NoVerdictFindings(result)
	}
	return event.ParseVerdict(result)
}

// NoVerdictFindings are the findings of a verifier of the verifier type that
// concluded with a reply instead of a typed verdict: a FAIL labelled
// event.NoVerdict, the reply as the rest.
func NoVerdictFindings(reply string) string {
	if reply = strings.TrimSpace(reply); reply != "" {
		return event.NoVerdict + ": " + reply
	}
	return event.NoVerdict
}
