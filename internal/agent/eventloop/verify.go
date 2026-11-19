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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/tool/spawn"
	"amplio/internal/verify"
)

// Verification, the second conclusion check (see internal/verify for the
// state and the rules' vocabulary). While it is on, an attempt to conclude
// gives its result to a verifier sub-agent the harness starts (or re-engages
// with a message) as the candidate, and then waits for the verdict (inside the
// conclude call, or, for a reply, in the harness). A verdict counts only for
// the latest candidate of the current version; one for a version that is
// over is recorded as stale (verify.RecordStale). The verifier's outcome
// reaches the agent's stream as a record that wakes it (a VerifyEvent verdict, not shown to the model), and the
// agent reads only what the harness tells it: the wait settles the verdict it
// waited for; one that arrives while the agent works, or waits in await_event,
// is told at its next turn (noteVerdict), and settled at its next attempt to
// conclude.

// verifierAgentType is the agent type of the verifiers the harness starts; a
// var so tests can stand one in.
var verifierAgentType = verify.AgentType

// verifyCheck is the verification check for an attempt to conclude while
// verification is on (st, read under the session's verification lock, which
// the caller holds). events is the context the attempt answered. It records
// its own VerifyEvents; they wake no one.
func (a *EventLoopAgent) verifyCheck(ctx context.Context, hist []db.EventRecord, st verify.State, at attempt) (outcome, error) {
	v, err := verify.Evaluate(ctx, a.env.Store, a.env.RunID, st)
	if err != nil {
		return outcome{}, err
	}
	candidate := at.result
	if sameResult(candidate, "") {
		candidate = st.Candidate
	}
	if st.EndTold != nil {
		return toldEnd(st, candidate, st.EndTold), nil
	}
	// fresh: a new candidate, not the one the verifier has.
	fresh := !st.Pending || !sameResult(candidate, st.Candidate)
	lab := st.Label()
	if fresh {
		lab = st.Next()
	}
	if !st.Pending {
		if err := a.recordStale(ctx, st); err != nil {
			return outcome{}, err
		}
	}
	// A rejection the agent was told counts whatever it submits now. Only the
	// last allowed one can still be pending once told (the conclusion records
	// it): the verification ended, and a new result concludes, not verified, as
	// the message said.
	told := false
	if v.Given && !v.Pass && fresh {
		if told, err = a.toldBefore(ctx, toldVerdict(st, v, st.Rejections+1)); err != nil {
			return outcome{}, err
		}
	}
	if v.Given && (!fresh || told) {
		if v.Pass {
			return passedOutcome(st, v), nil
		}
		st.Rejections++
		if st.Exhausted() {
			return roundsOutcome(st, candidate, v), nil
		}
		return rejectedOutcome(st, v), nil
	}
	if v.Stopped != "" && st.TooManyStops() {
		return stoppedOutcome(st, candidate, v), nil
	}

	round := st.Rejections + 1
	where := verify.Where{Label: lab, Round: fmt.Sprint(round)}
	verifier := st.Verifier
	// A rejection of a candidate this attempt replaces before the agent was
	// told it (Given, and fresh), or of one before the candidate the verifier
	// hasn't read yet (Owed), isn't a round: it doesn't judge what the agent
	// submits now. Its findings are worth reading, though: a note, before the
	// wait.
	var aside *event.UserEvent
	if (v.Given || v.Owed) && !v.Pass {
		text, told, err := a.earlierRejection(ctx, st, v, verify.LatestWith(verifier, where))
		if err != nil {
			return outcome{}, err
		}
		if told != nil {
			aside = verifyMessage(text, told)
		}
	}
	// Input that came while the checks ran overtakes an attempt that would give
	// the verifier a candidate, as input that came while it was generated does
	// (concludeCall): the attempt didn't read it. It may be a new version's
	// message (a loop's tick, a message with verify), and the result, which
	// answered the request before it, must become neither that version's
	// candidate nor that request the verifier's. Such a message is written
	// under the verification lock, held here, so one that came before is
	// visible now, and none can come until the candidate is given.
	if verifier == "" || v.Missing || fresh || v.Stopped != "" {
		if unread, err := a.unreadSince(ctx, at.step+1, nil); err != nil {
			return outcome{}, err
		} else if len(unread) > 0 {
			return held(verify.Overtaken(describeArrivals(unread), st), ""), nil
		}
	}
	switch {
	case verifier == "" || v.Missing:
		if verifier, err = a.startVerifier(ctx, st, hist, candidate, lab); err != nil {
			return a.unstarted(ctx, st, candidate, err)
		}
		// Recorded before waiting for it to start, so its stops count even if
		// one comes first.
		if err := a.recordVerifying(ctx, verifier, round, candidate); err != nil {
			return outcome{}, err
		}
		if ok, err := a.verifierTookIt(ctx, verifier); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("verifier %s didn't start", verifier)
			}
			st.Verifier = verifier
			return a.unstarted(ctx, st, candidate, err)
		}
		return waitingOutcome(verifier, where, true, aside), nil
	case !fresh && v.Stopped == "":
		// It has this very conclusion; make sure it is reading it.
		if v.Owed {
			if err := a.engage(ctx, verifier, a.latestDelivery(ctx, verifier), true); err != nil {
				return a.unstarted(ctx, st, candidate, err)
			}
		}
		return waitingOutcome(verifier, where, false, aside), nil
	default: // a new candidate, or the same one again after a stop
		if err := a.engage(ctx, verifier, a.delivery(ctx, hist, st, lab, round, candidate), false); err != nil {
			return a.unstarted(ctx, st, candidate, err)
		}
	}
	if err := a.recordVerifying(ctx, verifier, round, candidate); err != nil {
		return outcome{}, err
	}
	return waitingOutcome(verifier, where, true, aside), nil
}

// earlierRejection tells the agent a rejection (v) of an earlier candidate of
// its, followed by then, what stands now. told is nil if there is nothing to
// tell (no findings) or it was told already.
func (a *EventLoopAgent) earlierRejection(ctx context.Context, st verify.State, v verify.Verdict, then string) (text string, told *event.VerifyOutcome, err error) {
	if v.Findings == "" {
		return "", nil, nil
	}
	told = &event.VerifyOutcome{Verifier: st.Verifier, Verdict: event.VerdictFail, Findings: v.Findings, At: v.At,
		Earlier: true}
	if done, err := a.toldBefore(ctx, told); err != nil || done {
		return "", nil, err
	}
	return verify.Earlier(st.Verifier, false, v.Findings, true) + "\n\n" + then, told, nil
}

// waitingOutcome waits for verifier's verdict; its text is the note for a
// reply that waits (submitted: the reply is the candidate it was given), and
// aside a note to write first.
func waitingOutcome(verifier string, w verify.Where, submitted bool, aside *event.UserEvent) outcome {
	text := verify.StillWaiting(verifier, w)
	if submitted {
		text = verify.Submitted(verifier, w)
	}
	return outcome{kind: waits, verifier: verifier, text: text, aside: aside}
}

// rejectedOutcome holds an attempt the verifier rejected (v), rounds
// remaining (st counts this rejection): the answer carries the findings, and
// the round's record goes with it.
func rejectedOutcome(st verify.State, v verify.Verdict) outcome {
	out := held(verify.Rejected(st.Verifier, st, v.Findings), event.CheckVerify)
	out.told = toldVerdict(st, v, st.Rejections)
	out.records = []event.Event{rejectedRecord(st)}
	return out
}

// rejectedRecord counts the round st's latest rejection ends.
func rejectedRecord(st verify.State) *event.VerifyEvent {
	return &event.VerifyEvent{Action: event.VerifyRejected, By: event.ByHarness, Verifier: st.Verifier, Round: st.Rejections}
}

// toldVerdict is a verdict (v) on the latest candidate as the agent is told
// it, in round.
func toldVerdict(st verify.State, v verify.Verdict, round int) *event.VerifyOutcome {
	o := &event.VerifyOutcome{Verifier: st.Verifier, Verdict: event.VerdictFail, Findings: v.Findings, Round: round, At: v.At}
	if v.Pass {
		o.Verdict = event.VerdictPass
	}
	if st.Request != nil {
		o.MaxRounds = st.Request.MaxRounds
	}
	return o
}

// toldStop is the verifier's latest stop without a verdict (v) as the agent
// is told it: its status (from st, else v) and why. A stop isn't a round, so
// it carries none.
func toldStop(st verify.State, v verify.Verdict) *event.VerifyOutcome {
	o := &event.VerifyOutcome{Verifier: st.Verifier, Verdict: v.Stopped, At: v.At}
	if s, reason, ok := strings.Cut(st.LastStop, ": "); ok && (s == v.Stopped || v.Stopped == "") {
		o.Verdict, o.Findings = s, reason
	}
	return o
}

// stopText is a stop as the agent's texts put it: its status, and why.
func stopText(o *event.VerifyOutcome) string {
	if o.Findings == "" {
		return o.Verdict
	}
	return o.Verdict + ": " + o.Findings
}

// toldEnd concludes with candidate, not verified, the attempt that follows the
// agent being told its verification ended (o, State.EndTold), as that message
// said it would, whatever the verifier did since: re-engaged by someone's
// message, it may have given a new verdict, which comes too late to count.
// The conclusion carries the ending it was told.
func toldEnd(st verify.State, candidate string, o *event.VerifyOutcome) outcome {
	if o.Stopped() {
		if !st.TooManyStops() { // a verdict since ended the run of stops
			st.Stops, st.LastStop = verify.MaxStops, stopText(o)
		}
		return stoppedOutcome(st, candidate, verify.Verdict{Stopped: o.Verdict, At: o.At})
	}
	st.Rejections = o.Round
	return roundsOutcome(st, candidate, verify.Verdict{Given: true, Findings: o.Findings, At: o.At})
}

// recordStartVerification records the verification asked for with the
// agent's start (Config.Verify), under the verification lock. The session
// exists from early in its bootstrap, so its parent or the operator may have
// changed its verification since; that change is the newer one, and stands
// in place of the start's request.
func (a *EventLoopAgent) recordStartVerification(ctx context.Context) error {
	defer verify.Lock(a.env.RunID, a.cfg.SessionID)()
	st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
	if err != nil || len(st.History) > 0 {
		return err
	}
	_, err = a.env.Store.AppendEvent(ctx, a.env.RunID, a.cfg.SessionID, a.cfg.Verify)
	return err
}

// verifyMessage is a harness message telling the agent what its verifier said.
func verifyMessage(text string, told *event.VerifyOutcome) *event.UserEvent {
	return &event.UserEvent{Content: harness(text), Check: event.CheckVerify, Outcome: told}
}

// recordVerifying records candidate as given to verifier in round: the
// record is what the state reads the candidate from (verify.Derive).
func (a *EventLoopAgent) recordVerifying(ctx context.Context, verifier string, round int, candidate string) error {
	_, err := a.env.Store.AppendEvent(ctx, a.env.RunID, a.cfg.SessionID, &event.VerifyEvent{
		Action: event.VerifyVerifying, By: event.ByHarness, Verifier: verifier, Round: round, Candidate: candidate})
	return err
}

// passedOutcome concludes with the candidate the verifier passed (v).
func passedOutcome(st verify.State, v verify.Verdict) outcome {
	round := st.Rejections + 1
	return outcome{kind: concludes, result: st.Candidate, text: verify.Verified(st.Verifier, verify.At(st, round)),
		version: st.Version, notes: []event.ResultNote{verify.VerifiedNote(st.Verifier, round)},
		record: &event.VerifyEvent{Action: event.VerifyPassed, By: event.ByHarness, Verifier: st.Verifier, Round: round},
		again:  verify.Passed(st.Verifier, verify.At(st, round)), told: toldVerdict(st, v, round)}
}

// roundsOutcome concludes with candidate when the verifier's rejection (v,
// counted in st) was the last allowed.
func roundsOutcome(st verify.State, candidate string, v verify.Verdict) outcome {
	out := notVerified(st, candidate, verify.NotVerifiedNote(st.Rejections, v.Findings),
		verify.RoundsReason(st.Rejections, v.Findings), endRounds)
	out.told = toldVerdict(st, v, st.Rejections)
	return out
}

// Why a verification ended unverified (outcome.ended).
const (
	endRounds    = "rounds"    // the last allowed rejection
	endStops     = "stops"     // the verifier stopped too often without a verdict
	endUnstarted = "unstarted" // no verifier could be started or re-engaged
)

// stoppedOutcome concludes after the verifier stopped too often without a
// verdict (v, the latest stop).
func stoppedOutcome(st verify.State, candidate string, v verify.Verdict) outcome {
	out := notVerified(st, candidate, verify.StoppedNote(st.Stops, st.LastStop),
		verify.StopsReason(st.Stops, st.LastStop), endStops)
	out.told = toldStop(st, v)
	return out
}

// notVerified ends verification unverified, concluding with candidate. The
// record carries a reason only when the rounds aren't why (the verifier
// stopped, or couldn't start); a rounds ending reads from its round count.
func notVerified(st verify.State, candidate string, note event.ResultNote, reason, why string) outcome {
	var recordReason string
	if why != endRounds {
		recordReason = note.Text
	}
	again := verify.Ended(st.Version, reason)
	if why == endUnstarted {
		again = "Not verified" + reason + ". " + resultAgain
	}
	return outcome{kind: concludes, result: candidate, text: verify.NotVerified(reason), ended: why, again: again,
		version: st.Version, notes: []event.ResultNote{note},
		record: &event.VerifyEvent{Action: event.VerifyExhausted, By: event.ByHarness, Verifier: st.Verifier,
			Round: st.Rejections, Reason: recordReason}}
}

// unstarted handles a verifier that couldn't be started or re-engaged: the
// agent concludes, not verified, rather than wait for one that will never
// answer. Not if this process is stopping, though: that failure says nothing
// about the verifier, and concluding would leave nothing for a resume.
func (a *EventLoopAgent) unstarted(ctx context.Context, st verify.State, candidate string, err error) (outcome, error) {
	if ctx.Err() != nil || a.env.Registry.Closed() {
		return outcome{}, fmt.Errorf("verifier: %w (this process is stopping)", err)
	}
	if errors.Is(err, db.ErrStore) {
		return outcome{}, err // the store failed, not the verifier: leave it for the recovery
	}
	a.logger.Warn("could not start or re-engage a verifier; concluding unverified", "error", err)
	return notVerified(st, candidate, verify.UnstartedNote(err), verify.UnstartedReason(err), endUnstarted), nil
}

// verifierStartTimeout bounds how long engage waits for a verifier that a
// goroutine holds but hasn't started on its latest candidate.
const verifierStartTimeout = 5 * time.Second

// engage makes sure the verifier reads content. The message (a user event)
// revives a stopped verifier, and one still working reads it at its next
// turn. But one caught concluding when it lands only gets its exiting
// goroutine bumped, and reads nothing: it is sent the message again once that
// goroutine is gone. sent: the message is already in its stream. An error
// means no goroutine took it up (a respawn that failed, say).
func (a *EventLoopAgent) engage(ctx context.Context, verifier, content string, sent bool) error {
	for attempt := range 2 {
		if !sent || attempt > 0 {
			if _, err := a.env.Store.AppendEvent(ctx, a.env.RunID, verifier,
				&event.UserEvent{Content: content, Check: event.CheckVerify}); err != nil {
				return fmt.Errorf("give the candidate to verifier %s: %w", verifier, err)
			}
		}
		ok, err := a.verifierTookIt(ctx, verifier)
		if err != nil || ok {
			return err
		}
	}
	return fmt.Errorf("verifier %s didn't take up the candidate: it isn't running and wasn't revived", verifier)
}

// verifierTookIt waits until the verifier is on its latest candidate: reading
// it (verify.Verdict.Reading), or past it (it took a turn on it, then
// concluded or stopped: its outcome wakes the agent). Running isn't enough: a
// verifier concluding with a final turn when the candidate lands (one standing
// by concludes quietly, waking no one) leaves it unread, so it is trusted only
// once a turn reads it, and checked again when its goroutine is gone. False:
// it is stopped or concluded without having read it, and no goroutine is left
// to.
func (a *EventLoopAgent) verifierTookIt(ctx context.Context, verifier string) (bool, error) {
	took := func() (bool, error) {
		v, err := verify.Inspect(ctx, a.env.Store, a.env.RunID, verifier)
		return err == nil && !v.Missing && (v.Started || v.Reading), err
	}
	deadline := time.Now().Add(verifierStartTimeout)
	// Polls back off from 10 ms to 100 ms: a verifier slow to start (or to
	// leave) costs a few reads a second, not a hundred.
	pause := 10 * time.Millisecond
	timer := time.NewTimer(pause)
	defer timer.Stop()
	for {
		if ok, err := took(); err != nil || ok {
			return ok, err
		}
		if !a.env.Registry.IsRegistered(verifier) {
			return took() // gone; but it may have started and finished since the read
		}
		if time.Now().After(deadline) {
			return true, nil // a goroutine holds it, slow to start: trust it
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
		}
		pause = min(2*pause, 100*time.Millisecond)
		timer.Reset(pause)
	}
}

// startVerifier starts the verifier for the current request, as this agent's
// sub-agent in its workspace, on the request's model (default: this agent's),
// with candidate lab; hist is the attempt's requestHistory.
func (a *EventLoopAgent) startVerifier(ctx context.Context, st verify.State, hist []db.EventRecord, candidate string, lab verify.Label) (string, error) {
	task := verify.VerifierTask(a.cfg.SessionID, a.currentRequest(hist), st.Request.Criteria, candidate, lab,
		agent.IsVerifier(verifierAgentType))
	started, err := spawn.Start(ctx, a.env, a.cfg.SessionID, spawn.Options{
		Task: task, AgentType: verifierAgentType, Model: st.Request.Model, VerifierFor: a.cfg.SessionID})
	return started.SessionID, err
}

// delivery is the message that gives st's verifier candidate lab, in the
// verdict form its agent type takes (a verifier started before the verifier
// type existed is a standard agent, whose result's first line is read). A
// verifier kept from an earlier version gets the version's request and
// criteria with its first candidate; hist is the attempt's requestHistory, for
// the request.
func (a *EventLoopAgent) delivery(ctx context.Context, hist []db.EventRecord, st verify.State, lab verify.Label, round int, candidate string) string {
	typed := false
	if s, err := a.env.Store.GetSession(ctx, a.env.RunID, st.Verifier); err == nil {
		typed = agent.IsVerifier(s.AgentType)
	}
	if lab.Candidate == 1 && st.VerifierSince < st.Version {
		return verify.NewVersion(a.cfg.SessionID, a.currentRequest(hist), st.Request.Criteria, candidate, lab, typed)
	}
	return verify.Delivery(a.cfg.SessionID, round, candidate, lab, typed)
}

// latestDelivery is the latest candidate message verifier received, to send
// again to one that concluded as it arrived (see engage): the same words, so
// it reads what it would have.
func (a *EventLoopAgent) latestDelivery(ctx context.Context, verifier string) string {
	events, _ := a.env.Store.GetEvents(ctx, a.env.RunID, verifier, db.EventFilter{})
	for i := len(events) - 1; i >= 0; i-- {
		if u, ok := events[i].Event.(*event.UserEvent); ok && u.Check == event.CheckVerify {
			return u.Content
		}
	}
	return ""
}

// requestHistory is what the agent was asked, as of the turn that answered
// events (its context): its user messages and messages through that turn's
// step, read from its whole stream rather than its context. Compaction folds
// earlier turns into a summary, but their events stay in the stream, so the
// request the agent is answering, or the candidate a verifier was given, is
// found however long ago it came. Read by type, so it costs only those rows.
func (a *EventLoopAgent) requestHistory(ctx context.Context, events []db.EventRecord) ([]db.EventRecord, error) {
	through := 0
	if n := len(events); n > 0 {
		through = events[n-1].Step
	}
	hist, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID, db.EventFilter{
		EndStep: &through, Types: db.EventTypes(&event.UserEvent{}, &event.MessageEvent{})})
	if err != nil {
		return nil, fmt.Errorf("read what %s was asked: %w", a.cfg.SessionID, err)
	}
	return hist, nil
}

// currentRequest is what this agent is answering: its task, and the latest
// message from the operator or its parent if one came after it. hist is a
// requestHistory.
func (a *EventLoopAgent) currentRequest(hist []db.EventRecord) string {
	for i := len(hist) - 1; i >= 0; i-- {
		switch e := hist[i].Event.(type) {
		case *event.UserEvent:
			if hist[i].Step > 0 && !e.IsHarness() {
				return a.withTask("the operator's latest message", e.Content)
			}
		case *event.MessageEvent:
			if e.SenderType == event.SenderTypeAgent && e.Sender == a.cfg.ParentID && a.cfg.ParentID != "" {
				return a.withTask("its parent's latest message", e.Content)
			}
		}
	}
	return a.cfg.Task
}

func (a *EventLoopAgent) withTask(what, msg string) string {
	if a.cfg.Task == "" {
		return msg
	}
	return fmt.Sprintf("%s, which it is answering now:\n%s\n\n(Its original task: %s)", what, msg, a.cfg.Task)
}

// settle decides a conclude call whose wait for a verdict something ended
// (arrived: what woke it). The verdict concludes it (a pass, or the last
// allowed rejection) or answers it (a rejection, a verifier that stopped);
// with no verdict, the call returns saying what arrived, or waits on if that
// was only the verifier's own outcomes. A newer version or a clear meanwhile
// withdraws the candidate (its withdrawn record wakes the call).
func (a *EventLoopAgent) settle(ctx context.Context, arrived []db.EventRecord) (outcome, error) {
	defer verify.Lock(a.env.RunID, a.cfg.SessionID)()
	st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
	if err != nil {
		return outcome{}, err
	}
	if !st.On || !st.Pending {
		return withdrawnOutcome(st, st.Withdrawn), a.recordStale(ctx, st)
	}
	v, err := verify.Evaluate(ctx, a.env.Store, a.env.RunID, st)
	if err != nil {
		return outcome{}, err
	}
	round := st.Rejections + 1
	switch {
	case v.Given && v.Pass:
		return passedOutcome(st, v), nil
	case v.Given:
		st.Rejections++
		if st.Exhausted() {
			return roundsOutcome(st, st.Candidate, v), nil
		}
		return rejectedOutcome(st, v), nil
	case v.Stopped != "":
		if st.TooManyStops() {
			return stoppedOutcome(st, st.Candidate, v), nil
		}
		told := toldStop(st, v)
		out := held(verify.Stopped(st.Verifier, st.Label(), stopText(told)), event.CheckVerify)
		out.told = told
		return out, nil
	case v.Owed:
		// What woke it was the verifier's outcome for an earlier candidate, and
		// the latest one reached it as it concluded: make sure it reads it.
		if err := a.engage(ctx, st.Verifier, a.latestDelivery(ctx, st.Verifier), true); err != nil {
			return a.unstarted(ctx, st, st.Candidate, err)
		}
		out := held(verify.Superseded(st.Verifier, verify.At(st, round)), event.CheckVerify)
		if !v.Pass {
			// A rejection of that earlier candidate: not a round, but its
			// findings are worth reading.
			text, told, err := a.earlierRejection(ctx, st, v, verify.Superseded(st.Verifier, verify.At(st, round)))
			if err != nil {
				return outcome{}, err
			}
			if told != nil {
				out.text, out.told = text, told
			}
		}
		return out, nil
	}
	unread := a.unreadIn(ctx, arrived)
	if len(unread) == 0 {
		// Only the verifier's outcomes, which say nothing new (a stop the
		// checks re-engaged it after, say): wait on.
		return outcome{kind: waits, verifier: st.Verifier}, nil
	}
	return held(verify.Interrupted(st.Verifier, verify.At(st, round), describeArrivals(unread)), event.CheckVerify), nil
}

// withdrawnOutcome answers a conclude call whose candidate (withdrawn) a
// newer version or a clear withdrew while it waited.
func withdrawnOutcome(st verify.State, withdrawn verify.Label) outcome {
	version := 0
	if st.On {
		version = st.Version
	}
	return held(verify.Withdrawn(withdrawn, version), event.CheckVerify)
}

// recordStale records as stale the verifiers' verdicts that arrived after
// the version changed (st's Late ones, read while no candidate is pending):
// on a withdrawn candidate, they change nothing. It runs under the
// verification lock wherever the harness reads a verdict, so each is recorded
// once.
func (a *EventLoopAgent) recordStale(ctx context.Context, st verify.State) error {
	for _, h := range st.Late {
		e := st.History[h]
		o := *e.Outcome
		if err := verify.RecordStale(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID, &o, e.Label, st.WithdrawnBy(e.Label)); err != nil {
			return err
		}
	}
	return nil
}

// noteVerdict tells the agent what its verifier said when its outcome arrived
// at step, the turn about to be generated, while the agent worked or waited in
// await_event: a verify message written at that step, so the turn reads it
// where the outcome landed (the outcome's own record isn't shown to the
// model). A rejection counts its round here, its record written with the
// message. A verdict on an earlier candidate is told as information, and one
// on a candidate a newer version or a clear withdrew is recorded as stale and
// told as such, so the turn it woke reads why. Each outcome is told once
// (toldBefore): not again after the call result or message that told it.
func (a *EventLoopAgent) noteVerdict(ctx context.Context, step int) error {
	recs, err := a.env.Store.GetEvents(ctx, a.env.RunID, a.cfg.SessionID, db.EventFilter{StartStep: &step, EndStep: &step})
	if err != nil {
		return err
	}
	var arrived *event.VerifyOutcome // the latest outcome record at step
	fromVerifier, legacy, told := false, false, false
	for _, r := range recs {
		switch e := r.Event.(type) {
		case *event.VerifyEvent:
			switch e.Action {
			case event.VerifyVerdict:
				fromVerifier, legacy, told, arrived = true, false, false, e.Outcome
			case event.VerifyPassed, event.VerifyExhausted:
				// The harness acted on the verdict before it: the conclusion went
				// out with it. A turn at this step (the session revived by a
				// message that landed at its last step) has nothing to be told.
				told = true
			}
		case *event.ChildResultEvent:
			// How a verifier's outcome arrived before verifiers posted their
			// own.
			if a.isOwnVerifier(ctx, e.ChildSessionID) {
				fromVerifier, legacy, told, arrived = true, true, false, nil
			}
		case *event.UserEvent:
			// Told at this step already by a message written before outcomes
			// were carried, which has only its Check (toldBefore finds the rest).
			if legacy && e.Check == event.CheckVerify {
				told = true
			}
		}
	}
	if !fromVerifier || told {
		return nil
	}
	defer verify.Lock(a.env.RunID, a.cfg.SessionID)()
	st, err := verify.Load(ctx, a.env.Store, a.env.RunID, a.cfg.SessionID)
	if err != nil || (st.On && st.Pending && st.EndTold != nil) {
		// Once told the end, the next conclusion is delivered not verified, so
		// a later outcome (the verifier re-engaged by someone's message)
		// changes nothing to tell.
		return err
	}
	var v verify.Verdict
	if st.On && st.Pending {
		if v, err = verify.Evaluate(ctx, a.env.Store, a.env.RunID, st); err != nil {
			return err
		}
	} else if err := a.recordStale(ctx, st); err != nil {
		return err
	}
	var msg string
	var tell *event.VerifyOutcome
	var records []event.Event
	switch {
	case v.Given && v.Pass:
		tell = toldVerdict(st, v, st.Rejections+1)
		msg = verify.Passed(st.Verifier, verify.At(st, st.Rejections+1))
	case v.Given:
		st.Rejections++
		tell = toldVerdict(st, v, st.Rejections)
		if st.Exhausted() {
			msg = verify.Ended(st.Version, verify.RoundsReason(st.Rejections, v.Findings))
			break
		}
		records = []event.Event{rejectedRecord(st)}
		msg = verify.Rejected(st.Verifier, st, v.Findings)
	case v.Stopped != "" && st.TooManyStops():
		tell = toldStop(st, v)
		msg = verify.Ended(st.Version, verify.StopsReason(st.Stops, st.LastStop))
	case v.Stopped != "":
		tell = toldStop(st, v)
		msg = verify.Stopped(st.Verifier, st.Label(), stopText(tell))
	case arrived != nil && !arrived.Stopped():
		// A verdict, but not on the candidate pending now (or nothing is).
		pending := st.On && st.Pending
		tell = &event.VerifyOutcome{Verifier: arrived.Verifier, Verdict: arrived.Verdict, Findings: arrived.Findings,
			At: arrived.At, Earlier: true}
		msg = verify.Earlier(arrived.Verifier, arrived.Verdict == event.VerdictPass, arrived.Findings, pending)
		if lab, stale := st.StaleLabel(arrived); stale {
			msg = verify.StaleNote(arrived.Verifier, arrived.Verdict == event.VerdictPass, arrived.Findings, lab,
				st.WithdrawnBy(lab))
		}
		if pending {
			msg += "\n\n" + verify.LatestWith(st.Verifier, verify.At(st, st.Rejections+1)) + " " + verify.WaitForLatest
		}
	default:
		return nil // a stop that isn't the pending candidate's: nothing to act on
	}
	if done, err := a.toldBefore(ctx, tell); err != nil || done {
		return err
	}
	return a.writeAnswer(ctx, outcome{records: records}, verifyMessage(msg, tell), step)
}

// isOwnVerifier reports whether session is this agent's verifier.
func (a *EventLoopAgent) isOwnVerifier(ctx context.Context, session string) bool {
	s, err := a.env.Store.GetSession(ctx, a.env.RunID, session)
	return err == nil && verify.VerifierFor(s) == a.cfg.SessionID
}

// logVerifyOutcome logs how a root agent's verified conclusion ended, at info
// level, for a harness supervising a headless run to key on; a sub-agent's
// goes to debug, so the info lines speak for the run's result only.
func (a *EventLoopAgent) logVerifyOutcome(out outcome) {
	e := out.record
	level := slog.LevelInfo
	if a.cfg.ParentID != "" {
		level = slog.LevelDebug
	}
	ctx := context.Background()
	switch {
	case e.Action == event.VerifyPassed:
		a.logger.Log(ctx, level, "verification passed", "verifier", e.Verifier, "round", e.Round)
	case out.ended == endRounds:
		a.logger.Log(ctx, level, "not verified after "+verify.Rejections(e.Round), "verifier", e.Verifier, "rejections", e.Round)
	case e.Action == event.VerifyExhausted:
		reason := ""
		if len(out.notes) > 0 {
			reason = out.notes[0].Text
		}
		a.logger.Log(ctx, level, "not verified", "verifier", e.Verifier, "rejections", e.Round, "reason", reason)
	}
}
