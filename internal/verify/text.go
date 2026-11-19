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
	"fmt"
	"strings"

	"amplio/internal/event"
	"amplio/internal/util"
)

// What the agent, its verifier and its parent read. Harness messages start
// with "[system] ", like every harness nudge.

// Caps on what is copied into a verifier's messages and a parent's note; the
// verifier can read the rest in the agent's trajectory.
const (
	maxRequestRunes   = 6000
	maxCandidateRunes = 20000
	maxFindingsRunes  = 600
)

// Note is what the agent reads for a change someone else made to its
// verification (set, replace, clear), shown where the VerifyEvent sits in its
// stream; version is the version a set or replace starts, or the one a clear
// ends. The harness's own actions come with messages of their own, so they
// read as nothing here.
func Note(e *event.VerifyEvent, version int) string {
	if event.LoopOf(e.By) != "" && e.Action != event.VerifyClear { // a loop's clear reads as anyone's, below
		return loopNote(e, version)
	}
	against := "your current request"
	if e.Criteria != "" {
		against = e.Criteria
	}
	evidence := " Show evidence for your claims as you work (commands run, test output)."
	switch e.Action {
	case event.VerifySet:
		return fmt.Sprintf("[system] Verification v%d: your conclusion will be checked by an independent verifier before "+
			"it is delivered, against: %s.%s", version, against, evidence)
	case event.VerifyReplace:
		return fmt.Sprintf("[system] %s replaced your verification request: v%d supersedes v%d, so a conclusion of yours "+
			"still with the verifier for v%d is withdrawn and won't be delivered. Your next conclusion is checked "+
			"against: %s.%s", byWhom(e.By), version, version-1, version-1, against, evidence)
	case event.VerifyClear:
		return fmt.Sprintf("[system] %s turned verification off (v%d ended): a conclusion of yours still with the "+
			"verifier is withdrawn, and your next conclusion is delivered as it is.", byWhom(e.By), version)
	}
	return ""
}

// loopNote is Note for a version a loop started with its tick, the message
// right after it, whose label names the version: only criteria other than
// the tick itself are left to say.
func loopNote(e *event.VerifyEvent, version int) string {
	if e.Criteria == "" {
		return ""
	}
	return fmt.Sprintf("[system] Verification v%d checks your conclusion against: %s.", version, e.Criteria)
}

func byWhom(by string) string {
	switch by {
	case event.ByOperator:
		return "The operator"
	case event.ByHarness, "":
		return "Amplio"
	}
	if id := event.LoopOf(by); id != "" {
		return "The operator's loop " + id
	}
	return "Your parent " + by
}

// The agent's texts below are bare sentences: as a conclude call's result they
// stand as they are, and as a harness message the event loop prefixes them.

// Where is a candidate as the agent's texts place it: "v2, candidate 1;
// round 1" (round: the round in progress, or the one just decided).
type Where struct {
	Label Label
	Round string
}

// At places the latest candidate of st at round.
func At(st State, round int) Where { return Where{st.Label(), fmt.Sprint(round)} }

func (w Where) String() string { return w.Label.String() + "; round " + w.Round }

// Submitted is the agent's message when a reply without tool calls goes to
// the verifier as its conclusion; the harness then waits for the verdict.
func Submitted(verifier string, w Where) string {
	return fmt.Sprintf("Your conclusion (%s) is being verified by `%s`, and amplio waits for the verdict before your "+
		"next turn: a pass delivers this conclusion, marked verified. Anything else that arrives ends the wait early.",
		w, verifier)
}

// StillWaiting is the agent's message when it keeps replying without tool
// calls while its conclusion is with the verifier: the harness waits for the
// verdict, as conclude() would.
func StillWaiting(verifier string, w Where) string {
	return fmt.Sprintf("You replied again without calling conclude while your conclusion (%s) is with `%s`, so "+
		"amplio waits for its verdict, as conclude() would. A reply doesn't replace your conclusion; conclude(result) "+
		"does.", w, verifier)
}

// ReplyWhilePending is the agent's message for a reply without tool calls
// while its conclusion is with the verifier: such a reply isn't taken as a
// result. passed: the verifier has passed it already; stopped: the verifier
// stopped without a verdict (its status).
func ReplyWhilePending(verifier string, w Where, passed bool, stopped string) string {
	switch {
	case passed:
		return fmt.Sprintf("`%s` passed your conclusion (%s), so a reply without tool calls isn't taken as your "+
			"result. Call conclude() to deliver it, marked verified, or conclude(result) to submit a new one.", verifier, w)
	case stopped != "":
		return fmt.Sprintf("Your verifier `%s` stopped (%s) without a verdict on your conclusion (%s), and a reply "+
			"without tool calls isn't taken as your result. Call conclude() to give your conclusion to it again, or "+
			"conclude(result) to submit a new one.", verifier, stopped, w.Label)
	}
	return fmt.Sprintf("Your conclusion (%s) is with `%s`, so a reply without tool calls isn't taken as your result. "+
		"Call conclude() to wait for its verdict, conclude(result) to submit a new conclusion, or await_event to wait "+
		"for something else.", w, verifier)
}

// ReplyIsNoVerdict is a verifier's message for a reply without tool calls:
// its verdict is what it gives conclude.
const ReplyIsNoVerdict = "A reply without tool calls isn't your verdict. When you have checked the conclusion, " +
	"call conclude(verdict: \"PASS\" or \"FAIL\", findings) to deliver it; until then, keep checking."

// Passed is the agent's message when the verifier passes its conclusion while
// it isn't waiting in a conclude call.
func Passed(verifier string, w Where) string {
	return fmt.Sprintf("`%s` passed your conclusion (%s). Call conclude() to deliver it, marked verified, or "+
		"conclude(result) to submit a new one, which goes back to the verifier.", verifier, w)
}

// Verified is a conclude call's result when the verifier's pass concludes it.
func Verified(verifier string, w Where) string {
	return fmt.Sprintf("Verified by `%s` (%s): concluded with the result it verified.", verifier, w)
}

// Rejected is the agent's answer when the verifier rejects its conclusion and
// rounds remain: a conclude call's result, or a verify message. st counts the
// rejection.
func Rejected(verifier string, st State, findings string) string {
	return fmt.Sprintf("`%s` rejected your conclusion (%s):\n\n%s\n\nAddress the findings, then call "+
		"conclude(result) with your new result; it goes back to `%s`.", verifier, Where{st.Label(), roundOf(st)},
		util.TruncateRunes(findings, maxCandidateRunes), verifier)
}

// roundOf is the round just rejected, "n of max" when the rounds are bounded.
func roundOf(st State) string {
	if st.Request != nil && st.Request.MaxRounds > 0 {
		return fmt.Sprintf("%d of %d", st.Rejections, st.Request.MaxRounds)
	}
	return fmt.Sprintf("%d", st.Rejections)
}

// Ended is the agent's message when its verification (version) ends without
// a pass (reason: one of the *Reason texts) while it isn't waiting in a
// conclude call; its next conclusion is delivered with a not-verified note.
func Ended(version int, reason string) string {
	return fmt.Sprintf("Verification v%d ended: not verified%s. Call conclude() to deliver your last conclusion, or "+
		"conclude(result) to deliver a new one; either is marked not verified.", version, reason)
}

// IsEnded reports whether msg, a message to the agent (with or without the
// "[system] " every harness message starts with), is Ended's, or the same
// message from before versions were numbered.
func IsEnded(msg string) bool {
	msg = strings.TrimPrefix(msg, "[system] ")
	rest, numbered := strings.CutPrefix(msg, "Verification v")
	digits := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
	return numbered && digits > 0 && strings.HasPrefix(rest[digits:], " ended: not verified") ||
		strings.HasPrefix(msg, "Verification ended: not verified")
}

// RoundsReason, StopsReason and UnstartedReason say why a verification ended
// without a pass, completing "not verified".
func RoundsReason(rejections int, findings string) string {
	return fmt.Sprintf(" after %s (latest findings: %s)", Rejections(rejections), short(findings))
}

func StopsReason(stops int, last string) string {
	return fmt.Sprintf(": the verifier stopped %d times without a verdict (last: %s)", stops, short(last))
}

func UnstartedReason(err error) string { return ": the verifier could not be started: " + err.Error() }

// NotVerified is a conclude call's result when it concludes with its
// verification ended without a pass; reason is one of the *Reason texts.
func NotVerified(reason string) string { return "Concluded, marked not verified" + reason + "." }

// short puts findings on one line, capped.
func short(s string) string {
	return util.TruncateRunes(strings.Join(strings.Fields(s), " "), maxFindingsRunes)
}

// Interrupted is a conclude call's result when something other than a
// verdict ends its wait; arrived says what came.
func Interrupted(verifier string, w Where, arrived string) string {
	return fmt.Sprintf("Not concluded yet: your conclusion (%s) is still with `%s`, and this arrived meanwhile: %s. "+
		"Call conclude() to keep waiting for the verdict (a pass concludes with this conclusion), or "+
		"conclude(result) to submit a new one.", w, verifier, arrived)
}

// Overtaken is a conclude call's result when input (arrived) reached the
// agent, its verification on (st), while it generated the call: nothing went
// to the verifier, and a conclusion pending stays with it.
func Overtaken(arrived string, st State) string {
	if st.Pending {
		return fmt.Sprintf("Not concluded yet: this arrived meanwhile: %s. Nothing new went to the verifier: your "+
			"conclusion (%s) is still with `%s`. Call conclude() to wait for its verdict, or conclude(result) to "+
			"submit a new one.", arrived, At(st, st.Rejections+1), st.Verifier)
	}
	return fmt.Sprintf("Not concluded yet: this arrived meanwhile: %s. Nothing went to the verifier: call "+
		"conclude(result) again to submit your result.", arrived)
}

// VerdictOnEarlier answers a verifier's attempt to conclude when a new
// candidate arrived meanwhile (arrived says what came): its verdict was on the
// earlier one.
func VerdictOnEarlier(arrived string) string {
	return fmt.Sprintf("Not concluded: this arrived meanwhile: %s. Your verdict was on the earlier candidate, so it "+
		"doesn't count, and nothing was delivered. Check the new candidate, then conclude with your verdict on it.",
		arrived)
}

// VerdictWithdrawn answers a verifier's attempt to conclude when the withdrawal
// of its candidate arrived meanwhile (arrived says what came): it stands by.
func VerdictWithdrawn(arrived string) string {
	return fmt.Sprintf("Not concluded: this arrived meanwhile: %s. Your verdict was on a withdrawn candidate, so it "+
		"doesn't count, and nothing was delivered. End your turn without a verdict (a short reply is enough): the next "+
		"candidate wakes you.", arrived)
}

// Redelivered answers a verifier that, told its verdict was on an earlier
// candidate (VerdictOnEarlier), gives the same verdict again straight away.
const Redelivered = "Not concluded: that is the verdict you gave on the earlier candidate, which doesn't count for " +
	"the new one. Check the new candidate, then conclude with your verdict on it."

// Superseded is a conclude call's result when its verifier's verdict is for
// an earlier candidate: it is checking the latest one now.
func Superseded(verifier string, w Where) string {
	return fmt.Sprintf("`%s`'s verdict was on an earlier candidate, so it doesn't count; it is checking your latest "+
		"one (%s) now. Call conclude() to wait for its verdict.", verifier, w)
}

// Earlier tells the agent its verifier's verdict on an earlier conclusion of
// its, which doesn't count: a rejection isn't a round, but its findings may
// still help. pending: a later conclusion is with the verifier now (else the
// verification has moved on).
func Earlier(verifier string, pass bool, findings string, pending bool) string {
	why := "your verification has moved on"
	if pending {
		why = "it wasn't your latest"
	}
	if pass {
		return fmt.Sprintf("`%s` passed an earlier conclusion of yours; that doesn't count (%s).", verifier, why)
	}
	return fmt.Sprintf("`%s` rejected an earlier conclusion of yours (not a round: %s):\n\n%s",
		verifier, why, util.TruncateRunes(findings, maxCandidateRunes))
}

// StaleNote tells the agent a stale verdict: its verifier passed or rejected
// candidate lab after version (or, 0, verification turned off) withdrew it.
// It changes nothing, but a rejection's findings may still help.
func StaleNote(verifier string, pass bool, findings string, lab Label, version int) string {
	after := fmt.Sprintf("v%d superseded it", version)
	if version == 0 {
		after = "verification was turned off"
	}
	if pass {
		return fmt.Sprintf("`%s` passed your conclusion (%s) after %s: a stale verdict, which doesn't count.",
			verifier, lab, after)
	}
	return fmt.Sprintf("`%s` rejected your conclusion (%s) after %s: a stale verdict, so not a round. Its findings, "+
		"in case they still help:\n\n%s", verifier, lab, after, util.TruncateRunes(findings, maxCandidateRunes))
}

// LatestWith says where the agent's latest conclusion stands.
func LatestWith(verifier string, w Where) string {
	return fmt.Sprintf("Your latest conclusion (%s) is with `%s`.", w, verifier)
}

// WaitForLatest is what to do about it while working: wait for its verdict.
const WaitForLatest = "Call conclude() to wait for its verdict."

// Withdrawn is a conclude call's result when its candidate (withdrawn) was
// withdrawn while it waited: by version, a newer version, or (version 0) by
// verification turned off. Nothing was delivered.
func Withdrawn(withdrawn Label, version int) string {
	if withdrawn.Version == 0 {
		return "Not concluded: your verification changed while you waited, so your conclusion was withdrawn, and " +
			"nothing was delivered. Call conclude(result) to conclude."
	}
	if version > 0 {
		return fmt.Sprintf("Not concluded: superseded by v%d; your conclusion for v%d was withdrawn, and nothing was "+
			"delivered. Call conclude(result) to submit your result for v%d.", version, withdrawn.Version, version)
	}
	return fmt.Sprintf("Not concluded: verification was turned off; your conclusion for v%d was withdrawn, and nothing "+
		"was delivered. Call conclude(result) to deliver your result as it is.", withdrawn.Version)
}

// Stopped is the agent's message (or a conclude call's result) when its
// verifier stopped without a verdict on its conclusion (lab); stop is its
// status, and why if known ("crashed: ...").
func Stopped(verifier string, lab Label, stop string) string {
	return fmt.Sprintf("Your verifier `%s` stopped (%s) without a verdict on your conclusion (%s); that isn't a "+
		"round. Call conclude() to re-engage it with your conclusion, or conclude(result) to submit a new one.",
		verifier, short(stop), lab)
}

// Stale is a stale record's reason: the verdict was on candidate lab (the one
// given to the verifier last before the verdict arrived), withdrawn by
// version, a newer version, or (0) by verification turned off.
func Stale(lab Label, version int) string {
	if version > 0 {
		return fmt.Sprintf("for %s, which v%d superseded", lab, version)
	}
	return fmt.Sprintf("for %s, withdrawn when verification was turned off", lab)
}

// VerifierTask is the task of a verifier the harness starts for agent.
// request is the agent's current request, criteria may be empty; lab names
// the candidate. typed: the verifier gives a typed verdict (the verifier agent
// type, whose prompt says how to check); else its result's first line is read.
func VerifierTask(agent, request, criteria, candidate string, lab Label, typed bool) string {
	check := "Check it independently. Read the agent's trajectory with the session tools, inspect the workspace, and " +
		"run whatever commands you need (tests, builds). Don't modify the workspace; if you need to change files to " +
		"check something, work in a linked workspace or a copy.\n\n"
	if typed {
		check = ""
	}
	return fmt.Sprintf("You are verifying the conclusion of agent `%[1]s` (session `%[1]s` in this run) before it is "+
		"delivered.\n\n%[2]s%[3]s", agent, requestBlock(request, criteria, candidate, lab), check+verdictRule(typed))
}

// requestBlock is a version's request, its criteria and a candidate, as a
// verifier reads them.
func requestBlock(request, criteria, candidate string, lab Label) string {
	if criteria == "" {
		criteria = "the conclusion correctly and completely answers the request"
	}
	return fmt.Sprintf("Request (v%d): %s\n\nCriteria: %s\n\n%s%s\n\n", lab.Version,
		util.TruncateRunes(request, maxRequestRunes), criteria, candidateHeader(lab),
		util.TruncateRunes(candidate, maxCandidateRunes))
}

// verdictRule says how a verifier gives its verdict.
func verdictRule(typed bool) string {
	if typed {
		return "When you have checked it, give your verdict with conclude(verdict: \"PASS\" or \"FAIL\", findings): " +
			"for a PASS, the evidence; for a FAIL, concretely what is missing or wrong."
	}
	return "The first line of your result (what you conclude with) must be exactly `PASS` or `FAIL`, or start with " +
		"`PASS:` or `FAIL:`, followed by your findings. For a FAIL, say concretely what is missing or wrong."
}

// SameCandidate reports whether msg, a message giving a verifier a candidate
// (Delivery or NewVersion), gives the one prev, an earlier such message or its
// task, gave: the same label and text, as when it is re-engaged after a stop.
func SameCandidate(prev, msg string) bool {
	i := strings.Index(msg, candidateLead)
	if i < 0 {
		return false
	}
	given := msg[i:]
	for _, typed := range []bool{true, false} {
		given = strings.TrimSuffix(given, verdictRule(typed))
	}
	return strings.Contains(prev, given)
}

// candidateHeader introduces candidate lab in the messages that give one.
func candidateHeader(lab Label) string { return fmt.Sprintf("%s (%s):\n", candidateLead, lab) }

const candidateLead = "Candidate conclusion"

// Delivery re-engages a verifier with a newer candidate of the version it
// has the request of (typed: as for VerifierTask).
func Delivery(agent string, round int, candidate string, lab Label, typed bool) string {
	return fmt.Sprintf("[system] Agent `%s` concluded again: %s (round %d). Verify this candidate against the same "+
		"request and criteria; it replaces any earlier one.\n\n%s%s\n\n%s", agent, lab, round, candidateHeader(lab),
		util.TruncateRunes(candidate, maxCandidateRunes), verdictRule(typed))
}

// NewVersion gives a kept verifier the first candidate of a version it hasn't
// seen the request of: the request and criteria come with it.
func NewVersion(agent, request, criteria, candidate string, lab Label, typed bool) string {
	return fmt.Sprintf("[system] Agent `%s`'s verification has a new version: v%d, which supersedes anything you had "+
		"before. Verify this candidate against v%d's request and criteria; what you learned so far may help, but check "+
		"its claims anew.\n\n%s%s", agent, lab.Version, lab.Version, requestBlock(request, criteria, candidate, lab),
		verdictRule(typed))
}

// Withdrawal tells a verifier that the candidate it has (lab) was withdrawn
// by version, a newer version, or (0) by verification turned off: it stops
// and waits for the next one, keeping what it learned.
func Withdrawal(agent string, lab Label, version int) string {
	return fmt.Sprintf("[system] v%d is withdrawn; stop and wait for the next candidate. Agent `%s`'s %s, so a verdict "+
		"on it would be ignored. Do not call conclude: end your turn with a brief plain-text reply. amplio wakes you "+
		"when the next candidate arrives, and you keep what you have learned.", lab.Version, agent, WithdrawnBy(lab, version))
}

// WithdrawnBy says what withdrew candidate lab: version, a newer version, or
// (0) verification turned off. It is a withdrawn record's reason.
func WithdrawnBy(lab Label, version int) string {
	if version > 0 {
		return fmt.Sprintf("%s was superseded by v%d", lab, version)
	}
	return fmt.Sprintf("verification was turned off, which withdrew %s", lab)
}

// StandingBy answers a verifier that ends its turn after its candidate was
// withdrawn: nothing is delivered.
const StandingBy = "Standing by: your candidate was withdrawn, so nothing is delivered. The next candidate wakes you."

// VerifiedNote is the parent's note on a verified conclusion.
func VerifiedNote(verifier string, round int) event.ResultNote {
	return event.ResultNote{Kind: event.NoteVerified, Text: fmt.Sprintf("%s (round %d)", verifier, round)}
}

// NotVerifiedNote is the parent's note on a conclusion whose verification ran
// out of rounds: the latest findings, shortened.
func NotVerifiedNote(rejections int, findings string) event.ResultNote {
	return event.ResultNote{Kind: event.NoteNotVerified, Text: fmt.Sprintf("after %s. Latest findings: %s",
		Rejections(rejections), short(findings))}
}

// Rejections counts rejections in words: "1 rejection", "2 rejections".
func Rejections(n int) string {
	if n == 1 {
		return "1 rejection"
	}
	return fmt.Sprintf("%d rejections", n)
}

// UnstartedNote is the parent's note when no verifier could be started or
// re-engaged.
func UnstartedNote(err error) event.ResultNote {
	return event.ResultNote{Kind: event.NoteNotVerified, Text: "the verifier could not be started: " + err.Error()}
}

// StoppedNote is the parent's note when the verifier stopped MaxStops times
// in a row without a verdict; last is the latest stop's verdict and reason.
func StoppedNote(stops int, last string) event.ResultNote {
	return event.ResultNote{Kind: event.NoteNotVerified, Text: fmt.Sprintf(
		"the verifier stopped %d times without a verdict; last: %s", stops, short(last))}
}
