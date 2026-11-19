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

// Package verify is verified conclusions: a standing request, set on an
// autonomous session by the operator or its parent, that the session's next
// conclusion pass an independent verifier before it is delivered.
//
// The state lives in the session's stream as event.VerifyEvents (set, replace
// and clear by the requester; verifying, rejected, passed, exhausted,
// withdrawn and stale by the harness; verdict by the verifier's terminal
// write), so it needs no recovery after a restart. Every set or replace starts
// a new version (v1, v2, …), counted from the stream; a verdict counts only
// for the latest candidate of the current version. The verifier is an
// ordinary sub-agent of the verified session, marked with the session metadata
// MetadataVerifierFor, kept across versions; the conclusion check that starts
// and re-engages it is in the event loop (internal/agent/eventloop/verify.go).
// This package holds what the event loop, the tools, the API and the CLI
// share: requests, the state derived from a stream, the verifier's verdict,
// and the changes.
package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/session"
)

// MetadataVerifierFor is the session-metadata key marking a verifier; its
// value is the session it verifies, its parent.
const MetadataVerifierFor = db.MetadataVerifierFor

// AgentType is the agent type of the verifiers the harness starts
// (internal/agent/verifier): it concludes with a typed verdict.
const AgentType = "verifier"

// VerifierFor returns the session that s verifies, or "" if s isn't a
// verifier.
func VerifierFor(s *db.SessionRecord) string {
	v, _ := s.Metadata[MetadataVerifierFor].(string)
	return v
}

// Request asks for a session's conclusion to be verified, as the API, the
// tools, the CLI and the headless flags take it. Empty fields fall back to
// the [verify] defaults.
type Request struct {
	Criteria  string `json:"criteria,omitempty" jsonschema_description:"What the verifier checks the conclusion against (default: the agent's current request)"`
	Model     string `json:"model,omitempty" jsonschema_description:"The verifier's model: a spec or a #nickname (default: the configured verify model, else the agent's own)"`
	MaxRounds *int   `json:"max_rounds,omitempty" jsonschema_description:"How many rejections before the agent concludes anyway, marked not verified (default: the configured limit, 3; 0 = unbounded)"`
}

// UnmarshalJSON refuses a field Request doesn't have, so a misspelt one
// ("criterion", "max_round") is an error rather than a default silently used
// in its place.
func (r *Request) UnmarshalJSON(data []byte) error {
	type fields Request // without this method
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f fields
	if err := dec.Decode(&f); err != nil {
		return err
	}
	*r = Request(f)
	return nil
}

// ModelResolver turns a model name into the spec it means (agent.Env.ResolveLLM).
type ModelResolver func(ctx context.Context, model string) (spec string, provider llm.Provider, err error)

// Resolve fills r's empty fields from the defaults and checks them, giving the
// event that turns verification on (Set decides between set and replace). by
// is who asks: event.ByOperator or the parent's session id. A model is
// resolved to its spec, so a bad name is refused now rather than when the
// agent concludes; resolve may be nil where no model other than the agent's
// own can be used.
func (r Request) Resolve(ctx context.Context, by string, d config.VerifyDefaults, resolve ModelResolver) (*event.VerifyEvent, error) {
	e := &event.VerifyEvent{Action: event.VerifySet, Criteria: strings.TrimSpace(r.Criteria),
		Model: strings.TrimSpace(r.Model), MaxRounds: d.MaxRounds, By: by}
	if r.MaxRounds != nil {
		if *r.MaxRounds < 0 {
			return nil, fmt.Errorf("max_rounds = %d: want 0 (unbounded) or more", *r.MaxRounds)
		}
		e.MaxRounds = *r.MaxRounds
	}
	if e.Model == "" {
		e.Model = d.Model
	}
	if e.Model != "" {
		if resolve == nil {
			return nil, fmt.Errorf("verify model %q: this server can only verify on the agent's own model", e.Model)
		}
		spec, _, err := resolve(ctx, e.Model)
		if err != nil {
			return nil, fmt.Errorf("verify model: %w", err)
		}
		e.Model = spec
	}
	return e, nil
}

// ForLoop resolves r for a loop that verifies each tick, as Resolve does: the
// defaults are filled in and the model resolved when the loop is set, so a bad
// request is refused then, and every tick asks for the same.
func (r Request) ForLoop(ctx context.Context, d config.VerifyDefaults, resolve ModelResolver) (*event.LoopVerify, error) {
	e, err := r.Resolve(ctx, "", d, resolve)
	if err != nil {
		return nil, err
	}
	return &event.LoopVerify{Criteria: e.Criteria, Model: e.Model, MaxRounds: e.MaxRounds}, nil
}

// CanVerify reports why a session can't be verified, or nil if it can:
// verification applies at the transition to concluded, which interactive
// agents never make, and a verifier is never verified itself.
func CanVerify(s *db.SessionRecord) error {
	if agent.IsInteractive(s.AgentType) {
		return fmt.Errorf("%s is an interactive agent: it never concludes, so there is nothing to verify", s.SessionID)
	}
	if v := VerifierFor(s); v != "" {
		return fmt.Errorf("%s is the verifier of %s: a verifier can't be verified", s.SessionID, v)
	}
	return nil
}

// ErrNotParent refuses a change to a session's verification from an agent
// other than its parent: otherwise an agent could turn off its own.
var ErrNotParent = errors.New("only a session's parent (or the operator) can change its verification")

// CheckParent returns ErrNotParent unless caller is target's parent.
func CheckParent(target *db.SessionRecord, caller string) error {
	if target.ParentID == "" || target.ParentID != caller {
		return fmt.Errorf("%w; %s's parent is %s", ErrNotParent, target.SessionID, parentName(target))
	}
	return nil
}

func parentName(s *db.SessionRecord) string {
	if s.ParentID == "" {
		return "the operator (it is a root agent)"
	}
	return s.ParentID
}

// Set starts a new version of a session's verification with e: a set if it
// was off, else a replace, which supersedes the current version. It returns
// the new version's number. Every version after a session's first goes
// through it (the first may also be written when the session starts, by
// spawn_agent(verify) or a new run's verify).
//
// The verifier is kept, with what it learned, and gets the version's first
// candidate when the agent next concludes. It is retired (cancelled; the next
// conclusion starts a new one) when the version asks for another model than
// the one it runs on, or when it ended the last version itself, by stopping
// too often or failing to start. A candidate the verifier had is withdrawn:
// see withdraw. The set or replace itself wakes no one (db.IsRecordOnly): the
// session reads it as a note at its next turn. An error after the set or
// replace is recorded says that the version started.
func Set(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID string, e *event.VerifyEvent) (int, error) {
	return SetThen(ctx, store, reg, runID, sessionID, e, nil)
}

// SetThen is Set, then then, still under the verification lock: after the
// change is in place but before the agent can act on it. A conclude call
// waiting for a verdict, which the withdrawal of its candidate wakes, settles
// under that lock, so it sees what then writes: the message that came with
// the change, which its turn then reads with it. then doesn't run if the
// change failed.
func SetThen(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID string, e *event.VerifyEvent,
	then func()) (int, error) {
	return setThen(ctx, store, reg, runID, sessionID, e, appendOne(ctx, store, runID, sessionID), then)
}

// Clear turns verification off, reporting whether it was on. A candidate the
// verifier had is withdrawn (see withdraw), so the agent's next conclusion
// goes through. The verifier is kept, idle, for a later version. An error
// after the clear is recorded says that verification is off.
func Clear(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID, by string) (bool, error) {
	return ClearThen(ctx, store, reg, runID, sessionID, by, nil)
}

// ClearThen is Clear, then then, still under the verification lock (see
// SetThen). then runs only if it cleared something.
func ClearThen(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID, by string, then func()) (bool, error) {
	if _, err := store.GetSession(ctx, runID, sessionID); err != nil {
		return false, fmt.Errorf("no session %q in run %q", sessionID, runID)
	}
	write := appendOne(ctx, store, runID, sessionID)
	return clearIfThen(ctx, store, runID, sessionID, by, func(State) bool { return true },
		func(e *event.VerifyEvent) error { return write(e, 0) }, then)
}

// Tick changes a session's verification for the next tick of loop loopID,
// recorded together with the tick: write appends the change (nil for none)
// and the tick, with the version the tick starts (0 for none), in one write,
// so nothing reads one without the other. With v, the tick starts a new
// version with it (see Set; by event.ByLoop(loopID)). Without, a version the
// loop started that is still in force is turned off (see Clear): the tick
// mustn't be checked against what the loop used to ask. It returns the version
// started. An error after write succeeded says that the change was made.
func Tick(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID, loopID string, v *event.LoopVerify,
	write func(change *event.VerifyEvent, version int) error) (int, error) {
	if v != nil {
		return setThen(ctx, store, reg, runID, sessionID, v.Event(loopID), write, nil)
	}
	by := event.ByLoop(loopID)
	wrote := false
	_, err := clearIfThen(ctx, store, runID, sessionID, by, func(st State) bool { return st.Request.By == by },
		func(e *event.VerifyEvent) error { wrote = true; return write(e, 0) }, nil)
	if !wrote && err == nil {
		err = write(nil, 0)
	}
	return 0, err
}

// appendOne writes a change to a session's verification on its own.
func appendOne(ctx context.Context, store db.Store, runID, sessionID string) func(*event.VerifyEvent, int) error {
	return func(e *event.VerifyEvent, _ int) error {
		_, err := store.AppendEvent(ctx, runID, sessionID, e)
		return err
	}
}

// setThen is Set, with the set or replace recorded by write (given the new
// version's number) under the verification lock, then then (see SetThen).
func setThen(ctx context.Context, store db.Store, reg *session.Registry, runID, sessionID string, e *event.VerifyEvent,
	write func(*event.VerifyEvent, int) error, then func()) (int, error) {
	sess, err := store.GetSession(ctx, runID, sessionID)
	if err != nil {
		return 0, fmt.Errorf("no session %q in run %q", sessionID, runID)
	}
	if err := CanVerify(sess); err != nil {
		return 0, err
	}
	defer Lock(runID, sessionID)()
	st, err := Load(ctx, store, runID, sessionID)
	if err != nil {
		return 0, err
	}
	e.Action = event.VerifySet
	if st.On {
		e.Action = event.VerifyReplace
	}
	version := st.Version + 1
	if err := write(e, version); err != nil {
		return 0, fmt.Errorf("record verification: %w", err)
	}
	retire := st.Verifier != "" && !st.keepsVerifier(e)
	// Withdrawn with the verifier it was given to, whether it stays or not.
	err = withdraw(ctx, store, runID, sessionID, st, version, !retire)
	if retire {
		cancelVerifier(store, reg, runID, st.Verifier, fmt.Sprintf("verification v%d, %s by %s, takes a new verifier",
			version, e.Action, e.By))
	}
	if err != nil {
		return version, fmt.Errorf("verification v%d started, but withdrawing v%d's candidate failed: %w", version,
			st.Version, err)
	}
	if then != nil {
		then()
	}
	return version, nil
}

// clearIfThen is Clear when verification is on and mine says so, with the
// clear recorded by write under the verification lock, then then (see
// ClearThen).
func clearIfThen(ctx context.Context, store db.Store, runID, sessionID, by string, mine func(State) bool,
	write func(*event.VerifyEvent) error, then func()) (bool, error) {
	defer Lock(runID, sessionID)()
	st, err := Load(ctx, store, runID, sessionID)
	if err != nil || !st.On || !mine(st) {
		return false, err
	}
	if err := write(&event.VerifyEvent{Action: event.VerifyClear, By: by}); err != nil {
		return false, fmt.Errorf("record verification: %w", err)
	}
	if err := withdraw(ctx, store, runID, sessionID, st, 0, true); err != nil {
		return true, fmt.Errorf("verification is off, but withdrawing v%d's candidate failed: %w", st.Version, err)
	}
	if then != nil {
		then()
	}
	return true, nil
}

// withdraw withdraws the candidate the verifier has, if any, right after the
// change that withdrew it (st: the state before it; version: the version it
// started, or 0 for a clear): it is never delivered. The withdrawn record
// wakes the agent if it waits for that candidate's verdict. A verdict the
// verifier gave on it that the agent hasn't acted on, for a version that is
// now over, is recorded as stale. A verifier still working on it is told to
// stop and wait for the next candidate (tell: it stays the verifier), not
// cancelled; one that is done gets no word, which would only revive it. The
// read and the message aren't one write with the verifier's own conclusion:
// one that concludes in between is revived by the message for a turn in which
// it stands by, and its verdict, late, is recorded stale at the agent's next
// read.
func withdraw(ctx context.Context, store db.Store, runID, sessionID string, st State, version int, tell bool) error {
	if !st.Pending {
		return nil
	}
	lab := st.Label()
	if _, err := store.AppendEvent(ctx, runID, sessionID, &event.VerifyEvent{Action: event.VerifyWithdrawn,
		By: event.ByHarness, Verifier: st.Verifier, Round: st.Round(), Reason: WithdrawnBy(lab, version)}); err != nil {
		return fmt.Errorf("record the withdrawal: %w", err)
	}
	if st.Verifier == "" {
		return nil
	}
	v, err := Evaluate(ctx, store, runID, st)
	switch {
	case err != nil:
		return err
	case v.Given:
		return RecordStale(ctx, store, runID, sessionID, v.Outcome(st.Verifier), lab, version)
	case !tell || v.Owed || v.Stopped != "" || v.Missing:
		return nil
	}
	if _, err := store.AppendEvent(ctx, runID, st.Verifier, &event.UserEvent{
		Content: Withdrawal(sessionID, lab, version), Check: event.CheckWithdrawn}); err != nil {
		return fmt.Errorf("tell verifier %s its candidate was withdrawn: %w", st.Verifier, err)
	}
	return nil
}

// RecordStale records a verifier's verdict (o) as stale: it was on candidate
// lab, which version (or, 0, verification turned off) withdrew before the
// harness acted on it, and it changes nothing.
func RecordStale(ctx context.Context, store db.Store, runID, sessionID string, o *event.VerifyOutcome, lab Label, version int) error {
	if _, err := store.AppendEvent(ctx, runID, sessionID, &event.VerifyEvent{Action: event.VerifyStale,
		By: event.ByHarness, Verifier: o.Verifier, Outcome: o, Reason: Stale(lab, version)}); err != nil {
		return fmt.Errorf("record a stale verdict: %w", err)
	}
	slog.Debug("a stale verdict, ignored", "run", runID, "session", sessionID, "verifier", o.Verifier, "verdict", o.Verdict)
	return nil
}

// cancelVerifier stops a verifier that is no longer wanted. Best effort: one
// that won't stop finishes its check, and the verdict, for a candidate that
// is no longer pending, is recorded as stale.
func cancelVerifier(store db.Store, reg *session.Registry, runID, verifier, reason string) {
	if verifier == "" {
		return
	}
	if err := session.CancelSession(store, reg, runID, verifier, reason); err != nil {
		slog.Warn("cancel a verifier no longer wanted", "run", runID, "verifier", verifier, "error", err)
	}
}
