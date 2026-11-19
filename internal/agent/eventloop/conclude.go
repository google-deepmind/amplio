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
	"strings"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/tool"
	"amplio/internal/verify"
)

// The two ways an autonomous agent tries to conclude: a conclude call, and a
// reply without tool calls. checks.go decides what happens to either.

// concludeCallOf returns the call of a turn that calls conclude and nothing
// else, for an agent that has the tool. Alongside other calls it fails on its
// own (it is exclusive), and they run.
func (a *EventLoopAgent) concludeCallOf(calls []llm.ToolCall, tools map[string]*tool.Tool) (event.ToolCall, bool) {
	if a.cfg.Interactive || len(calls) != 1 || calls[0].Name != event.ConcludeTool || tools[event.ConcludeTool] == nil {
		return event.ToolCall{}, false
	}
	return event.ToolCall{ID: calls[0].ID, Name: calls[0].Name, Arguments: calls[0].Arguments}, true
}

// concludeCall runs a conclude call just made at step (runConcludeCall),
// unless input the agent hasn't read arrived while its turn was generated (at
// step+1 or later, beyond its verifier's results). The call then returns
// saying what came before any check runs, as an overtaken reply takes another
// turn: nothing it offers is delivered or given to a verifier, and no verdict
// is acted on, before that input is read.
func (a *EventLoopAgent) concludeCall(ctx context.Context, events []db.EventRecord, call event.ToolCall, step int) (done bool, err error) {
	unread, err := a.unreadSince(ctx, step+1, nil)
	if err != nil {
		return false, err
	}
	if len(unread) == 0 {
		return a.runConcludeCall(ctx, events, call, step)
	}
	out, err := a.overtaken(ctx, events, unread)
	if err != nil {
		return false, err
	}
	return false, a.env.Store.AppendEventAtStep(ctx, a.env.RunID, a.cfg.SessionID, step,
		&event.ToolResultEvent{ToolCallID: call.ID, Content: out.text, Check: out.check})
}

// overtaken answers an attempt to conclude that unread input overtook before
// any check ran (see concludeCall); events is the context its turn answered.
func (a *EventLoopAgent) overtaken(ctx context.Context, events, unread []db.EventRecord) (outcome, error) {
	if a.isVerifier(ctx) {
		hist, err := a.requestHistory(ctx, events)
		if err != nil {
			return outcome{}, err
		}
		return verifierHeld(hist, unread, a.concludeAgain()), nil
	}
	st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
	if err != nil {
		return outcome{}, err
	}
	if !st.On {
		return held("Not concluded yet: this arrived meanwhile: "+describeArrivals(unread)+". "+resultAgain, ""), nil
	}
	return held(verify.Overtaken(describeArrivals(unread), st), ""), nil
}

// verifierHeld answers a verifier's attempt to conclude over unread input
// (again: what concluding again does); hist is its turn's requestHistory.
// When the latest candidate message among it withdraws its
// candidate, or gives a new one, the verdict was on the earlier one: it is
// told so, and to stand by or check the new one, never invited to deliver that
// verdict again (which would count for the new candidate); the answer over a
// new candidate is marked (its Check) for redelivered.
func verifierHeld(hist, unread []db.EventRecord, again string) outcome {
	var others []db.EventRecord
	var last *event.UserEvent // the latest candidate or withdrawal message
	for _, r := range unread {
		if u, ok := r.Event.(*event.UserEvent); ok && (u.Check == event.CheckVerify || u.Check == event.CheckWithdrawn) {
			last = u
			continue
		}
		others = append(others, r)
	}
	with := func(what string) string {
		if len(others) > 0 {
			return what + "; " + describeArrivals(others)
		}
		return what
	}
	switch {
	case last != nil && last.Check == event.CheckWithdrawn:
		return held(verify.VerdictWithdrawn(with("the withdrawal of your candidate")), "")
	case last != nil && !verify.SameCandidate(latestCandidate(hist), last.Content):
		return held(verify.VerdictOnEarlier(with("a new candidate")), event.CheckVerify)
	}
	return held("Not concluded yet: this arrived meanwhile: "+describeArrivals(unread)+". "+again, "")
}

// latestCandidate is the latest message in a verifier's requestHistory (hist)
// that gave it a candidate (its task, or a message re-engaging it): what a
// verdict it gave next is on.
func latestCandidate(hist []db.EventRecord) string {
	for i := len(hist) - 1; i >= 0; i-- {
		if u, ok := hist[i].Event.(*event.UserEvent); ok && (u.Check == event.CheckVerify || hist[i].Step == 0 && !u.IsHarness()) {
			return u.Content
		}
	}
	return ""
}

// runConcludeCall runs a conclude call made at step, or resumed there in a
// later life of the session: the checks decide, and a call that waits for a
// verdict waits until something arrives, then is settled. Its result is
// written at step, atomically with the conclusion if it concludes. events is
// the context the call's turn answered. done: the session is finished (it
// concluded, or it was stopped while waiting, leaving the call open for the
// next life).
func (a *EventLoopAgent) runConcludeCall(ctx context.Context, events []db.EventRecord, call event.ToolCall, step int) (done bool, err error) {
	at := attempt{result: event.ConcludeResult(call.Arguments), call: &call, step: step}
	settled := map[string]bool{} // what a wait woke on that settle found nothing in
	var out outcome
	bad := a.badConcludeArgs(call)
	if bad != "" {
		hist, herr := a.requestHistory(ctx, events)
		if herr != nil {
			return false, herr
		}
		if standsBy(hist) {
			bad = ""
		}
	}
	if bad != "" {
		out = outcome{kind: holds, text: bad, isError: true}
	} else {
		out, err = a.conclusionCheck(ctx, events, at)
	}
	if err == nil {
		err = a.writeAside(ctx, out, settled)
	}
	for err == nil && out.kind == waits {
		a.logger.Debug("conclude waits for the verdict", "verifier", out.verifier)
		var arrived []db.EventRecord
		var stopped bool
		arrived, stopped, err = a.waitForVerdict(ctx, step+1, settled)
		if ctx.Err() != nil || stopped {
			return true, nil // cancelled, or shutting down: the status is the canceller's, or the recovery's
		}
		if err == nil {
			out, err = a.settle(ctx, arrived)
		}
		if out.kind == waits {
			for _, r := range arrived {
				settled[r.EventID] = true
			}
		}
	}
	if err != nil {
		return false, err
	}
	out, concluded, err := a.finish(ctx, events, out, at, step+1, settled)
	if err != nil || concluded {
		return concluded, err
	}
	answer := &event.ToolResultEvent{ToolCallID: call.ID, Content: out.text, IsError: out.isError, Check: out.check, Outcome: out.told}
	return false, a.writeAnswer(ctx, out, answer, step)
}

// finish carries out an attempt's outcome once any wait is over: it
// concludes, or comes back as the answer to write. A conclusion over input
// that arrived since step (beyond settled) is held (notOverUnread), and an
// outcome told before isn't told again (untold). A verification's outcome
// (passed or ended) was decided under the verification lock, which was
// released since; finish takes it again and holds it through the terminal
// write, so a new version or a clear in between withdraws the candidate
// instead of letting the old version's outcome deliver it or end it.
func (a *EventLoopAgent) finish(ctx context.Context, events []db.EventRecord, out outcome, at attempt, step int, settled map[string]bool) (outcome, bool, error) {
	if out.kind == concludes && out.record != nil {
		defer verify.Lock(a.env.RunID, a.cfg.SessionID)()
		st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
		if err != nil {
			return out, false, err
		}
		if !st.On || st.Version != out.version {
			out = withdrawnOutcome(st, verify.Label{Version: out.version})
		}
	}
	out, err := a.notOverUnread(ctx, events, out, step, settled)
	if err != nil {
		return out, false, err
	}
	if out, err = a.untold(ctx, out); err != nil {
		return out, false, err
	}
	if out.kind == concludes {
		return out, true, a.conclude(out, at)
	}
	return out, false, nil
}

// writeAnswer writes an attempt's answer at step (negative: the current one),
// with out's records at the current step, in one transaction.
func (a *EventLoopAgent) writeAnswer(ctx context.Context, out outcome, answer event.Event, step int) error {
	evts := make([]db.SelfEvent, 0, len(out.records)+1)
	for _, r := range out.records {
		evts = append(evts, db.SelfEvent{Event: r, Step: -1})
	}
	return a.env.Store.AppendEvents(ctx, a.env.RunID, a.cfg.SessionID, append(evts, db.SelfEvent{Event: answer, Step: step})...)
}

// givesVerdict reports whether this agent is a verifier, whose conclude takes
// a typed verdict instead of a result.
func (a *EventLoopAgent) givesVerdict() bool { return agent.IsVerifier(a.cfg.AgentType) }

// badConcludeArgs is the error a conclude call gets when its arguments aren't
// this agent's form: a verifier's needs a verdict (a FAIL with findings) and no
// result; anyone else's takes no verdict. "" for a call in its form. Refused,
// a call never concludes, so neither form silently stands in for the other.
func (a *EventLoopAgent) badConcludeArgs(call event.ToolCall) string {
	var keys map[string]json.RawMessage
	_ = json.Unmarshal([]byte(call.Arguments), &keys)
	_, hasResult := keys["result"]
	_, hasVerdict := keys["verdict"]
	_, hasFindings := keys["findings"]
	if !a.givesVerdict() {
		if hasVerdict || hasFindings {
			return "Error: conclude takes only `result`, your final result; `verdict` and `findings` are a verifier's."
		}
		return ""
	}
	pass, findings, ok := event.VerdictArgs(call.Arguments)
	switch {
	case hasResult:
		return "Error: give only `verdict` and `findings`; a verifier's conclude takes no `result`."
	case !ok:
		return `Error: give your verdict: conclude(verdict: "PASS" or "FAIL", findings).`
	case !pass && findings == "":
		return "Error: a FAIL needs findings: say concretely what is missing or wrong, for the agent to fix."
	}
	return ""
}

// writeAside writes out's note that doesn't answer the attempt (a verdict on
// an earlier candidate), marking it settled so it doesn't end a wait.
func (a *EventLoopAgent) writeAside(ctx context.Context, out outcome, settled map[string]bool) error {
	if out.aside == nil {
		return nil
	}
	id, err := a.env.Store.AppendEvent(ctx, a.env.RunID, a.cfg.SessionID, out.aside)
	settled[id] = true
	return err
}

// untold drops out's outcome when the agent was told it already (a verify
// message, then this attempt settling the same verdict): it is told once.
func (a *EventLoopAgent) untold(ctx context.Context, out outcome) (outcome, error) {
	if out.told == nil {
		return out, nil
	}
	told, err := a.toldBefore(ctx, out.told)
	if told {
		out.told = nil
	}
	return out, err
}

// toldBefore reports whether the agent's stream already tells o: a call result
// or message carrying the same outcome of the same verifier (the same
// terminal transition, At).
func (a *EventLoopAgent) toldBefore(ctx context.Context, o *event.VerifyOutcome) (bool, error) {
	if o.At.IsZero() {
		return false, nil
	}
	recs, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID, db.EventFilter{})
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		var told *event.VerifyOutcome
		switch e := r.Event.(type) {
		case *event.UserEvent:
			told = e.Outcome
		case *event.ToolResultEvent:
			told = e.Outcome
		}
		if told != nil && told.Verifier == o.Verifier && told.Verdict == o.Verdict && told.At.Equal(o.At) {
			return true, nil
		}
	}
	return false, nil
}

// notOverUnread turns a conclusion into an answer when input the agent hasn't
// read arrived at step or later (beyond its verifier's results and the events
// in settled): a concluded session would never act on it. events is the
// context the attempt's turn answered.
func (a *EventLoopAgent) notOverUnread(ctx context.Context, events []db.EventRecord, out outcome, step int, settled map[string]bool) (outcome, error) {
	if out.kind != concludes {
		return out, nil
	}
	unread, err := a.unreadSince(ctx, step, settled)
	if err != nil || len(unread) == 0 {
		return out, err
	}
	if a.isVerifier(ctx) {
		hist, err := a.requestHistory(ctx, events)
		if err != nil {
			return outcome{}, err
		}
		return verifierHeld(hist, unread, out.again), nil
	}
	check := ""
	if out.record != nil {
		check = event.CheckVerify
	}
	answer := held("Not concluded yet: this arrived meanwhile: "+describeArrivals(unread)+". "+out.again, check)
	answer.told = out.told
	return answer, nil
}

// resumeConclude resumes a conclude call made at step that an earlier life of
// the session left open. Run again, it finds its candidate with the verifier
// and waits on, or settles a verdict that came meanwhile; its result then
// completes the step.
func (a *EventLoopAgent) resumeConclude(ctx context.Context, call event.ToolCall, step int) (done bool, err error) {
	a.callStep = step
	events, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID,
		db.EventFilter{CurrentContextOnly: true, EndStep: &step})
	if err != nil {
		return false, err
	}
	// The context the call's turn answered: what came before it.
	for i, r := range events {
		if ae, ok := r.Event.(*event.AssistantEvent); ok {
			if tc, ok := ae.ConcludeCall(); ok && tc.ID == call.ID {
				events = events[:i]
				break
			}
		}
	}
	withdrawn, superseded, err := a.supersededSince(ctx, call.ID)
	if err != nil {
		return false, err
	}
	if reason, cancelled, err := a.cancelledSince(ctx, step+1); err != nil {
		return false, err
	} else if superseded {
		// A newer version or a clear came after it: its candidate, made for
		// the version then current, is withdrawn, and not given to the new one.
		if err := a.env.Store.AppendEventAtStep(ctx, a.env.RunID, a.cfg.SessionID, step, &event.ToolResultEvent{
			ToolCallID: call.ID, Content: withdrawn, Check: event.CheckVerify}); err != nil {
			return false, err
		}
	} else if cancelled {
		// A cancel withdrew it: answered as such, the checks not run again, so
		// a verifier the cancel stopped stays stopped.
		st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
		if err != nil {
			return false, err
		}
		if err := a.env.Store.AppendEventAtStep(ctx, a.env.RunID, a.cfg.SessionID, step, &event.ToolResultEvent{
			ToolCallID: call.ID, Content: cancelledCall(reason, st.On && st.Pending, a.givesVerdict())}); err != nil {
			return false, err
		}
	} else if done, err = a.runConcludeCall(ctx, db.KnownEvents(events), call, step); err != nil || done {
		return done, err
	}
	return false, a.env.Store.MarkStepFinalized(ctx, a.env.RunID, a.cfg.SessionID, step)
}

// supersededSince reports whether the candidate of the conclude call callID
// was withdrawn by a set, replace or clear after the call in the stream, and
// if so the call's answer. Not when the candidate went to the verifier after
// that change (a change that landed before the attempt took the verification
// lock, which made it the new version's candidate).
func (a *EventLoopAgent) supersededSince(ctx context.Context, callID string) (string, bool, error) {
	events, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID, db.EventFilter{})
	if err != nil {
		return "", false, err
	}
	// version: the one current at the call; changed: a change since it that no
	// verifying record (the call's candidate given to the verifier) followed.
	version, called, changed := 0, false, false
	for _, r := range events {
		switch e := r.Event.(type) {
		case *event.AssistantEvent:
			if tc, ok := e.ConcludeCall(); ok && tc.ID == callID {
				called = true
			}
		case *event.VerifyEvent:
			switch e.Action {
			case event.VerifySet, event.VerifyReplace:
				if called {
					changed = true
				} else {
					version++
				}
			case event.VerifyClear:
				changed = changed || called
			case event.VerifyVerifying:
				if called {
					changed = false
				}
			}
		}
	}
	if !changed {
		return "", false, nil
	}
	return withdrawnOutcome(verify.Derive(events), verify.Label{Version: version}).text, true, nil
}

// cancelledCall answers a conclude call its session was cancelled in;
// pending: its candidate is still the verifier's to check, once re-engaged;
// verdict: the session is a verifier, which concludes with a verdict.
func cancelledCall(reason string, pending, verdict bool) string {
	next := "Call conclude(result) to conclude."
	switch {
	case verdict:
		next = "Call conclude(verdict, findings) to deliver your verdict."
	case pending:
		next = "Call conclude() to give your conclusion to the verifier again, or conclude(result) to submit a new one."
	}
	return fmt.Sprintf("Not concluded: your session was cancelled (%s) while this call was open, so your conclusion "+
		"was withdrawn and nothing was delivered. %s", reason, next)
}

// cancelledSince reports whether the session was cancelled at step or later
// (the cancel's marker), with the reason.
func (a *EventLoopAgent) cancelledSince(ctx context.Context, step int) (string, bool, error) {
	recs, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID, db.EventFilter{StartStep: &step})
	if err != nil {
		return "", false, err
	}
	for _, r := range recs {
		if m, ok := r.Event.(*event.SystemEvent); ok && m.Marker == event.MarkerCancelled {
			return m.Content, true, nil
		}
	}
	return "", false, nil
}

// waitForVerdict parks the agent as awaiting, with no timeout, until
// something that wakes it lands at step or later (its verifier's outcome, or
// anything else), as in await_event, and returns what arrived. Events in
// settled (by id) were found to say nothing new already: they don't end the
// wait, and aren't returned. stopped: the session was cancelled meanwhile.
func (a *EventLoopAgent) waitForVerdict(ctx context.Context, step int, settled map[string]bool) (arrived []db.EventRecord, stopped bool, err error) {
	parked := false
	for {
		// Snapshot the counter before the store check, so a notify in between
		// still wakes the wait.
		before := a.handle.Counter()
		if arrived, err = a.arrivalsSince(ctx, step, settled); err != nil || len(arrived) > 0 {
			break
		}
		if !parked {
			// Never over a cancel that landed meanwhile.
			set, err := a.env.Store.SetStatusUnlessTerminal(ctx, a.env.RunID, a.cfg.SessionID, db.SessionAwaiting)
			if err != nil || !set {
				return nil, !set, err
			}
			parked = true
		}
		if _, err := a.handle.WaitAfter(ctx, before, 0); err != nil {
			return nil, false, err
		}
	}
	if err != nil || !parked {
		return arrived, false, err
	}
	set, err := a.env.Store.SetStatusUnlessTerminal(ctx, a.env.RunID, a.cfg.SessionID, db.SessionOngoing)
	return arrived, err == nil && !set, err
}

// arrivalsSince is what arrived at step or later that wakes the agent,
// except the events in settled.
func (a *EventLoopAgent) arrivalsSince(ctx context.Context, step int, settled map[string]bool) ([]db.EventRecord, error) {
	recs, err := db.WakingEvents(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID, step)
	if err != nil {
		return nil, err
	}
	out := recs[:0]
	for _, r := range recs {
		if !settled[r.EventID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// unreadSince is what arrived at step or later that wakes the agent, other
// than its verifier's outcomes, which a wait for the verdict settles itself,
// and the events in settled.
func (a *EventLoopAgent) unreadSince(ctx context.Context, step int, settled map[string]bool) ([]db.EventRecord, error) {
	arrived, err := a.arrivalsSince(ctx, step, settled)
	if err != nil {
		return nil, err
	}
	return a.unreadIn(ctx, arrived), nil
}

func (a *EventLoopAgent) unreadIn(ctx context.Context, arrived []db.EventRecord) []db.EventRecord {
	var out []db.EventRecord
	for _, r := range arrived {
		switch e := r.Event.(type) {
		case *event.VerifyEvent:
			if e.Action == event.VerifyVerdict {
				continue
			}
		case *event.ChildResultEvent:
			// How a verifier's outcome arrived before verifiers posted their own.
			if a.isOwnVerifier(ctx, e.ChildSessionID) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// maxArrivalsNamed bounds what describeArrivals lists.
const maxArrivalsNamed = 4

// describeArrivals says briefly what arrived, for a conclude call that
// returns because of it; the events themselves are in the agent's context.
func describeArrivals(recs []db.EventRecord) string {
	var parts []string
	seen := map[string]bool{}
	for _, r := range recs {
		var d string
		switch e := r.Event.(type) {
		case *event.UserEvent:
			switch {
			case e.Loop != nil:
				d = "a tick of loop " + e.Loop.ID
			case e.IsHarness():
				d = "a note from amplio"
			default:
				d = "a message from the operator"
			}
		case *event.MessageEvent:
			d = "a message from " + e.Sender
			if e.SenderType == event.SenderTypeEnvironment {
				d = "a notification from " + e.Sender
			}
		case *event.ChildResultEvent:
			d = fmt.Sprintf("the result of %s (%s)", e.ChildSessionID, e.Verdict)
		case *event.RecoverEvent:
			d = "a restart marker"
		case *event.VerifyEvent:
			d = "a change to your verification"
			if e.Action == event.VerifyWithdrawn {
				d = "the withdrawal of your conclusion (" + e.Reason + ")"
			}
		case *event.SystemEvent:
			d = "a note from amplio"
			if e.Marker == event.MarkerCancelled {
				d = "your session's cancellation (" + e.Content + ")"
			}
		default:
			d = "a " + r.Event.EventType() + " event"
		}
		if !seen[d] {
			seen[d] = true
			parts = append(parts, d)
		}
	}
	if len(parts) > maxArrivalsNamed {
		parts = append(parts[:maxArrivalsNamed], fmt.Sprintf("and %d more", len(parts)-maxArrivalsNamed))
	}
	if len(parts) == 0 {
		return "an event"
	}
	return strings.Join(parts, "; ")
}

// plainReply takes a reply without tool calls from an autonomous agent, made
// at step, as an attempt to conclude with its text (checks.go); events is the
// context it answered. A reply has no call to wait in, so when the candidate
// is with the verifier the harness waits for the verdict as conclude() would,
// after a note saying so. Unless it concludes, the agent gets a harness
// message, and goes on. done: it concluded, or was stopped while waiting.
func (a *EventLoopAgent) plainReply(ctx context.Context, events []db.EventRecord, text string, step int) (done bool, err error) {
	at := attempt{result: text, step: step}
	out, err := a.conclusionCheck(ctx, events, at)
	if err != nil {
		return false, err
	}
	settled := map[string]bool{} // the notes, and what a wait woke on that settle found nothing in
	if err := a.writeAside(ctx, out, settled); err != nil {
		return false, err
	}
	if out.kind == waits {
		id, err := a.env.Store.AppendEvent(ctx, a.env.RunID, a.cfg.SessionID, verifyMessage(out.text, nil))
		if err != nil {
			return false, err
		}
		settled[id] = true
	}
	for out.kind == waits {
		arrived, stopped, err := a.waitForVerdict(ctx, step+1, settled)
		if ctx.Err() != nil || stopped {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if out, err = a.settle(ctx, arrived); err != nil {
			return false, err
		}
		if out.kind == waits {
			for _, r := range arrived {
				settled[r.EventID] = true
			}
		}
	}
	out, concluded, err := a.finish(ctx, events, out, attempt{}, step+1, settled)
	if err != nil || concluded {
		return concluded, err
	}
	return false, a.writeAnswer(ctx, out, &event.UserEvent{Content: harness(out.text), Check: out.check, Outcome: out.told}, -1)
}

// harness marks text as a harness message, as every harness nudge is.
func harness(text string) string { return "[system] " + text }

// conclude ends an autonomous agent with out's result. It cascade-cancels any
// owned children, then atomically sets concluded and posts the result to the
// parent (unless out is quiet), with out's notes and record and, when a
// conclude call (at.call) concludes it, that call's result (out.text). Uses a
// background context so a late ctx cancellation can't tear down the terminal
// write.
func (a *EventLoopAgent) conclude(out outcome, at attempt) error {
	// What it leaves running rides on the parent's child_result (not in the
	// result text, which is the agent's own words).
	parent := a.cfg.ParentID
	if out.quiet {
		parent = ""
	}
	var notes []event.ResultNote
	if parent != "" {
		notes = a.runningWork(context.Background()).notes()
	}
	notes = append(notes, out.notes...)
	var self []db.SelfEvent
	if at.call != nil {
		self = append(self, db.SelfEvent{Event: &event.ToolResultEvent{ToolCallID: at.call.ID, Content: out.text,
			Outcome: out.told}, Step: at.step})
	}
	if out.record != nil {
		self = append(self, db.SelfEvent{Event: out.record, Step: -1})
	}
	a.cascadeCancelChildren("parent " + a.cfg.SessionID + " concluded")
	concluded, err := a.env.Store.TerminateAndNotifyParent(context.Background(), a.env.RunID, a.cfg.SessionID,
		parent, db.SessionConcluded, out.result, notes, self...)
	if err != nil || !concluded {
		return err // !concluded: already stopped (a cancel won the race), so nothing was delivered
	}
	if at.call != nil {
		if err := a.env.Store.MarkStepFinalized(context.Background(), a.env.RunID, a.cfg.SessionID, at.step); err != nil {
			a.logger.Warn("finalize the concluding step", "step", at.step, "error", err)
		}
	}
	if out.record != nil {
		a.logVerifyOutcome(out)
	}
	return nil
}
