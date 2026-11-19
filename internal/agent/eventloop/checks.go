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

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/verify"
)

// attempt is an attempt to conclude: a conclude call, or a reply without tool
// calls, which concludes an agent without the tool, or one that doesn't use
// it, as conclude(result=text) would.
type attempt struct {
	result string          // what it offers; "" for conclude() without a result
	call   *event.ToolCall // the conclude call; nil for a reply
	step   int             // the call's turn step, where its result goes
}

// outcome is what the conclusion checks decide about an attempt.
type outcome struct {
	kind outcomeKind
	// text is what the agent reads (a reply's harness message adds the
	// "[system] " prefix): a held attempt's answer, the note for a reply that
	// waits, or the result of the conclude call that concludes.
	text     string
	check    string // holds: the check that answered
	isError  bool   // holds: the call itself was wrong
	verifier string // waits: who has the candidate
	// told is what the verifier said, when the answer (or the conclusion's
	// call result) tells the agent: a verdict is told once (see noteVerdict).
	told *event.VerifyOutcome
	// records go with the answer, written with it in one transaction (a
	// rejection's record, so a counted round is never left untold).
	records []event.Event
	// aside is a note written before any wait, which it doesn't end: a verdict
	// on an earlier candidate, told as information.
	aside *event.UserEvent
	// concludes: the result delivered (the attempt's, or the candidate the
	// verifier passed), notes for the parent's child_result, a record written
	// with it, and why a verification ended unverified (an end* reason, for
	// the log). again says what calling conclude again does, for a call that
	// can't conclude yet after all (see runConcludeCall).
	result string
	notes  []event.ResultNote
	record *event.VerifyEvent
	ended  string
	again  string
	// quiet: it concludes without a word to its parent (a verifier standing
	// by: see standsBy).
	quiet bool
	// version is the verification's version a verification outcome (one with
	// a record) was decided in (see finish).
	version int
}

type outcomeKind int

const (
	concludes outcomeKind = iota // the session concludes
	holds                        // the attempt is answered (text), and the agent goes on
	waits                        // the candidate is with the verifier
)

func held(text, check string) outcome { return outcome{kind: holds, text: text, check: check} }

// nothingPending answers conclude() with no result when no conclusion of the
// agent's is waiting for a verdict.
const nothingPending = "Error: no conclusion of yours is waiting for a verdict, so there is nothing to deliver. Call " +
	"conclude(result) with your full result."

// concludeAgain is what calling conclude again does after an attempt that
// would have concluded directly, in this agent's form of the call
// (resultAgain for an agent that concludes with a result).
func (a *EventLoopAgent) concludeAgain() string {
	if a.givesVerdict() {
		return "Call conclude(verdict, findings) again to deliver your verdict."
	}
	return resultAgain
}

const resultAgain = "Call conclude(result) again to conclude."

// conclusionCheck runs the checks for an attempt to conclude, given the
// context its turn answered: work still running (leftovers.go), then
// verification (verify.go). Each lets the conclusion through or holds it;
// verification may instead make it wait for the verifier's verdict. A reply
// without tool calls while a candidate is pending is never a candidate: it
// gets a nudge (see pendingNudges), and past the nudges it is taken as
// conclude(), a wait for the pending candidate. A verifier whose candidate
// was withdrawn stands by instead (standsBy).
func (a *EventLoopAgent) conclusionCheck(ctx context.Context, events []db.EventRecord, at attempt) (outcome, error) {
	if err := ctx.Err(); err != nil {
		return outcome{}, err // shutting down: leave the attempt to the recovery
	}
	hist, err := a.requestHistory(ctx, events)
	if err != nil {
		return outcome{}, err
	}
	if standsBy(hist) {
		if w := a.runningWork(ctx); !w.empty() && !justReminded(events) {
			return held(w.reminder(at.call != nil), event.CheckLeftovers), nil
		}
		return outcome{kind: concludes, quiet: true, result: at.result, text: verify.StandingBy,
			again: a.concludeAgain()}, nil
	}
	if again, told := redelivered(events, at.result); again && a.isVerifier(ctx) {
		if told >= maxPendingNudges {
			// One that keeps at it stops without a verdict, as a crash does,
			// rather than hold the agent forever: the agent re-engages it, and
			// repeated stops end the verification.
			return outcome{}, errRedelivering
		}
		return held(verify.Redelivered, event.CheckVerify), nil
	}
	// Read outside the verification lock for what doesn't change the
	// verification (the reply rules, an empty conclude(), the reminder, whose
	// process scan needn't hold up a set or clear); verifyCheck reads it again
	// under the lock.
	st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
	if err != nil {
		return outcome{}, err
	}
	pending := st.On && st.Pending
	// A reply isn't a conclusion while one is pending, nor ever a verifier's
	// verdict, which its conclude call types.
	if at.call == nil && (pending || a.givesVerdict()) {
		if n := pendingNudges(events); n < maxPendingNudges {
			if !pending {
				return held(verify.ReplyIsNoVerdict, event.CheckConclude), nil
			}
			v, err := verify.Evaluate(ctx, a.env.Store, a.env.RunID, st)
			if err != nil {
				return outcome{}, err
			}
			return held(verify.ReplyWhilePending(st.Verifier, verify.At(st, st.Rejections+1), v.Given && v.Pass, v.Stopped),
				event.CheckConclude), nil
		}
		if pending {
			// A model that keeps replying instead of calling conclude: after
			// maxPendingNudges in a row its reply is taken as conclude(), so it
			// waits for (or delivers) the pending candidate, which a reply never
			// replaces, rather than taking turn after turn.
			a.logger.Debug("replies while a conclusion is pending; waiting for its verdict", "nudges", maxPendingNudges)
			at.result = ""
		} else {
			// A verifier that keeps replying: after maxPendingNudges in a row its
			// reply concludes it with no verdict, a FAIL whatever the reply says,
			// so it can't hold the agent forever, nor pass on words it wouldn't
			// type.
			a.logger.Debug("a verifier replies instead of calling conclude; concluding it without a verdict", "nudges", maxPendingNudges)
			at.result = event.FormatVerdict(false, verify.NoVerdictFindings(at.result))
		}
	}
	if nothingToWaitFor(at, st) {
		return outcome{kind: holds, text: nothingPending, isError: true}, nil
	}
	if !justReminded(events) {
		if w := a.runningWork(ctx); !w.empty() {
			return held(w.reminder(at.call != nil), event.CheckLeftovers), nil
		}
	}
	// The lock serializes the verification steps with a concurrent set or
	// clear, which sees either none of them or all.
	defer verify.Lock(a.env.RunID, a.cfg.SessionID)()
	if st, err = verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID); err != nil {
		return outcome{}, err
	}
	switch {
	case nothingToWaitFor(at, st):
		return outcome{kind: holds, text: nothingPending, isError: true}, nil // it changed meanwhile
	case !st.On:
		// A verdict for a candidate withdrawn since is stale.
		if err := a.recordStale(ctx, st); err != nil {
			return outcome{}, err
		}
		return outcome{kind: concludes, result: at.result, text: "Concluded.", again: a.concludeAgain()}, nil
	}
	return a.verifyCheck(ctx, hist, st, at)
}

// standsBy reports whether this agent is a verifier whose latest candidate
// was withdrawn (a withdrawal came after it in hist, the requestHistory of its
// turn): its verdict wouldn't count, so whatever it ends its turn with
// delivers nothing, and it concludes quietly until the next candidate, which
// revives it with what it has learned.
func standsBy(hist []db.EventRecord) bool {
	for i := len(hist) - 1; i >= 0; i-- {
		if u, ok := hist[i].Event.(*event.UserEvent); ok {
			switch u.Check {
			case event.CheckWithdrawn:
				return true
			case event.CheckVerify:
				return false
			}
			if hist[i].Step == 0 && !u.IsHarness() {
				return false // its task: the first candidate
			}
		}
	}
	return false
}

var errRedelivering = errors.New("the verifier kept giving its verdict on an earlier candidate instead of checking the new one")

// redelivered reports whether a verifier's attempt (result) gives again,
// straight away, the verdict of its conclude call that a new candidate
// overtook (answered by verifierHeld): that verdict was on the earlier
// candidate. told counts the times it was told so since (Redelivered). An
// attempt after a turn of work isn't one: the verifier checked something.
func redelivered(events []db.EventRecord, result string) (again bool, told int) {
	answered := map[string]bool{} // conclude calls a verification hold answered
	candidate := false
	for i := len(events) - 1; i >= 0; i-- {
		switch e := events[i].Event.(type) {
		case *event.ToolResultEvent:
			if e.Check == event.CheckVerify {
				answered[e.ToolCallID] = true
				if !candidate {
					told++
				}
			}
		case *event.UserEvent:
			if e.Check == event.CheckVerify || (events[i].Step == 0 && !e.IsHarness()) {
				if candidate {
					return false, told
				}
				candidate = true
			}
		case *event.AssistantEvent:
			tc, ok := e.ConcludeCall()
			if !ok || !answered[tc.ID] {
				return false, told // a turn of work, or a call not held over a candidate
			}
			if candidate {
				prev, _ := e.Offered()
				return sameResult(prev, result), told
			}
		}
	}
	return false, told
}

// isVerifier reports whether this agent is a verifier the harness started,
// from its session (a revived verifier's config doesn't say).
func (a *EventLoopAgent) isVerifier(ctx context.Context) bool {
	if a.cfg.VerifierFor != "" {
		return true
	}
	s, err := a.env.Store.GetSession(ctx, a.env.RunID, a.cfg.SessionID)
	return err == nil && verify.VerifierFor(s) != ""
}

// nothingToWaitFor reports an attempt without a result (conclude()) when no
// candidate of the agent's is pending: there is nothing to deliver.
func nothingToWaitFor(at attempt, st verify.State) bool {
	return sameResult(at.result, "") && (!st.On || !st.Pending)
}

// sameResult reports whether two results are the same conclusion.
func sameResult(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// maxPendingNudges bounds the nudges for replies without tool calls while a
// conclusion awaits its verdict, or from a verifier (see conclusionCheck).
const maxPendingNudges = 2

// pendingNudges counts the nudges for replies that weren't taken as a
// conclusion, back to the agent's last turn with tool calls or the last
// message about its verification (a submission, a wait, a verdict; for a
// verifier, a candidate): the run of them its reply would extend. From the
// stream, so a restart doesn't reset it.
func pendingNudges(events []db.EventRecord) int {
	n := 0
	for i := len(events) - 1; i >= 0; i-- {
		switch e := events[i].Event.(type) {
		case *event.AssistantEvent:
			if len(e.ToolCalls) > 0 {
				return n
			}
		case *event.UserEvent:
			switch e.Check {
			case event.CheckConclude:
				n++
			case event.CheckVerify:
				return n
			}
		}
	}
	return n
}
