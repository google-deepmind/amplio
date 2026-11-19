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
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/runtime"
	"amplio/internal/session"
	"amplio/internal/verify"
)

// fakeClock runs timers only when the test advances it, each at its due time
// plus latency (a timer that fires late).
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	latency time.Duration
	timers  []*fakeTimer
}

type fakeTimer struct {
	c    *fakeClock
	at   time.Time
	f    func()
	done bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// Advance moves the clock forward by d, running every timer that comes due on
// the way, in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.done && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			if target.After(c.now) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		if at := next.at.Add(c.latency); at.After(c.now) {
			c.now = at
		}
		next.done = true
		c.mu.Unlock()
		next.f()
	}
}

const (
	run  = "r1"
	sess = "main-agent"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type env struct {
	t     *testing.T
	ctx   context.Context
	store db.Store
	clock *fakeClock
	s     *Scheduler

	mu        sync.Mutex
	verifyErr error // startVersion fails with it, writing nothing
	afterErr  error // startVersion fails with it after the write
}

// startVersion is the scheduler's VerifyFunc: verify.Tick on the store.
func (e *env) startVersion(ctx context.Context, runID, sessionID, loopID string, v *event.LoopVerify,
	write func(*event.VerifyEvent, int) error) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.verifyErr != nil {
		return 0, e.verifyErr
	}
	n, err := verify.Tick(ctx, e.store, nil, runID, sessionID, loopID, v, write)
	if err == nil {
		err = e.afterErr
	}
	return n, err
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: run}); err != nil {
		t.Fatal(err)
	}
	store.SetCommitListener(reviveOnTick(store))
	e := &env{t: t, ctx: ctx, store: store, clock: &fakeClock{now: t0}}
	e.addSession(sess)
	e.restart()
	return e
}

// reviveOnTick stands in for the commit notifier: the session reads each tick
// at its next step, revived if it was at rest.
func reviveOnTick(store db.Store) func(runID, sessionID string, evt event.Event) {
	return func(runID, sessionID string, evt event.Event) {
		if u, ok := evt.(*event.UserEvent); ok && u.Loop != nil {
			_, _ = store.AdvanceStep(context.Background(), runID, sessionID)
		}
	}
}

func (e *env) addSession(id string) { e.addSessionAs(id, db.SessionConcluded, "") }

func (e *env) addSessionAs(id, status, parent string) {
	e.t.Helper()
	if err := e.store.CreateSession(e.ctx, db.SessionRecord{RunID: run, SessionID: id, Status: status, ParentID: parent}); err != nil {
		e.t.Fatal(err)
	}
}

// setStatus sets main-agent's status; the scheduler doesn't hear of it unless a test installs its listener.
func (e *env) setStatus(status string) {
	e.t.Helper()
	if err := e.store.UpdateSessionStatus(e.ctx, run, sess, status); err != nil {
		e.t.Fatal(err)
	}
}

// waitFor polls cond for up to two seconds (for work done on another goroutine).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recv waits for a value on ch (or its close), failing the test after a while
// instead of hanging it.
func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// restart replaces the scheduler with a fresh one on the same store, as a
// server restart does.
func (e *env) restart() {
	e.t.Helper()
	if e.s != nil {
		e.s.Shutdown()
	}
	e.s = New(e.store, Options{Clock: e.clock, Location: time.UTC, Verify: e.startVersion})
	if err := e.s.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) add(prompt string, f Form) Info {
	e.t.Helper()
	s, err := f.Schedule(e.clock.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return e.addSpec(sess, prompt, s.Spec)
}

func (e *env) addSpec(session, prompt, spec string) Info {
	e.t.Helper()
	info, err := e.s.Add(e.ctx, run, session, prompt, sched(spec), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return info
}

func (e *env) events(session string) []event.Event {
	e.t.Helper()
	recs, err := e.store.GetEvents(e.ctx, run, session, db.EventFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]event.Event, len(recs))
	for i, r := range recs {
		out[i] = r.Event
	}
	return out
}

// tickCount counts a session's ticks.
func (e *env) tickCount(session string) int {
	n := 0
	for _, ev := range e.events(session) {
		if u, ok := ev.(*event.UserEvent); ok && u.Loop != nil {
			n++
		}
	}
	return n
}

func (e *env) ticks() []*event.UserEvent {
	var out []*event.UserEvent
	for _, ev := range e.events(sess) {
		if u, ok := ev.(*event.UserEvent); ok && u.Loop != nil {
			out = append(out, u)
		}
	}
	return out
}

func (e *env) loopEvents(session string) []*event.LoopEvent {
	var out []*event.LoopEvent
	for _, ev := range e.events(session) {
		if l, ok := ev.(*event.LoopEvent); ok {
			out = append(out, l)
		}
	}
	return out
}

func (e *env) wantTicks(n int) {
	e.t.Helper()
	if got := len(e.ticks()); got != n {
		e.t.Fatalf("at +%s: %d ticks; want %d", e.clock.Now().Sub(t0), got, n)
	}
}

// mirrored returns the loops mirrored in the session's metadata.
func (e *env) mirrored(session string) []Loop {
	e.t.Helper()
	sr, err := e.store.GetSession(e.ctx, run, session)
	if err != nil {
		e.t.Fatal(err)
	}
	loops, err := decodeLoops(sr.Metadata)
	if err != nil {
		e.t.Fatal(err)
	}
	return loops
}

func lastLoopEvent(t *testing.T, evs []*event.LoopEvent) *event.LoopEvent {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no loop events")
	}
	return evs[len(evs)-1]
}

// The live-run shape: every minute, the first tick at once, three in all.
func TestScheduler_SendNowThreeTimes(t *testing.T) {
	e := newEnv(t)
	info := e.add("report the eval", Form{Every: time.Minute, SendNow: true, Times: 3})
	if info.ID != "1" || !info.NextTick.Equal(t0) || info.Summary != "every 1m, 3 times" {
		t.Fatalf("Add = %+v", info)
	}
	if m := e.mirrored(sess); len(m) != 1 || m[0].Prompt != "report the eval" {
		t.Fatalf("metadata mirror after Add = %+v", m)
	}
	e.clock.Advance(0)
	e.wantTicks(1)
	e.clock.Advance(59 * time.Second)
	e.wantTicks(1)
	e.clock.Advance(time.Second)
	e.wantTicks(2)
	e.clock.Advance(time.Minute)
	e.wantTicks(3)
	e.clock.Advance(10 * time.Minute)
	e.wantTicks(3)

	ticks := e.ticks()
	if got, want := ticks[2].Content, "[loop 1 · every 1m, 3 times · tick 3] report the eval"; got != want {
		t.Errorf("tick content = %q; want %q", got, want)
	}
	if ticks[2].Loop.ID != "1" || ticks[2].Loop.N != 3 || ticks[2].IsHarness() {
		t.Errorf("tick = %+v; want loop 1 tick 3, operator input", ticks[2])
	}
	evs := e.loopEvents(sess)
	if len(evs) != 2 || evs[0].Action != event.LoopSet || evs[0].By != event.ByOperator || evs[0].Summary != "every 1m, 3 times" ||
		evs[1].Action != event.LoopEnded || evs[1].Reason != "schedule finished after 3 ticks" {
		t.Fatalf("loop events = %+v", evs)
	}
	if m := e.mirrored(sess); len(m) != 0 {
		t.Errorf("an ended loop is still mirrored: %+v", m)
	}
	if got := e.s.List(run, sess); len(got) != 0 {
		t.Errorf("List after the end = %+v", got)
	}
}

func TestScheduler_FirstTickAfterOneInterval(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, Times: 2})
	e.clock.Advance(59 * time.Second)
	e.wantTicks(0)
	e.clock.Advance(time.Second)
	e.wantTicks(1)
}

func TestScheduler_NeverAndUntil(t *testing.T) {
	e := newEnv(t)
	e.add("forever", Form{Every: time.Minute, SendNow: true})
	for range 6 {
		e.clock.Advance(time.Minute)
	}
	e.wantTicks(7)
	if got := e.s.List(run, sess); len(got) != 1 || got[0].Ticks != 7 {
		t.Fatalf("a never-ending loop: List = %+v", got)
	}

	u := newEnv(t)
	u.add("until", Form{Every: time.Minute, SendNow: true, Ends: 150 * time.Second})
	u.clock.Advance(10 * time.Minute)
	u.wantTicks(3)
	if last := lastLoopEvent(t, u.loopEvents(sess)); last.Action != event.LoopEnded {
		t.Errorf("UNTIL passed but the loop didn't end: %+v", last)
	}
}

// No schedule ticks faster than MinInterval: dates added with RDATE that are
// closer than that are held back, then sent as one tick. A tick sent late (for
// an earlier occurrence) holds the next one 55 s, a minute less tickSlack.
func TestScheduler_MinIntervalHoldsBackCloseOccurrences(t *testing.T) {
	e := newEnv(t)
	e.addSpec(sess, "p", "DTSTART:20261110T120000Z\nRDATE:20261110T120000Z,20261110T120010Z,20261110T120020Z,20261110T120110Z")
	e.clock.Advance(0)
	e.wantTicks(1)
	e.clock.Advance(59 * time.Second)
	e.wantTicks(1) // 12:00:10 and :20 are due, but within a minute of the last tick
	e.clock.Advance(time.Second)
	e.wantTicks(2)
	e.clock.Advance(54 * time.Second)
	e.wantTicks(2) // 12:01:10 is due, held until 12:01:55
	e.clock.Advance(time.Second)
	e.wantTicks(3)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Reason != "schedule finished after 3 ticks" {
		t.Errorf("last loop event = %+v", last)
	}
}

// The fire-time check holds even when a timer goes off early (the wall clock
// moved, say), not just when the timer is armed.
func TestScheduler_MinIntervalEnforcedWhenTimerFires(t *testing.T) {
	e := newEnv(t)
	info := e.addSpec(sess, "p", "DTSTART:20261110T120000Z\nRDATE:20261110T120000Z,20261110T120010Z,20261110T120500Z")
	e.clock.Advance(0)
	e.wantTicks(1)
	e.s.mu.Lock()
	k := key{run, sess, info.ID}
	e.s.setTimerLocked(k, e.s.loops[k], t0.Add(20*time.Second))
	e.s.mu.Unlock()
	e.clock.Advance(30 * time.Second)
	e.wantTicks(1)
	e.clock.Advance(30 * time.Second)
	e.wantTicks(2)
}

// A timer that fires a little late doesn't push later ticks back: tick times
// are kept at whole seconds, as iCalendar's are.
func TestScheduler_LateTimersDontDrift(t *testing.T) {
	e := newEnv(t)
	e.clock.latency = 300 * time.Millisecond
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(10 * time.Minute)
	e.wantTicks(11)
	if m := e.mirrored(sess); !m[0].LastTick.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("last tick at %v; want %v", m[0].LastTick, t0.Add(10*time.Minute))
	}
}

// A restart re-arms from metadata. Occurrences that passed while the server was
// down are not replayed, and count toward COUNT.
func TestScheduler_RestartRearmsWithoutReplay(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true, Times: 5})
	e.clock.Advance(0)
	e.wantTicks(1)

	e.s.Shutdown()
	e.clock.Advance(210 * time.Second) // down through 12:01, 12:02 and 12:03
	e.wantTicks(1)
	e.restart()
	e.clock.Advance(0)
	e.wantTicks(1) // nothing replayed at startup
	if got := e.s.List(run, sess); len(got) != 1 || !got[0].NextTick.Equal(t0.Add(4*time.Minute)) || got[0].Ticks != 1 {
		t.Fatalf("after restart: %+v; want the next tick at 12:04", got)
	}
	e.clock.Advance(30 * time.Second)
	e.wantTicks(2)
	if tk := e.ticks()[1]; tk.Loop.N != 2 {
		t.Errorf("second tick numbered %d", tk.Loop.N)
	}
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Reason != "schedule finished after 2 ticks" {
		t.Errorf("COUNT=5 with 3 missed should end after 2 ticks; last loop event %+v", last)
	}
}

func TestScheduler_RestartKeepsMinInterval(t *testing.T) {
	e := newEnv(t)
	e.addSpec(sess, "p", "DTSTART:20261110T120000Z\nRDATE:20261110T120000Z,20261110T120030Z")
	e.clock.Advance(0)
	e.clock.Advance(10 * time.Second)
	e.restart()
	if got := e.s.List(run, sess); len(got) != 1 || !got[0].NextTick.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after restart: %+v; want the next tick a minute after the last", got)
	}
}

func TestScheduler_ArchivedRunEndsLoops(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	if err := e.store.SetRunArchived(e.ctx, run, true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.wantTicks(1)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEnded || last.Reason != "run archived" {
		t.Fatalf("last loop event = %+v", last)
	}
	if _, err := e.s.Add(e.ctx, run, sess, "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("Add on an archived run = %v; want refused", err)
	}
}

func TestScheduler_EndRun(t *testing.T) {
	e := newEnv(t)
	e.addSession("helper")
	e.add("p", Form{Every: time.Minute})
	e.addSpec("helper", "q", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
	e.s.EndRun(e.ctx, run, "run archived")
	for _, s := range []string{sess, "helper"} {
		if last := lastLoopEvent(t, e.loopEvents(s)); last.Reason != "run archived" {
			t.Errorf("%s: last loop event %+v", s, last)
		}
		if m := e.mirrored(s); len(m) != 0 {
			t.Errorf("%s: still mirrored %+v", s, m)
		}
	}
	e.clock.Advance(2 * time.Hour)
	e.wantTicks(0)
}

// A run archived while the server was down: its loops end at startup.
func TestScheduler_StartEndsArchivedRunsLoops(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute})
	e.s.Shutdown()
	if err := e.store.SetRunArchived(e.ctx, run, true); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Reason != "run archived" {
		t.Fatalf("last loop event %+v", last)
	}
}

func TestScheduler_DeletedRun(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.s.ForgetRun(run)
	e.clock.Advance(time.Minute)
	e.wantTicks(0)
	if got := e.s.List(run, ""); len(got) != 0 {
		t.Fatalf("ForgetRun left %+v", got)
	}

	// Deleted without ForgetRun: the timer finds no run and drops the loop.
	d := newEnv(t)
	d.add("p", Form{Every: time.Minute})
	if err := d.store.DeleteRun(d.ctx, run); err != nil {
		t.Fatal(err)
	}
	d.clock.Advance(time.Minute)
	if got := d.s.List(run, ""); len(got) != 0 {
		t.Fatalf("a deleted run's loop is still listed: %+v", got)
	}
}

func TestScheduler_EditAndStop(t *testing.T) {
	e := newEnv(t)
	info := e.add("old", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	newPrompt := "new"
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Prompt: &newPrompt}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if got := e.ticks()[1].Content; !strings.HasSuffix(got, "] new") {
		t.Errorf("tick after a prompt edit = %q", got)
	}

	// A new schedule applies from now, but nothing at or before the last tick
	// (sent just now, at 12:01) comes due again: its first occurrence is that
	// same 12:01, so the next is 12:03.
	newSched, err := Form{Every: 2 * time.Minute, SendNow: true}.Schedule(e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	edited, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Schedule: &newSched})
	if err != nil {
		t.Fatal(err)
	}
	if !edited.NextTick.Equal(t0.Add(3*time.Minute)) || edited.Ticks != 2 || edited.Summary != "every 2m" {
		t.Fatalf("Edit = %+v", edited)
	}
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEdit || last.Prompt != "new" || last.Schedule.Spec != newSched.Spec ||
		last.Summary != "every 2m" {
		t.Errorf("edit event = %+v", last)
	}
	e.clock.Advance(119 * time.Second)
	e.wantTicks(2)
	e.clock.Advance(time.Second)
	e.wantTicks(3)

	if err := e.s.Stop(e.ctx, run, sess, info.ID); err != nil {
		t.Fatal(err)
	}
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopStop || last.By != event.ByOperator {
		t.Errorf("stop event = %+v", last)
	}
	e.clock.Advance(10 * time.Minute)
	e.wantTicks(3)
	if err := e.s.Stop(e.ctx, run, sess, info.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stop of a stopped loop = %v", err)
	}
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Prompt: &newPrompt}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Edit of a stopped loop = %v", err)
	}
}

func TestScheduler_IDsAreNotReused(t *testing.T) {
	e := newEnv(t)
	e.addSession("helper")
	first := e.add("p", Form{Every: time.Hour})
	if err := e.s.Stop(e.ctx, run, sess, first.ID); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if second := e.add("p", Form{Every: time.Hour}); second.ID != "2" {
		t.Errorf("second loop id %q; want 2", second.ID)
	}
	if other := e.addSpec("helper", "p", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"); other.ID != "1" {
		t.Errorf("another session's first loop id %q; want 1", other.ID)
	}
}

func TestScheduler_AddRejects(t *testing.T) {
	e := newEnv(t)
	valid := sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
	for _, tc := range []struct {
		name, session, prompt string
		s                     event.Schedule
		want                  error
	}{
		{"empty prompt", sess, " ", valid, ErrInvalid},
		{"bad kind", sess, "p", event.Schedule{Kind: "cron", Spec: valid.Spec}, ErrInvalid},
		{"all in the past", sess, "p", sched("DTSTART:20261110T100000Z\nRRULE:FREQ=HOURLY;COUNT=1"), ErrInvalid},
		{"no session", "nobody", "p", valid, ErrNotFound},
	} {
		if _, err := e.s.Add(e.ctx, run, tc.session, tc.prompt, tc.s, nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: Add = %v; want %v", tc.name, err, tc.want)
		}
	}
	if evs := e.loopEvents(sess); len(evs) != 0 {
		t.Errorf("a refused Add wrote %+v", evs)
	}
	if _, err := New(e.store, Options{}).Add(e.ctx, run, sess, "p", valid, nil); err == nil {
		t.Error("Add before Start accepted")
	}
}

// A wall-clock rule is read in the scheduler's zone.
func TestScheduler_FloatingTimeInServerZone(t *testing.T) {
	zurich := mustLoad(t, "Europe/Zurich")
	e := newEnv(t)
	e.s.Shutdown()
	e.s = New(e.store, Options{Clock: e.clock, Location: zurich})
	if err := e.s.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	info := e.addSpec(sess, "p", "DTSTART:20261111T090000\nRRULE:FREQ=DAILY")
	if want := time.Date(2026, 11, 11, 9, 0, 0, 0, zurich); !info.NextTick.Equal(want) {
		t.Errorf("next tick %v; want %v", info.NextTick, want)
	}
}

// A tick is operator input (it revives a concluded session); the loop's own
// bookkeeping is a notice.
func TestTickIsInput(t *testing.T) {
	e := newEnv(t)
	var committed []event.Event
	e.store.SetCommitListener(func(_, _ string, evt event.Event) { committed = append(committed, evt) })
	e.add("p", Form{Every: time.Minute, SendNow: true, Times: 1})
	e.clock.Advance(0)
	var kinds []string
	for _, evt := range committed {
		class := "notice"
		if db.IsInput(evt) {
			class = "input"
		}
		kinds = append(kinds, evt.EventType()+":"+class)
	}
	if got, want := strings.Join(kinds, " "), "loop:notice user:input loop:notice"; got != want {
		t.Errorf("committed %q; want %q", got, want)
	}
}

// A cancelled session is a deliberate stop: when a tick comes due, the loop
// ends instead of reviving it, however the cancel happened.
func TestScheduler_CancelledSessionEndsAtTick(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.setStatus(db.SessionCancelled) // no listener: only the tick sees it
	e.clock.Advance(time.Minute)
	e.wantTicks(1)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEnded || last.Reason != EndedSessionCancelled {
		t.Fatalf("last loop event = %+v", last)
	}
	if m := e.mirrored(sess); len(m) != 0 {
		t.Errorf("still mirrored: %+v", m)
	}
}

// A crash isn't a deliberate stop: the next tick revives the session, as a
// typed message would.
func TestScheduler_CrashedSessionStillTicks(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.setStatus(db.SessionCrashed)
	e.s.OnSessionStatus(run, sess, db.SessionCrashed)
	e.clock.Advance(time.Minute)
	e.wantTicks(2)
}

// Every cancel goes through the store's status listener, so a cancelled
// session's loops end at once, and so do those of every session under it,
// finished ones included: the cancel cascade skips a concluded sub-agent (and
// what's under it), and a tick reviving one would revive the cancelled session
// through its result. A sibling's loop stays.
func TestScheduler_CancelEndsSubtreeLoopsAtOnce(t *testing.T) {
	e := newEnv(t)
	e.store.SetSessionStatusChangedListener(e.s.OnSessionStatus)
	e.addSessionAs("worker", db.SessionAwaiting, "")
	e.addSessionAs("helper", db.SessionConcluded, "worker")
	e.addSessionAs("grandchild", db.SessionIdle, "helper")
	e.addSessionAs("sibling", db.SessionIdle, "")
	spec := "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"
	for _, s := range []string{"worker", "helper", "grandchild", "sibling"} {
		e.addSpec(s, "p", spec)
	}
	if err := session.CancelSession(e.store, nil, run, "worker", "cancelled via UI"); err != nil {
		t.Fatal(err)
	}
	for s, want := range map[string]string{
		"worker": EndedSessionCancelled, "helper": "session worker cancelled", "grandchild": "session worker cancelled",
	} {
		waitFor(t, s+"'s loop to end", func() bool {
			evs := e.loopEvents(s)
			return len(evs) == 2 && evs[1].Reason == want
		})
	}
	if got := e.s.List(run, ""); len(got) != 1 || got[0].SessionID != "sibling" {
		t.Errorf("loops left: %+v; want only the sibling's", got)
	}
}

// The same holds when no listener saw the cancel: a tick on a finished
// sub-agent of a cancelled session ends the loop instead, and a loop can't be
// set there.
func TestScheduler_CancelledAncestorEndsLoopAtTick(t *testing.T) {
	e := newEnv(t)
	e.addSessionAs("worker", db.SessionAwaiting, "")
	e.addSessionAs("helper", db.SessionConcluded, "worker")
	e.addSpec("helper", "p", "DTSTART:20261110T120100Z\nRRULE:FREQ=MINUTELY;INTERVAL=1")
	if err := e.store.UpdateSessionStatus(e.ctx, run, "worker", db.SessionCancelled); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if got := e.tickCount("helper"); got != 0 {
		t.Errorf("%d ticks reached the cancelled worker's sub-agent", got)
	}
	if last := lastLoopEvent(t, e.loopEvents("helper")); last.Reason != "session worker cancelled" {
		t.Errorf("last loop event %+v", last)
	}
	if _, err := e.s.Add(e.ctx, run, "helper", "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("Add under a cancelled ancestor = %v", err)
	}
}

// A session cancelled while no scheduler ran: its loops end at startup.
func TestScheduler_StartEndsCancelledSessionsLoops(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute})
	e.s.Shutdown()
	e.setStatus(db.SessionCancelled)
	e.restart()
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Reason != EndedSessionCancelled {
		t.Fatalf("last loop event %+v", last)
	}
	if got := e.s.List(run, ""); len(got) != 0 {
		t.Errorf("loops left: %+v", got)
	}
}

// Its ticks couldn't revive it, so a loop can't be set on a cancelled session.
func TestScheduler_AddRefusesCancelledSession(t *testing.T) {
	e := newEnv(t)
	e.setStatus(db.SessionCancelled)
	if _, err := e.s.Add(e.ctx, run, sess, "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "cancelled") {
		t.Errorf("Add on a cancelled session = %v", err)
	}
}

// faultStore fails or blocks the scheduler's store calls on demand.
type faultStore struct {
	db.Store
	mu                   sync.Mutex
	failRun, failSession error
	failTick             error
	blockSession         string        // a tick appended to it waits for block
	block, entered       chan struct{} // entered is signalled as it starts waiting
}

func (f *faultStore) set(fn func(*faultStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faultStore) GetRun(ctx context.Context, runID string) (*db.RunRecord, error) {
	f.mu.Lock()
	err := f.failRun
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Store.GetRun(ctx, runID)
}

func (f *faultStore) GetSession(ctx context.Context, runID, sessionID string) (*db.SessionRecord, error) {
	f.mu.Lock()
	err := f.failSession
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Store.GetSession(ctx, runID, sessionID)
}

// AppendEventsIf is how a tick is written.
func (f *faultStore) AppendEventsIf(ctx context.Context, runID, sessionID string, ok func([]db.SessionRecord) error,
	md map[string]any, evts ...event.Event) error {
	for _, evt := range evts {
		if err := f.tickFault(sessionID, evt); err != nil {
			return err
		}
	}
	return f.Store.AppendEventsIf(ctx, runID, sessionID, ok, md, evts...)
}

// tickFault fails, or holds up, the write of a tick as the test asks.
func (f *faultStore) tickFault(sessionID string, evt event.Event) error {
	if u, ok := evt.(*event.UserEvent); !ok || u.Loop == nil {
		return nil
	}
	f.mu.Lock()
	err, block := f.failTick, f.block != nil && sessionID == f.blockSession
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if block {
		f.entered <- struct{}{}
		<-f.block
	}
	return nil
}

// restartWith replaces the scheduler with one on store.
func (e *env) restartWith(store Store) {
	e.t.Helper()
	e.s.Shutdown()
	e.s = New(store, Options{Clock: e.clock, Location: time.UTC, Verify: e.startVersion})
	if err := e.s.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// Re-saving a loop's schedule (say with a prompt edit) right after a tick
// doesn't send that tick again.
func TestScheduler_EditRightAfterTickNoDuplicate(t *testing.T) {
	e := newEnv(t)
	info := e.add("p", Form{Every: 30 * time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.clock.Advance(20 * time.Second)
	p, s := "p2", info.Schedule
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Prompt: &p, Schedule: &s}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5 * time.Minute)
	e.wantTicks(1)
	e.clock.Advance(25 * time.Minute)
	e.wantTicks(2)
}

// One tick sent late (here 1.5 s) leaves the later ones on the schedule: the
// minute between ticks is measured from the occurrence a tick was sent for.
func TestScheduler_OneLateTickDoesNotShiftLaterTicks(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.latency = 1500 * time.Millisecond
	e.clock.Advance(0)
	e.clock.latency = 0
	e.clock.Advance(5 * time.Minute)
	e.wantTicks(6)
	if m := e.mirrored(sess); !m[0].LastTick.Equal(t0.Add(5*time.Minute)) || !m[0].LastDue.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("last tick %v for %v; want both %v", m[0].LastTick, m[0].LastDue, t0.Add(5*time.Minute))
	}
}

// A timer is capped at maxWait; when it goes off with nothing due, the loop
// re-arms instead of ticking.
func TestScheduler_CappedWaitRearms(t *testing.T) {
	e := newEnv(t)
	e.add("p", Form{Every: 3 * time.Hour})
	e.clock.Advance(2*time.Hour + 59*time.Minute)
	e.wantTicks(0)
	e.clock.Advance(time.Minute)
	e.wantTicks(1)
}

// A failed read or write when a tick is due delays it a minute, and leaves the
// loop as it was.
func TestScheduler_RetriesAfterStoreErrors(t *testing.T) {
	boom := errors.New("disk on fire")
	for _, tc := range []struct {
		name string
		fail func(*faultStore, error)
	}{
		{"read run", func(f *faultStore, err error) { f.failRun = err }},
		{"read session", func(f *faultStore, err error) { f.failSession = err }},
		{"write tick", func(f *faultStore, err error) { f.failTick = err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			fs := &faultStore{Store: e.store}
			e.restartWith(fs)
			e.add("p", Form{Every: time.Hour, SendNow: true})
			e.clock.Advance(0)
			e.wantTicks(1)
			fs.set(func(f *faultStore) { tc.fail(f, boom) })
			e.clock.Advance(time.Hour)
			e.wantTicks(1)
			got := e.s.List(run, sess)
			if len(got) != 1 || got[0].Ticks != 1 || !got[0].NextTick.Equal(t0.Add(time.Hour+time.Minute)) {
				t.Fatalf("after a failed tick: %+v; want 1 tick, a retry at 13:01", got)
			}
			fs.set(func(f *faultStore) { tc.fail(f, nil) })
			e.clock.Advance(time.Minute)
			e.wantTicks(2)
			if m := e.mirrored(sess); m[0].Ticks != 2 || !m[0].LastDue.Equal(t0.Add(time.Hour)) {
				t.Errorf("after the retry: %+v", m[0])
			}
		})
	}
}

// A session that has vanished drops its loop quietly; a schedule that can't
// be evaluated any more ends it.
func TestScheduler_GoneSessionAndBadWalk(t *testing.T) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store}
	e.restartWith(fs)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	fs.set(func(f *faultStore) { f.failSession = fmt.Errorf("scan session: %w", sql.ErrNoRows) })
	e.clock.Advance(time.Minute)
	if got := e.s.List(run, ""); len(got) != 0 {
		t.Errorf("a gone session's loop is still listed: %+v", got)
	}
	if evs := e.loopEvents(sess); len(evs) != 1 {
		t.Errorf("loop events %+v; want only the set", evs)
	}

	w := newEnv(t)
	w.add("p", Form{Every: time.Minute, SendNow: true})
	w.clock.Advance(0)
	defer func(n int) { maxOccurrences = n }(maxOccurrences)
	maxOccurrences = 2
	w.clock.Advance(time.Minute)
	if last := lastLoopEvent(t, w.loopEvents(sess)); last.Action != event.LoopEnded || !strings.Contains(last.Reason, "too many occurrences") {
		t.Errorf("last loop event %+v", last)
	}
}

// Loops list and mirror in numeric id order: 2 before 10.
func TestScheduler_LoopOrder(t *testing.T) {
	e := newEnv(t)
	for range 11 {
		e.add("p", Form{Every: time.Hour})
	}
	var listed, mirrored []string
	for _, i := range e.s.List(run, sess) {
		listed = append(listed, i.ID)
	}
	for _, l := range e.mirrored(sess) {
		mirrored = append(mirrored, l.ID)
	}
	want := "1 2 3 4 5 6 7 8 9 10 11"
	if strings.Join(listed, " ") != want || strings.Join(mirrored, " ") != want {
		t.Errorf("listed %v, mirrored %v; want %s", listed, mirrored, want)
	}
}

// A slow tick delivery (the write runs the wake path) holds up only its own
// session: listing and another session's changes go ahead meanwhile.
func TestScheduler_SlowDeliveryBlocksOnlyItsSession(t *testing.T) {
	e := newEnv(t)
	e.addSession("other")
	fs := &faultStore{Store: e.store, blockSession: sess, block: make(chan struct{}), entered: make(chan struct{}, 1)}
	e.restartWith(fs)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	other := e.addSpec("other", "q", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
	done := make(chan struct{})
	go func() { e.clock.Advance(0); close(done) }() // main-agent's tick blocks in its write
	recv(t, fs.entered, "the tick's write to start")
	if got := e.s.List(run, ""); len(got) != 2 {
		t.Errorf("List during a slow delivery = %+v", got)
	}
	if err := e.s.Stop(e.ctx, run, "other", other.ID); err != nil {
		t.Errorf("Stop on another session during a slow delivery: %v", err)
	}
	close(fs.block)
	recv(t, done, "the tick to finish")
	e.wantTicks(1)
}

// Only a missing session is "not found"; a failed read is an error.
func TestScheduler_AddReadErrorIsNotNotFound(t *testing.T) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store}
	e.restartWith(fs)
	fs.set(func(f *faultStore) { f.failSession = errors.New("disk on fire") })
	_, err := e.s.Add(e.ctx, run, sess, "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil)
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
		t.Errorf("Add with a failing read = %v; want a plain error", err)
	}
}

// A verifier is started and re-engaged by the harness; a loop's ticks would
// revive it, so it can't have one.
func TestScheduler_AddRefusesVerifier(t *testing.T) {
	e := newEnv(t)
	if err := e.store.CreateSession(e.ctx, db.SessionRecord{RunID: run, SessionID: "checker", Status: db.SessionConcluded,
		ParentID: sess, Metadata: map[string]any{verify.MetadataVerifierFor: sess}}); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Add(e.ctx, run, "checker", "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "verifier") {
		t.Errorf("Add on a verifier = %v", err)
	}
}

// ForgetRun returns only once a tick already being delivered for the run has
// finished, so nothing writes into a run being deleted after it returns.
func TestScheduler_ForgetRunWaitsForInFlightTick(t *testing.T) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store, blockSession: sess, block: make(chan struct{}), entered: make(chan struct{}, 1)}
	e.restartWith(fs)
	e.add("p", Form{Every: time.Minute, SendNow: true})
	ticked := make(chan struct{})
	go func() { e.clock.Advance(0); close(ticked) }()
	recv(t, fs.entered, "the tick's write to start")
	forgot := make(chan struct{})
	go func() { e.s.ForgetRun(run); close(forgot) }()
	select {
	case <-forgot:
		t.Fatal("ForgetRun returned while a tick was being delivered")
	case <-time.After(100 * time.Millisecond):
	}
	close(fs.block)
	recv(t, ticked, "the tick to finish")
	recv(t, forgot, "ForgetRun to return")
	if got := e.s.List(run, ""); len(got) != 0 {
		t.Errorf("loops left: %+v", got)
	}
	e.wantTicks(0) // the tick's write found its loop gone
}

// A loop id sequence reads back whether it went through JSON (float64) or was
// stored as an int.
func TestMetaInt(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
	}{{float64(7), 7}, {7, 7}, {int64(7), 7}, {"7", 0}, {nil, 0}} {
		if got := metaInt(tc.in); got != tc.want {
			t.Errorf("metaInt(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// respawns records the sessions the real commit notifier would respawn, with
// their status and their parent's at that moment.
type respawns struct {
	mu  sync.Mutex
	got []string
}

func (r *respawns) install(e *env) {
	e.store.SetCommitListener(runtime.NewCommitNotifier(runtime.NewRunRegistry(), func(runID, sessionID string) {
		sess, _ := e.store.GetSession(context.Background(), runID, sessionID)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, fmt.Sprintf("%s (%s, parent %q)", sessionID, sess.Status, sess.ParentID))
	}, nil, nil))
}

func (r *respawns) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// A cancel that lands while a tick is on its way (past fire's checks, waiting
// to be written) stops it: the write checks the ancestors again, in its
// transaction. Otherwise the tick would revive the concluded helper, which the
// cascade skipped, and its result the cancelled worker. With the status
// listener, the cancel's own end of the loops races the write; without it (a
// cancel this process hasn't acted on yet), only the write's check stops it.
func TestScheduler_CancelDuringTickDoesNotLand(t *testing.T) {
	for _, listen := range []bool{true, false} {
		t.Run(fmt.Sprintf("listener=%v", listen), func(t *testing.T) {
			e := newEnv(t)
			e.addSessionAs("worker", db.SessionAwaiting, "")
			e.addSessionAs("helper", db.SessionConcluded, "worker")
			fs := &faultStore{Store: e.store, blockSession: "helper", block: make(chan struct{}), entered: make(chan struct{}, 1)}
			e.restartWith(fs)
			if listen {
				e.store.SetSessionStatusChangedListener(e.s.OnSessionStatus)
			}
			var r respawns
			r.install(e)

			e.addSpec("helper", "check the build", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
			done := make(chan struct{})
			go func() { defer close(done); e.clock.Advance(time.Hour) }()
			recv(t, fs.entered, "the tick's write to start")
			if err := session.CancelSession(e.store, nil, run, "worker", "cancelled"); err != nil {
				t.Fatal(err)
			}
			close(fs.block)
			recv(t, done, "the tick to finish")
			waitFor(t, "helper's loop to end", func() bool { return len(e.s.List(run, "helper")) == 0 })

			if got := r.list(); len(got) != 0 {
				t.Errorf("respawned after the cancel: %v", got)
			}
			if n := e.tickCount("helper"); n != 0 {
				t.Errorf("%d ticks landed under the cancelled worker", n)
			}
			waitFor(t, "the end to be recorded", func() bool { return len(e.loopEvents("helper")) == 2 })
			if last := lastLoopEvent(t, e.loopEvents("helper")); last.Action != event.LoopEnded || last.Reason != "session worker cancelled" {
				t.Errorf("last loop event %+v", last)
			}
		})
	}
}

// A run cancel ends the run's loops before it cancels the sessions, and a tick
// on its way then doesn't land either: else it would revive a concluded root
// that the cancel, finding it concluded, leaves alone.
func TestScheduler_RunCancelDuringTickDoesNotLand(t *testing.T) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store, blockSession: sess, block: make(chan struct{}), entered: make(chan struct{}, 1)}
	e.restartWith(fs)
	var r respawns
	r.install(e)
	info := e.addSpec(sess, "p", "DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")

	ticked := make(chan struct{})
	go func() { defer close(ticked); e.clock.Advance(time.Hour) }()
	recv(t, fs.entered, "the tick's write to start")
	ended := make(chan struct{})
	go func() { // as handleCancelRun does
		defer close(ended)
		e.s.EndRun(e.ctx, run, EndedRunCancelled)
		if err := session.CancelSession(e.store, nil, run, sess, "cancelled via UI"); err != nil {
			t.Error(err)
		}
	}()
	k := key{run, sess, info.ID}
	waitFor(t, "EndRun to mark the loop", func() bool {
		e.s.mu.Lock()
		defer e.s.mu.Unlock()
		return e.s.loops[k] != nil && e.s.loops[k].ending
	})
	close(fs.block)
	recv(t, ticked, "the tick to finish")
	recv(t, ended, "the run cancel to finish")
	if got := r.list(); len(got) != 0 {
		t.Errorf("respawned during the run cancel: %v", got)
	}
	e.wantTicks(0)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Reason != EndedRunCancelled {
		t.Errorf("last loop event %+v", last)
	}
}

// DTSTART and RDATE are bounded from above too: a time near the end of year
// 9999 reads as year 10000 east of UTC, which JSON can't carry.
func TestScheduler_AddRefusesFarFutureDates(t *testing.T) {
	e := newEnv(t)
	for _, spec := range []string{
		"DTSTART:99991231T230000Z\nRRULE:FREQ=DAILY",
		"DTSTART:21271231T000000Z\nRRULE:FREQ=DAILY",
		"DTSTART:20261110T130000Z\nRDATE:20261110T140000Z,99991231T230000Z",
	} {
		if _, err := e.s.Add(e.ctx, run, sess, "p", sched(spec), nil); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "100 years") {
			t.Errorf("Add(%q) = %v", spec, err)
		}
	}
	e.addSpec(sess, "p", "DTSTART:21200101T000000Z\nRRULE:FREQ=YEARLY")
}

// A loop that never ends keeps ticking past the walk cap: as its DTSTART ages,
// a tick moves it up to the occurrence the tick is for, so walks stay short.
// 50 daily ticks with a cap of 40: past the cap, with a rebase at 30 days
// (rebaseAge) on the way; without it, the walks would hit the cap at day 40.
func TestScheduler_NeverEndingLoopOutlivesWalkCap(t *testing.T) {
	defer func(n int) { maxOccurrences = n }(maxOccurrences)
	maxOccurrences = 40
	const days = 50
	e := newEnv(t)
	info := e.add("daily", Form{Every: 24 * time.Hour, SendNow: true})
	e.clock.Advance(0)
	for range days {
		e.clock.Advance(24 * time.Hour)
	}
	e.wantTicks(days + 1)
	got := e.s.List(run, sess)
	if len(got) != 1 || got[0].Summary != info.Summary {
		t.Fatalf("after %d days: %+v", days, got)
	}
	rule := mustParse(t, got[0].Schedule.Spec, time.UTC)
	if age := e.clock.Now().Sub(rule.Start()); age > rebaseAge+24*time.Hour {
		t.Errorf("DTSTART %v is %s old", rule.Start(), age)
	}
	if m := e.mirrored(sess); m[0].Schedule.Spec != got[0].Schedule.Spec {
		t.Errorf("mirrored spec %q; listed %q", m[0].Schedule.Spec, got[0].Schedule.Spec)
	}
	for i, tk := range e.ticks() {
		if want := fmt.Sprintf("[loop 1 · every 1d · tick %d] daily", i+1); tk.Content != want {
			t.Fatalf("tick %d = %q", i+1, tk.Content)
		}
	}
}

// A session has at most maxLoops loops.
func TestScheduler_LoopsPerSessionCapped(t *testing.T) {
	e := newEnv(t)
	e.addSession("other")
	var first Info
	for i := range maxLoops {
		if info := e.add("p", Form{Every: time.Hour}); i == 0 {
			first = info
		}
	}
	valid := sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY")
	if _, err := e.s.Add(e.ctx, run, sess, "p", valid, nil); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "20 loops") {
		t.Errorf("Add past the cap = %v", err)
	}
	e.addSpec("other", "p", valid.Spec)
	if err := e.s.Stop(e.ctx, run, sess, first.ID); err != nil {
		t.Fatal(err)
	}
	e.addSpec(sess, "p", valid.Spec)
}

// Nor on a session under a verifier: a tick reviving the verifier's sub-agent
// would revive the verifier through its result.
func TestScheduler_AddRefusesUnderVerifier(t *testing.T) {
	e := newEnv(t)
	if err := e.store.CreateSession(e.ctx, db.SessionRecord{RunID: run, SessionID: "checker", Status: db.SessionConcluded,
		ParentID: sess, Metadata: map[string]any{verify.MetadataVerifierFor: sess}}); err != nil {
		t.Fatal(err)
	}
	e.addSessionAs("checker-helper", db.SessionConcluded, "checker")
	e.addSessionAs("helper-helper", db.SessionConcluded, "checker-helper")
	for _, s := range []string{"checker-helper", "helper-helper"} {
		_, err := e.s.Add(e.ctx, run, s, "p", sched("DTSTART:20261110T130000Z\nRRULE:FREQ=HOURLY"), nil)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "under checker, main-agent's verifier") {
			t.Errorf("Add on %s = %v", s, err)
		}
	}
}

// A tick that revives a crashed session counts, though the turn it revived it
// for fails again before reaching its next step (a model that is still down):
// the loop goes on, as it does for any crash.
func TestScheduler_RevivedThenCrashedAgainKeepsLoop(t *testing.T) {
	e := newEnv(t)
	e.store.SetSessionStatusChangedListener(e.s.OnSessionStatus)
	// It crashed in a model call: at step 2, with no turn at step 1.
	for range 2 {
		if _, err := e.store.AdvanceStep(e.ctx, run, sess); err != nil {
			t.Fatal(err)
		}
	}
	e.setStatus(db.SessionCrashed)
	// Each tick revives it, and the turn crashes again at the same step.
	e.store.SetCommitListener(func(_, _ string, evt event.Event) {
		if u, ok := evt.(*event.UserEvent); ok && u.Loop != nil {
			_ = e.store.UpdateSessionStatus(context.Background(), run, sess, db.SessionOngoing)
			_ = e.store.UpdateSessionStatus(context.Background(), run, sess, db.SessionCrashed)
		}
	})
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.clock.Advance(time.Minute)
	e.clock.Advance(time.Minute)
	e.wantTicks(3)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopSet {
		t.Errorf("last loop event %+v; want the loop still set", last)
	}
}

// A tick that leaves its session at rest and unread (reviving it failed: its
// agent type is gone, say) ends the loop at the next tick, instead of one
// unread tick per interval. A busy session reads its ticks later, so they
// queue; and after a restart the first tick goes out again, since the last
// one's revival may have died with the old process.
func TestScheduler_UnrevivedSessionEndsLoop(t *testing.T) {
	e := newEnv(t)
	e.store.SetCommitListener(nil) // nothing revives main-agent
	e.store.SetSessionStatusChangedListener(e.s.OnSessionStatus)
	// A session that has run: it is at rest at step 2.
	for range 2 {
		if _, err := e.store.AdvanceStep(e.ctx, run, sess); err != nil {
			t.Fatal(err)
		}
	}
	e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.clock.Advance(time.Minute)
	e.wantTicks(1)
	if last := lastLoopEvent(t, e.loopEvents(sess)); last.Action != event.LoopEnded ||
		last.Reason != "tick 1 didn't wake the session, still concluded (the server log says why)" {
		t.Errorf("last loop event %+v", last)
	}

	busy := newEnv(t)
	busy.store.SetCommitListener(nil)
	busy.setStatus(db.SessionOngoing)
	busy.add("p", Form{Every: time.Minute, SendNow: true})
	busy.clock.Advance(0)
	busy.clock.Advance(2 * time.Minute)
	busy.wantTicks(3)

	r := newEnv(t)
	r.store.SetCommitListener(nil)
	r.add("p", Form{Every: time.Minute, SendNow: true})
	r.clock.Advance(0)
	r.restart()
	r.clock.Advance(time.Minute)
	r.wantTicks(2)
	r.clock.Advance(time.Minute)
	r.wantTicks(2)

	// Revived but not yet at its first step: it will read the tick.
	slow := newEnv(t)
	slow.store.SetCommitListener(func(_, _ string, evt event.Event) {
		if u, ok := evt.(*event.UserEvent); ok && u.Loop != nil {
			_ = slow.store.UpdateSessionStatus(context.Background(), run, sess, db.SessionOngoing)
		}
	})
	slow.add("p", Form{Every: time.Minute, SendNow: true})
	slow.clock.Advance(0)
	slow.clock.Advance(time.Minute)
	slow.wantTicks(2)
}

// The mirror keeps the fields of a loop that this binary doesn't know, so a
// newer binary's (say, a verification per tick) survive this one's rewrites
// and are there when the newer binary runs again.
func TestScheduler_MirrorKeepsFieldsItDoesntKnow(t *testing.T) {
	e := newEnv(t)
	info := e.add("p", Form{Every: time.Minute, SendNow: true})
	e.clock.Advance(0)
	e.s.Shutdown()
	sr, err := e.store.GetSession(e.ctx, run, sess)
	if err != nil {
		t.Fatal(err)
	}
	stored := sr.Metadata[MetadataKey].([]any)
	stored[0].(map[string]any)["future"] = map[string]any{"criteria": "tests pass"}
	if err := e.store.MergeSessionMetadata(e.ctx, run, sess, map[string]any{MetadataKey: stored}); err != nil {
		t.Fatal(err)
	}
	e.restart()
	e.clock.Advance(time.Minute)
	p := "q"
	if _, err := e.s.Edit(e.ctx, run, sess, info.ID, Change{Prompt: &p}); err != nil {
		t.Fatal(err)
	}
	sr, err = e.store.GetSession(e.ctx, run, sess)
	if err != nil {
		t.Fatal(err)
	}
	entry := sr.Metadata[MetadataKey].([]any)[0].(map[string]any)
	if fmt.Sprint(entry["future"]) != "map[criteria:tests pass]" || entry["ticks"] != float64(2) || entry["prompt"] != "q" {
		t.Errorf("mirrored entry after a tick and an edit: %v", entry)
	}
}
