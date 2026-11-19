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

package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/session"
	"amplio/internal/verify"
)

type stubAgent struct{}

func (stubAgent) Run(context.Context) error { return nil }
func (stubAgent) SessionID() string         { return "" }

func init() {
	agent.Register("loop_test_chat", func(*agent.Env, *agent.Config) (agent.Agent, error) { return stubAgent{}, nil },
		agent.Traits{Interactive: true})
}

const everyMinute = "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=1"

// stream is main-agent's verification changes and ticks, in stream order:
// "set by loop 1", "tick 1 v1".
func (e *env) stream() []string {
	var out []string
	for _, ev := range e.events(sess) {
		switch x := ev.(type) {
		case *event.VerifyEvent:
			if x.Action == event.VerifySet || x.Action == event.VerifyReplace || x.Action == event.VerifyClear {
				out = append(out, x.Action+" by "+x.By)
			}
		case *event.UserEvent:
			if x.Loop != nil {
				out = append(out, fmt.Sprintf("tick %d v%d", x.Loop.N, x.Loop.Version))
			}
		}
	}
	return out
}

func (e *env) wantStream(want ...string) {
	e.t.Helper()
	if got := e.stream(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		e.t.Fatalf("at +%s: stream %q\nwant %q", e.clock.Now().Sub(t0), got, want)
	}
}

func (e *env) addVerifying(v *event.LoopVerify) Info {
	e.t.Helper()
	info, err := e.s.Add(e.ctx, run, sess, "run the tests", sched(everyMinute), v)
	if err != nil {
		e.t.Fatal(err)
	}
	return info
}

func (e *env) loadVerify() verify.State {
	e.t.Helper()
	st, err := verify.Load(e.ctx, e.store, run, sess)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// Each tick of a loop that verifies its ticks is a new task: just before its
// message, the loop starts a new version of the session's verification (a set
// after the last one passed, a replace while one is on), and the tick names
// it. The request survives a restart.
func TestScheduler_VerifyEachTick(t *testing.T) {
	e := newEnv(t)
	v := &event.LoopVerify{Criteria: "the tests pass", Model: "vertex:judge", MaxRounds: 2}
	info := e.addVerifying(v)
	if info.Verify == nil || *info.Verify != *v {
		t.Fatalf("Add = %+v; want its verification", info)
	}
	if set := lastLoopEvent(t, e.loopEvents(sess)); set.Verify == nil || *set.Verify != *v {
		t.Errorf("the set records %+v; want the verification", set.Verify)
	}
	if m := e.mirrored(sess); m[0].Verify == nil || *m[0].Verify != *v {
		t.Errorf("mirrored %+v", m[0])
	}
	e.clock.Advance(0)
	e.wantStream("set by loop 1", "tick 1 v1")
	// The tick's conclusion passed: the next version is a set again.
	if _, err := e.store.AppendEvent(e.ctx, run, sess, &event.VerifyEvent{Action: event.VerifyPassed, By: event.ByHarness}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.clock.Advance(time.Minute)
	e.wantStream("set by loop 1", "tick 1 v1", "set by loop 1", "tick 2 v2", "replace by loop 1", "tick 3 v3")
	if got, want := e.ticks()[2].Content, "[loop 1 · every 1m · tick 3 · verification v3] run the tests"; got != want {
		t.Errorf("tick content = %q; want %q", got, want)
	}
	st := e.loadVerify()
	if r := st.Request; !st.On || st.Version != 3 || r.Criteria != v.Criteria || r.Model != v.Model || r.MaxRounds != 2 || r.By != "loop 1" {
		t.Errorf("verification after three ticks: version %d, request %+v", st.Version, r)
	}

	e.restart()
	e.clock.Advance(time.Minute)
	if got := e.stream(); got[len(got)-2] != "replace by loop 1" || got[len(got)-1] != "tick 4 v4" {
		t.Errorf("after a restart: %q", got)
	}
}

// A loop's ticks are verified only where verification applies: not on an
// agent that never concludes, and not by a scheduler that can't start
// versions.
func TestScheduler_VerifyEachTickRefused(t *testing.T) {
	e := newEnv(t)
	if err := e.store.CreateSession(e.ctx, db.SessionRecord{RunID: run, SessionID: "chat", AgentType: "loop_test_chat",
		Status: db.SessionIdle}); err != nil {
		t.Fatal(err)
	}
	v := &event.LoopVerify{MaxRounds: 3}
	if _, err := e.s.Add(e.ctx, run, "chat", "p", sched(everyMinute), v); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "interactive") {
		t.Errorf("Add on an interactive agent = %v", err)
	}
	info := e.addSpec("chat", "p", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
	if _, err := e.s.Edit(e.ctx, run, "chat", info.ID, Change{Verify: v}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "interactive") {
		t.Errorf("Edit on an interactive agent = %v", err)
	}
	if got := e.loopEvents("chat"); len(got) != 1 {
		t.Errorf("refused changes wrote %+v", got)
	}

	bare := New(e.store, Options{Clock: e.clock, Location: time.UTC})
	if err := bare.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	defer bare.Shutdown()
	if _, err := bare.Add(e.ctx, run, sess, "p", sched(everyMinute), v); !errors.Is(err, ErrInvalid) {
		t.Errorf("Add without a VerifyFunc = %v", err)
	}
}

// Edit makes a loop verify its ticks, changes the request, and stops, each
// from the next tick on. Once it stops, its next tick turns the loop's own
// version off, so that tick isn't checked against what the loop used to ask;
// a version someone else set is left alone.
func TestScheduler_EditVerify(t *testing.T) {
	e := newEnv(t)
	info := e.addVerifying(nil)
	e.clock.Advance(0)
	e.wantStream("tick 1 v0")
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{NoVerify: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("stopping a loop that doesn't verify = %v; want nothing to change", err)
	}

	v := &event.LoopVerify{MaxRounds: 1}
	got, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Verify: v})
	if err != nil || got.Verify == nil || *got.Verify != *v {
		t.Fatalf("Edit verify = %+v, %v", got, err)
	}
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Verify: v, NoVerify: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("verify and stop verifying together = %v", err)
	}
	if edit := lastLoopEvent(t, e.loopEvents(sess)); edit.Action != event.LoopEdit || edit.Verify == nil || *edit.Verify != *v ||
		edit.VerifyStopped || edit.Prompt != "run the tests" || edit.Summary != "every 1m" {
		t.Errorf("the edit records %+v", edit)
	}
	e.clock.Advance(time.Minute)
	e.wantStream("tick 1 v0", "set by loop 1", "tick 2 v1")

	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{NoVerify: true}); err != nil {
		t.Fatal(err)
	}
	if edit := lastLoopEvent(t, e.loopEvents(sess)); edit.Verify != nil || !edit.VerifyStopped {
		t.Errorf("the edit that stops verifying records %+v", edit)
	}
	if m := e.mirrored(sess); m[0].Verify != nil {
		t.Errorf("mirrored after stopping %+v", m[0].Verify)
	}
	e.clock.Advance(time.Minute)
	e.wantStream("tick 1 v0", "set by loop 1", "tick 2 v1", "clear by loop 1", "tick 3 v0")
	if got, want := e.ticks()[2].Content, "[loop 1 · every 1m · tick 3] run the tests"; got != want {
		t.Errorf("a tick that started no version = %q; want %q", got, want)
	}
	if st := e.loadVerify(); st.On {
		t.Errorf("the loop's version is still on after it stopped verifying: v%d", st.Version)
	}
	e.clock.Advance(time.Minute)
	e.wantStream("tick 1 v0", "set by loop 1", "tick 2 v1", "clear by loop 1", "tick 3 v0", "tick 4 v0")

	// A version the operator set after the loop's last verifying tick isn't the
	// loop's to turn off.
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Verify: v}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{NoVerify: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Set(e.ctx, e.store, nil, run, sess, &event.VerifyEvent{By: event.ByOperator, MaxRounds: 2}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if got := e.stream(); strings.Join(got[len(got)-4:], ", ") != "set by loop 1, tick 5 v2, replace by operator, tick 6 v0" {
		t.Errorf("stream %q", got)
	}
	if st := e.loadVerify(); !st.On || st.Request.By != event.ByOperator {
		t.Errorf("the operator's version after the loop's unverified tick: on %v, by %q", st.On, st.Request.By)
	}

	for _, c := range []Change{{}, {Verify: v, NoVerify: true}} {
		if _, err := e.s.Edit(e.ctx, run, sess, info.ID, c); !errors.Is(err, ErrInvalid) {
			t.Errorf("Edit(%+v) = %v", c, err)
		}
	}
}

// A session has one verification, so only one of its loops can verify its
// ticks: a second is refused, on Add and on Edit, until the first stops.
func TestScheduler_OneVerifyingLoopPerSession(t *testing.T) {
	e := newEnv(t)
	first := e.addVerifying(&event.LoopVerify{Criteria: "A", MaxRounds: 3})
	if _, err := e.s.Add(e.ctx, run, sess, "p", sched(everyMinute), &event.LoopVerify{Criteria: "B"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "loop 1 already verifies each tick") {
		t.Errorf("a second verifying loop = %v", err)
	}
	second := e.addVerifying(nil)
	if _, err := e.s.Edit(e.ctx, run, sess, second.ID, Change{Verify: &event.LoopVerify{Criteria: "B"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Edit to a second verifying loop = %v", err)
	}
	if _, err := e.s.Edit(e.ctx, run, sess, first.ID, Change{Verify: &event.LoopVerify{Criteria: "A2"}}); err != nil {
		t.Errorf("Edit of the verifying loop itself = %v", err)
	}
	if _, err := e.s.Edit(e.ctx, run, sess, first.ID, Change{NoVerify: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Edit(e.ctx, run, sess, second.ID, Change{Verify: &event.LoopVerify{Criteria: "B"}}); err != nil {
		t.Errorf("Edit once the first stopped = %v", err)
	}
	if err := e.s.Stop(e.ctx, run, sess, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Add(e.ctx, run, sess, "p", sched(everyMinute), &event.LoopVerify{Criteria: "C"}); err != nil {
		t.Errorf("Add once the verifying loop stopped = %v", err)
	}
}

// A tick that comes while the previous tick's conclusion is with the verifier
// starts its version and the tick in one write (whoever is woken reads both),
// then withdraws that candidate: the verifier, still checking, is told to stop
// and wait.
func TestScheduler_TickWithdrawsAPendingCandidate(t *testing.T) {
	e := newEnv(t)
	if err := e.store.CreateSession(e.ctx, db.SessionRecord{RunID: run, SessionID: "checker", ParentID: sess,
		AgentType: verify.AgentType, Status: db.SessionOngoing, Metadata: map[string]any{verify.MetadataVerifierFor: sess}}); err != nil {
		t.Fatal(err)
	}
	e.addVerifying(&event.LoopVerify{MaxRounds: 3})
	e.clock.Advance(0)
	// Tick 1's conclusion goes to the verifier, which is still at it when tick 2 fires.
	if _, err := e.store.AppendEvent(e.ctx, run, sess, &event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness,
		Verifier: "checker", Round: 1}); err != nil {
		t.Fatal(err)
	}
	if st := e.loadVerify(); !st.Pending {
		t.Fatal("no candidate pending")
	}
	// When the replace is committed, its tick is too.
	var tickWithReplace []bool
	e.store.SetCommitListener(func(runID, sessionID string, evt event.Event) {
		if v, ok := evt.(*event.VerifyEvent); ok && v.Action == event.VerifyReplace {
			tickWithReplace = append(tickWithReplace, e.tickCount(sess) == 2)
		}
	})
	e.clock.Advance(time.Minute)
	e.store.SetCommitListener(nil)
	if len(tickWithReplace) != 1 || !tickWithReplace[0] {
		t.Errorf("tick 2 committed with its version: %v", tickWithReplace)
	}
	var tail []string
	for _, ev := range e.events(sess) {
		switch x := ev.(type) {
		case *event.VerifyEvent:
			tail = append(tail, x.Action)
		case *event.UserEvent:
			if x.Loop != nil {
				tail = append(tail, fmt.Sprintf("tick %d v%d", x.Loop.N, x.Loop.Version))
			}
		}
	}
	if got := strings.Join(tail, ", "); got != "set, tick 1 v1, verifying, replace, tick 2 v2, withdrawn" {
		t.Errorf("stream: %s", got)
	}
	var told string
	for _, ev := range e.events("checker") {
		if u, ok := ev.(*event.UserEvent); ok && u.Check == event.CheckWithdrawn {
			told = u.Content
		}
	}
	if !strings.Contains(told, "v1 is withdrawn; stop and wait for the next candidate") {
		t.Errorf("the verifier was told %q", told)
	}
}

// A tick that can't be written leaves no version behind, and its retry starts
// the one it names; a tick whose version can't be started isn't sent, and its
// retries back off; one whose verification change reports an error after the
// write goes out with its version. A tick that can't be sent ends its loop
// once the schedule has nothing left and it has been due for an hour.
func TestScheduler_VerifyTickRetries(t *testing.T) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store}
	e.restartWith(fs)
	e.addVerifying(&event.LoopVerify{MaxRounds: 3})
	e.clock.Advance(0)
	e.wantStream("set by loop 1", "tick 1 v1")

	fs.set(func(f *faultStore) { f.failTick = errors.New("disk on fire") })
	e.clock.Advance(time.Minute)
	e.wantStream("set by loop 1", "tick 1 v1")
	fs.set(func(f *faultStore) { f.failTick = nil })
	e.clock.Advance(time.Minute)
	e.wantStream("set by loop 1", "tick 1 v1", "replace by loop 1", "tick 2 v2")

	e.mu.Lock()
	e.afterErr = errors.New("withdrawing the candidate failed")
	e.mu.Unlock()
	e.clock.Advance(time.Minute)
	e.wantStream("set by loop 1", "tick 1 v1", "replace by loop 1", "tick 2 v2", "replace by loop 1", "tick 3 v3")
	e.mu.Lock()
	e.afterErr, e.verifyErr = nil, errors.New("no verification today")
	e.mu.Unlock()
	// Tick 4 is due at 12:04; its retries come 1, 2 and 4 minutes apart.
	e.clock.Advance(t0.Add(4 * time.Minute).Sub(e.clock.Now()))
	for _, at := range []time.Duration{5, 7, 11} {
		if got := e.s.List(run, sess)[0].NextTick; !got.Equal(t0.Add(at * time.Minute)) {
			t.Fatalf("at %s, next try %s; want 12:%02d", e.clock.Now().Format("15:04"), got.Format("15:04"), at)
		}
		e.clock.Advance(t0.Add(at * time.Minute).Sub(e.clock.Now()))
		e.wantTicks(3)
	}
	e.wantStream("set by loop 1", "tick 1 v1", "replace by loop 1", "tick 2 v2", "replace by loop 1", "tick 3 v3")
}

// A loop whose ticks can't be sent at all ends once its schedule has nothing
// left and its last tick has been due for an hour.
func TestScheduler_UnsendableTicksEndTheLoop(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.verifyErr = errors.New("no verification today")
	e.mu.Unlock()
	if _, err := e.s.Add(e.ctx, run, sess, "p", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;COUNT=2"), &event.LoopVerify{}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(59 * time.Minute)
	if len(e.s.List(run, sess)) != 1 {
		t.Fatal("ended before its last tick had been due for an hour")
	}
	e.clock.Advance(2 * time.Hour)
	if got := e.s.List(run, sess); len(got) != 0 {
		t.Fatalf("still armed: %+v", got)
	}
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEnded || !strings.Contains(last.Reason, "couldn't be sent: no verification today") {
		t.Errorf("ended with %+v", last)
	}
	e.wantTicks(0)
}

// A cancel that lands while a tick that starts a version is on its way, held
// in its write under the verification lock, stops both: the version and the
// tick are one write, which re-checks the ancestors and the loop. Neither a
// session cancel nor a run cancel (which ends the run's loops first) lets the
// tick land, or changes the verification.
func TestScheduler_CancelDuringVerifyingTick(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cancel       func(e *env)
	}{
		{"session", EndedSessionCancelled, func(e *env) {
			if err := session.CancelSession(e.store, nil, run, sess, "cancelled via UI"); err != nil {
				e.t.Error(err)
			}
		}},
		{"run", EndedRunCancelled, func(e *env) {
			e.s.EndRun(e.ctx, run, EndedRunCancelled)
			if err := session.CancelSession(e.store, nil, run, sess, "cancelled via UI"); err != nil {
				e.t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.setStatus(db.SessionAwaiting)
			fs := &faultStore{Store: e.store, blockSession: sess, block: make(chan struct{}), entered: make(chan struct{}, 1)}
			e.restartWith(fs)
			info := e.addVerifying(&event.LoopVerify{Criteria: "the tests pass"})
			ticked := make(chan struct{})
			go func() { defer close(ticked); e.clock.Advance(0) }()
			<-fs.entered
			cancelled := make(chan struct{})
			go func() { defer close(cancelled); tc.cancel(e) }()
			k := key{run, sess, info.ID}
			waitFor(t, "the cancel to land", func() bool {
				sr, err := e.store.GetSession(e.ctx, run, sess)
				e.s.mu.Lock()
				defer e.s.mu.Unlock()
				return err == nil && sr.Status == db.SessionCancelled || e.s.loops[k] != nil && e.s.loops[k].ending
			})
			close(fs.block)
			<-ticked
			<-cancelled
			waitFor(t, "the loop to end", func() bool { return len(e.s.List(run, sess)) == 0 })
			e.wantStream()
			if st := e.loadVerify(); st.On || st.Version != 0 {
				t.Errorf("verification after the cancel: on %v, version %d", st.On, st.Version)
			}
			if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEnded || last.Reason != tc.reason {
				t.Errorf("last loop event %+v", last)
			}
		})
	}
}
