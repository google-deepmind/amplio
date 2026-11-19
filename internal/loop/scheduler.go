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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/verify"
)

// Store is the part of db.Store the scheduler uses.
type Store interface {
	GetRun(ctx context.Context, runID string) (*db.RunRecord, error)
	GetSession(ctx context.Context, runID, sessionID string) (*db.SessionRecord, error)
	GetChildSessions(ctx context.Context, runID, parentID string) ([]db.SessionRecord, error)
	AppendEventWithMetadata(ctx context.Context, runID, sessionID string, evt event.Event, updates map[string]any) (string, error)
	AppendEventsIf(ctx context.Context, runID, sessionID string, ok func(lineage []db.SessionRecord) error,
		updates map[string]any, evts ...event.Event) error
	ListSessionsWithMetadata(ctx context.Context, key string) ([]db.SessionRecord, error)
}

// Clock is the scheduler's time source; tests substitute a fake.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc call.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Session metadata keys: the session's active loops (mirrored from its
// stream, so startup re-arms them without reading every stream), and the last
// loop id handed out in it.
const (
	MetadataKey = "loops"
	seqKey      = "loop_seq"
)

const (
	// addGrace lets an occurrence up to this long before a loop is added or
	// rescheduled still count as due, so a rule starting "now" by the client's
	// clock fires at once.
	addGrace = time.Minute
	// tickSlack is how much less than MinInterval may separate two ticks by
	// the wall clock, when the first was late. Ticks are held a minute apart
	// by the occurrences they were sent for, so a late tick doesn't push the
	// later ones off the schedule; this only stops them bunching up.
	tickSlack = 5 * time.Second
	// maxWait caps a timer, so a change to the wall clock is noticed within it.
	maxWait = time.Hour
	// retryAfter is how long a tick that couldn't be written waits to retry;
	// each further failure in a row doubles it, up to maxWait.
	retryAfter = time.Minute
	// giveUpAfter is how long a loop with no occurrence left keeps retrying the
	// tick it couldn't send, before it ends without it.
	giveUpAfter = time.Hour
	// maxDepth bounds a walk up a session's ancestors.
	maxDepth = 64
	// maxStartAge is how far in the past a new schedule's DTSTART may be. rrule-go
	// walks from DTSTART, so this bounds the walk to now (see checkRRule).
	maxStartAge = 366 * 24 * time.Hour
	// rebaseAge is how old a ticking loop's DTSTART may get before the tick
	// moves it up to the occurrence it is sent for (Rule.Rebase), so the walks
	// of a loop that never ends stay short.
	rebaseAge = 30 * 24 * time.Hour
	// maxAhead is how far in the future a new schedule's DTSTART and RDATEs may
	// be: a date near the end of year 9999 reads as year 10000 in a zone east
	// of UTC, which a loop's JSON can't carry.
	maxAhead = 100 * 366 * 24 * time.Hour
	// maxLoops is how many loops a session may have. Each session's loops are
	// mirrored in its metadata, which every change to one of them rewrites.
	maxLoops = 20
)

// ErrNotFound is returned for a session or loop that doesn't exist (or a loop
// that has ended).
var ErrNotFound = errors.New("not found")

// Why a loop ended, other than its schedule running out (LoopEvent.Reason). A
// loop under a cancelled ancestor ends with "session <ancestor> cancelled".
const (
	EndedRunArchived      = "run archived"
	EndedRunCancelled     = "run cancelled"
	EndedSessionCancelled = "session cancelled"
)

// Loop is one active loop, as mirrored in its session's metadata.
type Loop struct {
	ID       string         `json:"id"`
	Prompt   string         `json:"prompt"`
	Schedule event.Schedule `json:"schedule"`
	// Verify, if set, is the verification each tick starts (see VerifyFunc).
	Verify *event.LoopVerify `json:"verify,omitempty"`
	// Verified: the last tick started a version, which may still be in force;
	// the next tick without Verify turns it off.
	Verified  bool      `json:"verified,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Ticks     int       `json:"ticks"`
	LastTick  time.Time `json:"last_tick,omitzero"` // when the last tick was sent, in whole seconds
	LastDue   time.Time `json:"last_due,omitzero"`  // the occurrence it was sent for
	// Spent: every occurrence at or before it was sent or missed.
	Spent time.Time `json:"spent"`
	// unknown holds the mirrored fields this binary doesn't know (a newer one
	// wrote them), so that rewriting the mirror keeps them.
	unknown map[string]json.RawMessage
}

// mirrored is a Loop as session metadata stores it, unknown fields included.
type mirrored Loop

func (m mirrored) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(Loop(m))
	if err != nil || len(m.unknown) == 0 {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	maps.Copy(fields, m.unknown)
	return json.Marshal(fields)
}

func (m *mirrored) UnmarshalJSON(data []byte) error {
	var l Loop
	if err := json.Unmarshal(data, &l); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	maps.DeleteFunc(fields, func(name string, _ json.RawMessage) bool { return loopFields[name] })
	if len(fields) > 0 {
		l.unknown = fields
	}
	*m = mirrored(l)
	return nil
}

// loopFields are the JSON names of Loop's fields.
var loopFields = func() map[string]bool {
	names := map[string]bool{}
	t := reflect.TypeFor[Loop]()
	for i := range t.NumField() {
		if f := t.Field(i); f.IsExported() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			names[name] = true
		}
	}
	return names
}()

// Info is a loop as listed.
type Info struct {
	Loop
	RunID     string
	SessionID string
	Summary   string    // Rule.Describe
	NextTick  time.Time // when its timer fires
}

// Options configures a Scheduler; the zero value is the real clock, the
// server's zone, and no loops that verify their ticks.
type Options struct {
	Clock    Clock
	Location *time.Location // where floating times are read; default time.Local
	Verify   VerifyFunc
}

// VerifyFunc changes a session's verification for the next tick of a loop and
// has write record the change with the tick, in one write: verify.Tick, bound
// to the store and the run's registry. With v, the tick starts a new version
// with it (each tick is a new task), whose number it returns; with nil, a
// version the loop started that is still in force is turned off.
type VerifyFunc func(ctx context.Context, runID, sessionID, loopID string, v *event.LoopVerify,
	write func(change *event.VerifyEvent, version int) error) (int, error)

// Scheduler arms each active loop's next tick and appends ticks as operator
// input (UserEvent with Loop set), which wakes or revives the session through
// the ordinary commit path. Only `serve` runs one; headless runs don't arm
// loops.
//
// A tick fires at the next occurrence of the loop's schedule, but never within
// MinInterval of the occurrence the previous tick was sent for. Occurrences
// that pass without a tick (the server was down, or they came too close) are
// not replayed: they are spent, and count toward the rule's COUNT. A loop ends
// when its schedule has no occurrence left, when its run is archived or
// cancelled, and when its session or an ancestor of it is cancelled: a
// deliberate stop, which a tick must not undo, even by reviving a finished
// sub-agent whose result would revive the cancelled parent: a tick's write
// checks the session and its ancestors again, in its transaction (mayTick). A
// crash doesn't end a loop, even one that recurs in the turn a tick revived it
// for; a tick that left its session at rest, never running (it couldn't be
// revived), does. ForgetRun drops a deleted run's loops.
//
// Locking: each session's loops change under that session's write lock, which
// is held across the change's DB write (and the wake path the write runs), so
// the metadata mirror is written in the order the changes happen. s.mu guards
// the in-memory state and is never held across I/O, so a slow write delays
// only its own session.
type Scheduler struct {
	store  Store
	clock  Clock
	loc    *time.Location
	verify VerifyFunc

	mu       sync.Mutex
	started  bool
	closed   bool
	loops    map[key]*armed
	locks    map[sessionKey]*sync.Mutex // per-session write locks
	inflight sync.WaitGroup             // work in progress, which Shutdown waits for
}

type sessionKey struct{ run, session string }

type key struct{ run, session, id string }

func (k key) sess() sessionKey { return sessionKey{k.run, k.session} }

// armed is a loop in memory. Its Loop, rule and unread change only under its
// session's write lock and s.mu together, so either one is enough to read
// them; the timer fields, ending and ran are s.mu's.
type armed struct {
	Loop
	rule     *Rule
	parseErr error     // why rule is nil: a stored schedule Start couldn't parse
	at       time.Time // the tick the timer is for
	timer    Timer
	gen      uint64      // identifies the current timer, so a stale one does nothing
	failures int         // ticks in a row that couldn't be sent (fire's, under the session's lock)
	ending   bool        // endWhere is ending it: no tick may start or land
	unread   *unreadTick // the last tick, if this process sent it to the session at rest
	// ran: the session has been running since the last tick was sent
	// (OnSessionStatus), so that tick revived it, whatever came of it.
	ran bool
}

// unreadTick is a tick sent to a session at rest: tick n, at step.
type unreadTick struct{ n, step int }

// atRest are the statuses a session leaves when a tick wakes it: revived, or
// woken from idle, it is ongoing for the turn that reads it.
var atRest = map[string]bool{db.SessionConcluded: true, db.SessionCrashed: true, db.SessionIdle: true}

// New returns a scheduler; Start re-arms the loops already stored.
func New(store Store, opts Options) *Scheduler {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	return &Scheduler{store: store, clock: opts.Clock, loc: opts.Location, verify: opts.Verify,
		loops: map[key]*armed{}, locks: map[sessionKey]*sync.Mutex{}}
}

// Location is the zone floating times are read in.
func (s *Scheduler) Location() *time.Location { return s.loc }

// enter registers work Shutdown must wait for; the caller calls
// s.inflight.Done when it returns nil.
func (s *Scheduler) enter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.started:
		return errors.New("loop scheduler not started")
	case s.closed:
		return errors.New("loop scheduler shut down")
	}
	s.inflight.Add(1)
	return nil
}

// lockSession takes a session's write lock (without holding s.mu while it
// waits) and returns it locked.
func (s *Scheduler) lockSession(k sessionKey) *sync.Mutex {
	s.mu.Lock()
	mu := s.locks[k]
	if mu == nil {
		mu = &sync.Mutex{}
		s.locks[k] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	return mu
}

// Start re-arms every loop mirrored in session metadata. Occurrences that
// passed while no scheduler ran are spent, not replayed; a loop whose run was
// archived, or whose session or an ancestor was cancelled, meanwhile ends.
// Call it once, before any other method.
func (s *Scheduler) Start(ctx context.Context) error {
	sessions, err := s.store.ListSessionsWithMetadata(ctx, MetadataKey)
	if err != nil {
		return fmt.Errorf("list sessions with loops: %w", err)
	}
	// Load everything before changing anything: ending one loop rewrites its
	// session's list from memory, which must already hold the others.
	type loaded struct {
		sess sessionKey
		ids  []string
	}
	var all []loaded
	found := map[key]*armed{}
	for _, sess := range sessions {
		loops, err := decodeLoops(sess.Metadata)
		if err != nil {
			slog.Warn("loops: unreadable session metadata; not re-armed",
				"run_id", sess.RunID, "session_id", sess.SessionID, "error", err)
			continue
		}
		l := loaded{sess: sessionKey{sess.RunID, sess.SessionID}}
		for _, lp := range loops {
			rule, err := Parse(lp.Schedule, s.loc) // a nil rule ends below
			found[key{sess.RunID, sess.SessionID, lp.ID}] = &armed{Loop: lp, rule: rule, parseErr: err}
			l.ids = append(l.ids, lp.ID)
		}
		all = append(all, l)
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("loop scheduler already started")
	}
	s.started = true
	maps.Copy(s.loops, found)
	s.inflight.Add(1)
	s.mu.Unlock()
	defer s.inflight.Done()

	rearmed, total := 0, 0
	for _, l := range all {
		total += len(l.ids)
		rearmed += s.rearmSession(ctx, l.sess, l.ids)
	}
	if total > 0 {
		slog.Info("loops re-armed", "loops", rearmed, "ended", total-rearmed)
	}
	return nil
}

// rearmSession arms a session's loops at startup, or ends them if their run
// was archived, their session (or an ancestor) cancelled, or their schedule
// can't be read. It returns how many it armed.
func (s *Scheduler) rearmSession(ctx context.Context, sk sessionKey, ids []string) int {
	mu := s.lockSession(sk)
	defer mu.Unlock()
	reason := ""
	if run, err := s.store.GetRun(ctx, sk.run); err == nil && run.Archived {
		reason = EndedRunArchived
	} else if c, err := s.cancelledAncestor(ctx, sk.run, sk.session); err == nil && c != "" {
		reason = cancelReason(c, sk.session)
	}
	now := s.clock.Now()
	armed := 0
	for _, id := range ids {
		k := key{sk.run, sk.session, id}
		s.mu.Lock()
		a := s.loops[k]
		r := reason
		if a != nil && a.rule == nil && r == "" {
			r = fmt.Sprintf("unreadable schedule: %v", a.parseErr)
		}
		if a != nil && r == "" && now.After(a.Spent) {
			a.Spent = now // missed occurrences aren't replayed
		}
		s.mu.Unlock()
		switch {
		case a == nil:
		case r != "":
			s.end(ctx, k, r)
		case s.arm(ctx, k):
			armed++
		}
	}
	return armed
}

// Shutdown stops every timer and waits for work in progress; the scheduler
// does nothing after it. Loops stay in metadata for the next Start.
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	s.closed = true
	for _, a := range s.loops {
		if a.timer != nil {
			a.timer.Stop()
		}
	}
	s.mu.Unlock()
	s.inflight.Wait()
}

func checkPrompt(prompt string) error {
	switch {
	case strings.TrimSpace(prompt) == "":
		return invalidf("prompt is required")
	case len(prompt) > maxPromptLen:
		return invalidf("prompt is %d bytes; the limit is %d", len(prompt), maxPromptLen)
	}
	return nil
}

// Add sets a new loop on a session and arms it; a schedule whose first
// occurrence is due fires at once. With v, each tick starts a new version of
// the session's verification with it.
func (s *Scheduler) Add(ctx context.Context, runID, sessionID, prompt string, sched event.Schedule, v *event.LoopVerify) (Info, error) {
	if err := checkPrompt(prompt); err != nil {
		return Info{}, err
	}
	sched.Spec = NormalizeSpec(sched.Spec)
	rule, err := Parse(sched, s.loc)
	if err != nil {
		return Info{}, err
	}
	if err := s.enter(); err != nil {
		return Info{}, err
	}
	defer s.inflight.Done()
	// Before taking its lock, so a missing session leaves no lock behind.
	if _, err := s.store.GetSession(ctx, runID, sessionID); errors.Is(err, sql.ErrNoRows) {
		return Info{}, fmt.Errorf("%w: no session %q in run %q", ErrNotFound, sessionID, runID)
	} else if err != nil {
		return Info{}, err
	}
	sk := sessionKey{runID, sessionID}
	mu := s.lockSession(sk)
	defer mu.Unlock()
	lineage, err := s.lineage(ctx, runID, sessionID) // again, under the lock: loop_seq
	if err != nil {
		return Info{}, err
	}
	sess := &lineage[0]
	if err := checkNotVerifier(lineage); err != nil {
		return Info{}, err
	}
	if err := s.checkVerify(sk, "", sess, v); err != nil {
		return Info{}, err
	}
	if c := cancelledIn(lineage); c != "" {
		return Info{}, invalidf("session %s is cancelled, and a loop's ticks must not revive it (or, through a sub-agent's result, its cancelled ancestor); send it a message first", c)
	}
	if n := s.count(sk); n >= maxLoops {
		return Info{}, invalidf("session %s has %d loops, as many as a session can have; stop one first", sessionID, n)
	}
	if run, err := s.store.GetRun(ctx, runID); err != nil {
		return Info{}, err
	} else if run.Archived {
		return Info{}, invalidf("run %s is archived", runID)
	}
	now := s.clock.Now()
	l := Loop{Prompt: prompt, Schedule: sched, Verify: v, CreatedAt: now, Spent: now.Add(-addGrace)}
	if err := checkStart(rule, now); err != nil {
		return Info{}, err
	}
	if err := hasNext(rule, l.Spent); err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	seq := s.nextIDLocked(sk, sess.Metadata)
	l.ID = strconv.Itoa(seq)
	md := s.mirrorLocked(sk, func(m map[string]Loop) { m[l.ID] = l })
	s.mu.Unlock()
	md[seqKey] = seq
	if _, err := s.store.AppendEventWithMetadata(ctx, runID, sessionID, &event.LoopEvent{
		Action: event.LoopSet, ID: l.ID, Prompt: prompt, Schedule: &l.Schedule, Summary: rule.Describe(), Verify: v,
		By: event.ByOperator,
	}, md); err != nil {
		return Info{}, err
	}
	k := key{runID, sessionID, l.ID}
	s.mu.Lock()
	s.loops[k] = &armed{Loop: l, rule: rule}
	s.mu.Unlock()
	slog.Info("loop set", "run_id", runID, "session_id", sessionID, "loop", l.ID, "schedule", rule.Describe())
	s.arm(ctx, k)
	return s.info(k), nil
}

// checkNotVerifier refuses a loop on a verifier or a session under one
// (lineage: the session, then its ancestors). The harness starts and
// re-engages a verifier; a tick would revive it, directly or through a
// sub-agent's result.
func checkNotVerifier(lineage []db.SessionRecord) error {
	for i := range lineage {
		switch v := verify.VerifierFor(&lineage[i]); {
		case v == "":
		case i == 0:
			return invalidf("session %s is %s's verifier, and a loop's ticks would revive it", lineage[0].SessionID, v)
		default:
			return invalidf("session %s is under %s, %s's verifier, and a loop's ticks would revive that verifier through its results",
				lineage[0].SessionID, lineage[i].SessionID, v)
		}
	}
	return nil
}

// count is how many loops a session has.
func (s *Scheduler) count(sk sessionKey) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.loops {
		if k.sess() == sk {
			n++
		}
	}
	return n
}

// checkVerify refuses a loop that verifies its ticks (v) where verification
// can't apply: on a session that never concludes, without a VerifyFunc, or
// beside another of the session's loops (but loop except) that verifies its
// ticks: a session has one verification, and each would replace the other's.
func (s *Scheduler) checkVerify(sk sessionKey, except string, sess *db.SessionRecord, v *event.LoopVerify) error {
	switch {
	case v == nil:
		return nil
	case s.verify == nil:
		return invalidf("this server can't verify a loop's ticks")
	}
	if err := verify.CanVerify(sess); err != nil {
		return invalidf("verify each tick: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range s.loops {
		if k.sess() == sk && k.id != except && a.Verify != nil {
			return invalidf("loop %s already verifies each tick of %s, and a session has one verification at a time; "+
				"stop that loop's verification (or the loop) first", k.id, sk.session)
		}
	}
	return nil
}

// checkStart refuses a DTSTART more than maxStartAge before now, and a
// DTSTART or RDATE more than maxAhead after it.
func checkStart(rule *Rule, now time.Time) error {
	start := rule.Start()
	if start.Before(now.Add(-maxStartAge)) {
		return invalidf("DTSTART %s is more than a year before now; start the schedule from now", start.Format(time.RFC3339))
	}
	for _, t := range append([]time.Time{start}, rule.set.GetRDate()...) {
		if t.After(now.Add(maxAhead)) {
			return invalidf("%s is more than 100 years from now", t.Format(time.RFC3339))
		}
	}
	return nil
}

// hasNext rejects a schedule with no occurrence after t.
func hasNext(rule *Rule, t time.Time) error {
	next, err := rule.Next(t)
	if err != nil {
		return err
	}
	if next.IsZero() {
		return invalidf("the schedule has no tick after now")
	}
	return nil
}

// metaInt reads a number from session metadata: a float64 once it has been
// through JSON, an int if a caller stored one directly; 0 otherwise.
func metaInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// nextIDLocked returns the session's next loop id: one past the last handed
// out, so an ended loop's id is never reused.
func (s *Scheduler) nextIDLocked(sk sessionKey, meta map[string]any) int {
	seq := metaInt(meta[seqKey])
	for k := range s.loops {
		if n, err := strconv.Atoi(k.id); err == nil && k.sess() == sk {
			seq = max(seq, n)
		}
	}
	return seq + 1
}

// Change is what an Edit changes; nil fields stay as they are.
type Change struct {
	Prompt   *string
	Schedule *event.Schedule
	// Verify makes the loop verify each tick with it from the next tick on;
	// NoVerify makes it stop, and its next tick turns off the version the
	// loop started, if that is still in force.
	Verify   *event.LoopVerify
	NoVerify bool
}

// Edit changes a loop's prompt, schedule, verification, or several. A new
// schedule applies from now: its first occurrence (up to addGrace ago, but not
// at or before the last tick) is due, though still no sooner than a minute
// after the occurrence the last tick was sent for; the tick count carries on.
func (s *Scheduler) Edit(ctx context.Context, runID, sessionID, id string, c Change) (Info, error) {
	prompt, sched := c.Prompt, c.Schedule
	switch {
	case prompt == nil && sched == nil && c.Verify == nil && !c.NoVerify:
		return Info{}, invalidf("nothing to change: give a prompt, a schedule, a verification request, or stop verifying")
	case c.Verify != nil && c.NoVerify:
		return Info{}, invalidf("verify each tick, or stop verifying: not both")
	}
	if prompt != nil {
		if err := checkPrompt(*prompt); err != nil {
			return Info{}, err
		}
	}
	var rule *Rule
	if sched != nil {
		normalized := *sched
		normalized.Spec = NormalizeSpec(sched.Spec)
		sched = &normalized
		var err error
		if rule, err = Parse(normalized, s.loc); err != nil {
			return Info{}, err
		}
	}
	if err := s.enter(); err != nil {
		return Info{}, err
	}
	defer s.inflight.Done()
	k := key{runID, sessionID, id}
	mu := s.lockSession(k.sess())
	defer mu.Unlock()
	s.mu.Lock()
	a := s.loops[k]
	s.mu.Unlock()
	if a == nil {
		return Info{}, fmt.Errorf("%w: no active loop %q on session %q", ErrNotFound, id, sessionID)
	}
	next, nextRule := a.Loop, a.rule
	if prompt != nil {
		next.Prompt = *prompt
	}
	stopped := c.NoVerify && a.Verify != nil
	switch {
	case c.NoVerify && !stopped && prompt == nil && sched == nil:
		return Info{}, invalidf("loop %s doesn't verify its ticks; nothing to change", id)
	case stopped:
		next.Verify = nil
	case c.Verify != nil:
		sess, err := s.store.GetSession(ctx, runID, sessionID)
		if err != nil {
			return Info{}, err
		}
		if err := s.checkVerify(k.sess(), id, sess, c.Verify); err != nil {
			return Info{}, err
		}
		next.Verify = c.Verify
	}
	if sched != nil {
		// Nothing at or before the last tick comes due again.
		now := s.clock.Now()
		next.Schedule, nextRule, next.Spent = *sched, rule, maxTime(now.Add(-addGrace), a.LastTick)
		if err := checkStart(rule, now); err != nil {
			return Info{}, err
		}
		if err := hasNext(rule, next.Spent); err != nil {
			return Info{}, err
		}
	}
	s.mu.Lock()
	md := s.mirrorLocked(k.sess(), func(m map[string]Loop) { m[id] = next })
	s.mu.Unlock()
	if _, err := s.store.AppendEventWithMetadata(ctx, runID, sessionID, &event.LoopEvent{
		Action: event.LoopEdit, ID: id, Prompt: next.Prompt, Schedule: &next.Schedule, Summary: nextRule.Describe(),
		Verify: next.Verify, VerifyStopped: stopped, By: event.ByOperator,
	}, md); err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	if s.loops[k] == a {
		a.Loop, a.rule = next, nextRule
	}
	s.mu.Unlock()
	slog.Info("loop edited", "run_id", runID, "session_id", sessionID, "loop", id, "schedule", nextRule.Describe())
	if sched != nil {
		s.arm(ctx, k)
	}
	return s.info(k), nil
}

// Stop ends a loop at the operator's request.
func (s *Scheduler) Stop(ctx context.Context, runID, sessionID, id string) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.inflight.Done()
	k := key{runID, sessionID, id}
	mu := s.lockSession(k.sess())
	defer mu.Unlock()
	a, err := s.remove(ctx, k, &event.LoopEvent{Action: event.LoopStop, ID: id, By: event.ByOperator})
	switch {
	case err != nil:
		return err
	case a == nil:
		return fmt.Errorf("%w: no active loop %q on session %q", ErrNotFound, id, sessionID)
	}
	slog.Info("loop stopped", "run_id", runID, "session_id", sessionID, "loop", id, "ticks", a.Ticks)
	return nil
}

// List returns a session's active loops (every session's, for sessionID ""),
// in id order.
func (s *Scheduler) List(runID, sessionID string) []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Info
	for k, a := range s.loops {
		if k.run == runID && (sessionID == "" || k.session == sessionID) {
			out = append(out, infoOf(k, a))
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return cmpLoops(a.SessionID, a.ID, b.SessionID, b.ID) })
	return out
}

func (s *Scheduler) info(k key) Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.loops[k]; a != nil {
		return infoOf(k, a)
	}
	return Info{} // ended at once (its schedule had no tick left)
}

func infoOf(k key, a *armed) Info {
	summary := a.Schedule.Spec // a schedule Start couldn't parse, about to end
	if a.rule != nil {
		summary = a.rule.Describe()
	}
	return Info{Loop: a.Loop, RunID: k.run, SessionID: k.session, Summary: summary, NextTick: a.at}
}

// EndRun ends every loop of a run, recording why (archived, cancelled).
func (s *Scheduler) EndRun(ctx context.Context, runID, reason string) {
	s.endWhere(ctx, func(k key) bool { return k.run == runID }, func(string) string { return reason })
}

// OnSessionStatus is a db.SessionStatusListener. A session's going running
// marks its loops' last tick as having woken it (see unrevived). A session's
// cancellation, by whatever path, ends its loops and those of every session
// under it, at once. (Cancelling skips descendants that had already finished,
// and one of those, revived by a tick, would revive the cancelled session with
// its result.) It returns at once and does that work on another goroutine,
// since the canceller may be anywhere; a tick's write checks for a cancelled
// ancestor too, for a cancel this process didn't see or hasn't acted on yet.
func (s *Scheduler) OnSessionStatus(runID, sessionID, status string) {
	if status == db.SessionOngoing {
		s.mu.Lock()
		for k, a := range s.loops {
			if k.run == runID && k.session == sessionID {
				a.ran = true
			}
		}
		s.mu.Unlock()
		return
	}
	if status != db.SessionCancelled || !s.hasLoopsIn(runID) {
		return
	}
	go func() {
		if s.enter() != nil {
			return
		}
		defer s.inflight.Done()
		ctx := context.Background()
		subtree, err := s.subtree(ctx, runID, sessionID)
		if err != nil {
			slog.Warn("loops: listing a cancelled session's sub-agents failed; their next tick ends them",
				"run_id", runID, "session_id", sessionID, "error", err)
		}
		s.endWhere(ctx, func(k key) bool { return k.run == runID && slices.Contains(subtree, k.session) },
			func(sid string) string { return cancelReason(sessionID, sid) })
	}()
}

func (s *Scheduler) hasLoopsIn(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.loops {
		if k.run == runID {
			return true
		}
	}
	return false
}

// endWhere ends the loops that match, session by session. It marks them ending
// first, so that no tick of theirs starts or, already on its way, lands
// (mayTick) while it waits for their sessions' locks; a run's cancel or
// delete that follows can't miss a session such a tick would have revived.
func (s *Scheduler) endWhere(ctx context.Context, match func(key) bool, reason func(session string) string) {
	if s.enter() != nil {
		return
	}
	defer s.inflight.Done()
	bySession := map[sessionKey][]key{}
	s.mu.Lock()
	for k, a := range s.loops {
		if match(k) {
			a.ending = true
			if a.timer != nil {
				a.timer.Stop()
			}
			bySession[k.sess()] = append(bySession[k.sess()], k)
		}
	}
	s.mu.Unlock()
	for sk, keys := range bySession {
		mu := s.lockSession(sk)
		for _, k := range keys {
			s.end(ctx, k, reason(sk.session))
		}
		mu.Unlock()
	}
}

// ForgetRun drops a run's loops without writing anything: the run is being
// deleted. It returns once no tick for the run is in flight.
func (s *Scheduler) ForgetRun(runID string) {
	s.mu.Lock()
	var locks []*sync.Mutex
	for k, a := range s.loops {
		if k.run == runID {
			if a.timer != nil {
				a.timer.Stop()
			}
			delete(s.loops, k)
		}
	}
	for sk, mu := range s.locks {
		if sk.run == runID {
			locks = append(locks, mu)
			delete(s.locks, sk)
		}
	}
	s.mu.Unlock()
	for _, mu := range locks {
		// Empty on purpose: a tick holding it finishes first; the next finds its
		// loop gone.
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // SA2001: waiting for the holder is the point
	}
}

// earliest is the soonest the loop may tick again: a minute after the
// occurrence its last tick was sent for, and a minute (less tickSlack) after
// that tick by the wall clock.
func earliest(l Loop) time.Time {
	var t time.Time
	if !l.LastDue.IsZero() {
		t = l.LastDue.Add(MinInterval)
	}
	if !l.LastTick.IsZero() {
		t = maxTime(t, l.LastTick.Add(MinInterval-tickSlack))
	}
	return t
}

// arm sets the timer for a loop's next tick: the first occurrence after its
// Spent, but no sooner than earliest. A schedule with no occurrence left ends
// the loop; arm reports whether it armed. The caller holds the session's lock.
func (s *Scheduler) arm(ctx context.Context, k key) bool {
	s.mu.Lock()
	a := s.loops[k]
	s.mu.Unlock()
	if a == nil {
		return false
	}
	next, err := a.rule.Next(a.Spent)
	switch {
	case err != nil:
		s.end(ctx, k, err.Error())
		return false
	case next.IsZero():
		s.end(ctx, k, fmt.Sprintf("schedule finished after %d %s", a.Ticks, plural(a.Ticks, "tick")))
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loops[k] == a {
		s.setTimerLocked(k, a, maxTime(next, earliest(a.Loop)))
	}
	return true
}

func (s *Scheduler) setTimerLocked(k key, a *armed, at time.Time) {
	if a.timer != nil {
		a.timer.Stop()
	}
	if s.closed || a.ending {
		return
	}
	a.gen++
	gen := a.gen
	a.at = at
	wait := min(max(at.Sub(s.clock.Now()), 0), maxWait)
	a.timer = s.clock.AfterFunc(wait, func() { s.fire(k, gen) })
}

func (s *Scheduler) retry(k key, a *armed, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loops[k] == a {
		s.setTimerLocked(k, a, at)
	}
}

// fire is a loop's timer: it sends the tick that is due, or re-arms if none is
// yet. The minute between ticks is enforced here, whatever the schedule says.
func (s *Scheduler) fire(k key, gen uint64) {
	if s.enter() != nil {
		return
	}
	defer s.inflight.Done()
	mu := s.lockSession(k.sess())
	defer mu.Unlock()
	s.mu.Lock()
	a := s.loops[k]
	if a == nil || a.gen != gen || a.ending {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	ctx := context.Background()
	now := s.clock.Now()
	run, err := s.store.GetRun(ctx, k.run)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.forget(k) // deleted without ForgetRun: nothing left to write to
		return
	case err != nil:
		slog.Warn("loop: read run failed; retrying", "run_id", k.run, "loop", k.id, "error", err)
		s.retry(k, a, now.Add(retryAfter))
		return
	case run.Archived:
		s.end(ctx, k, EndedRunArchived)
		return
	}
	due, _, err := a.rule.window(a.Spent, now)
	switch {
	case err != nil:
		s.end(ctx, k, err.Error())
		return
	case due.IsZero(): // early: a capped wait, or the wall clock moved
		s.arm(ctx, k)
		return
	case now.Before(earliest(a.Loop)):
		s.retry(k, a, earliest(a.Loop))
		return
	}
	// A tick is due. It would revive a cancelled session (or a finished one
	// under a cancelled ancestor, whose result would revive that), so the loop
	// ends instead; the tick's write checks again, for a cancel meanwhile. A
	// crashed one is revived, as a typed message would.
	lineage, err := s.lineage(ctx, k.run, k.session)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.forget(k)
		return
	case err != nil:
		slog.Warn("loop: read session failed; retrying", "run_id", k.run, "session_id", k.session, "loop", k.id, "error", err)
		s.retry(k, a, now.Add(retryAfter))
		return
	}
	if c := cancelledIn(lineage); c != "" {
		s.end(ctx, k, cancelReason(c, k.session))
		return
	}
	s.mu.Lock()
	reason := a.unrevived(&lineage[0])
	// From here, the session's going running means this tick woke it: it may
	// do so before the tick's write returns.
	a.ran = false
	s.mu.Unlock()
	if reason != "" {
		s.end(ctx, k, reason)
		return
	}
	next, nextRule := a.Loop, a.rule
	next.Ticks++
	next.LastTick, next.LastDue = now.Truncate(time.Second), due
	next.Spent = now // with the due one, any earlier occurrence is passed over
	if a.rule.Start().Before(now.Add(-rebaseAge)) {
		if r, ok := a.rule.Rebase(due); ok {
			next.Schedule.Spec, nextRule = r.Spec(), r
		}
	}
	next.Verified = a.Verify != nil
	version, step, err := s.sendTick(ctx, k, a, next)
	var cancelled cancelledError
	switch {
	case errors.As(err, &cancelled):
		s.end(ctx, k, cancelReason(cancelled.session, k.session))
		return
	case errors.Is(err, errEnding):
		return // whoever is ending it records why
	case err != nil:
		s.tickFailed(ctx, k, a, now, due, err)
		return
	}
	s.mu.Lock()
	if s.loops[k] == a {
		a.Loop, a.rule, a.failures, a.unread = next, nextRule, 0, nil
		if atRest[lineage[0].Status] {
			a.unread = &unreadTick{n: next.Ticks, step: step}
		}
	}
	s.mu.Unlock()
	slog.Debug("loop tick", "run_id", k.run, "session_id", k.session, "loop", k.id, "n", next.Ticks, "verify_version", version)
	s.arm(ctx, k)
}

// sendTick writes the tick of next (the loop as it is once the tick is sent)
// with its mirror in metadata, and returns the verification version it
// started and the step it landed at. A loop that verifies its ticks starts a
// new version with each tick (each tick is a new task), which the tick's
// label names ("· verification v3"), and one that stopped turns its own
// version off, both through s.verify, in the tick's write; an error from
// s.verify after the write is logged, as the tick went out. It returns an
// error if the tick wasn't written: mayTick's, if a cancel or an end got
// there first. The caller holds the session's lock.
func (s *Scheduler) sendTick(ctx context.Context, k key, a *armed, next Loop) (version, step int, err error) {
	s.mu.Lock()
	md := s.mirrorLocked(k.sess(), func(m map[string]Loop) { m[k.id] = next })
	s.mu.Unlock()
	written := false
	write := func(change *event.VerifyEvent, v int) error {
		label := fmt.Sprintf("loop %s · %s · tick %d", k.id, a.rule.Describe(), next.Ticks)
		if v > 0 {
			label += fmt.Sprintf(" · verification v%d", v)
		}
		tick := &event.UserEvent{
			Content: "[" + label + "] " + next.Prompt,
			Loop:    &event.LoopTick{ID: k.id, N: next.Ticks, Version: v},
		}
		evts := []event.Event{tick}
		if change != nil {
			evts = []event.Event{change, tick}
		}
		if err := s.store.AppendEventsIf(ctx, k.run, k.session, s.mayTick(k, a), md, evts...); err != nil {
			return err
		}
		written, version, step = true, v, tick.Step
		return nil
	}
	switch {
	case a.Verify == nil && !a.Verified:
		err = write(nil, 0)
		return version, step, err
	case s.verify == nil && a.Verify != nil:
		return 0, 0, errors.New("this server can't verify a loop's ticks")
	case s.verify == nil: // nothing can have started a version to turn off
		err = write(nil, 0)
		return version, step, err
	}
	_, err = s.verify(ctx, k.run, k.session, k.id, a.Verify, write)
	switch {
	case !written && err == nil:
		return 0, 0, errors.New("the tick wasn't written")
	case !written:
		return 0, 0, err
	case err != nil:
		slog.Warn("loop: tick sent, but changing its verification reported an error", "run_id", k.run,
			"session_id", k.session, "loop", k.id, "version", version, "error", err)
	}
	return version, step, nil
}

// tickFailed handles a tick that couldn't be sent: it is retried, backing off
// (retryAfter, doubled with each failure in a row, up to maxWait), until it
// is sent; or, once the schedule has no occurrence left and the tick has been
// due for giveUpAfter, the loop ends without it.
func (s *Scheduler) tickFailed(ctx context.Context, k key, a *armed, now, due time.Time, err error) {
	if next, nerr := a.rule.Next(now); nerr == nil && next.IsZero() && now.Sub(due) >= giveUpAfter {
		s.end(ctx, k, fmt.Sprintf("its last tick couldn't be sent: %v", err))
		return
	}
	s.mu.Lock()
	a.failures++
	wait := retryAfter << min(a.failures-1, 6)
	s.mu.Unlock()
	wait = min(wait, maxWait)
	slog.Warn("loop: tick not sent; retrying", "run_id", k.run, "session_id", k.session, "loop", k.id,
		"in", wait, "error", err)
	s.retry(k, a, now.Add(wait))
}

// errEnding refuses the write of a tick whose loop is ending or gone.
var errEnding = errors.New("the loop is ending")

// cancelledError refuses the write of a tick under a cancelled session.
type cancelledError struct{ session string }

func (e cancelledError) Error() string { return "session " + e.session + " is cancelled" }

// mayTick is checked inside a tick's write, for what may have changed since
// fire checked: the loop is still armed (endWhere marks it ending, and
// ForgetRun drops it, before they wait for its session), and no session from
// it to the root is cancelled.
func (s *Scheduler) mayTick(k key, a *armed) func([]db.SessionRecord) error {
	return func(lineage []db.SessionRecord) error {
		s.mu.Lock()
		live := s.loops[k] == a && !a.ending
		s.mu.Unlock()
		if !live {
			return errEnding
		}
		if c := cancelledIn(lineage); c != "" {
			return cancelledError{c}
		}
		return nil
	}
}

// unrevived says why the loop ends if the last tick, which this process sent
// to sess at rest, never got it running: reviving it failed (an unknown agent
// type, an invalid workspace; the server log says which), and would for every
// tick to come. A session that ran is not unrevived, though it is at rest again
// at the same step: the turn the tick revived it for can fail as the one before
// did (a model that is still down), without reaching its next step. The caller
// holds the session's lock and s.mu.
func (a *armed) unrevived(sess *db.SessionRecord) string {
	u := a.unread
	if u == nil || a.ran || !atRest[sess.Status] || sess.CurrentStep > u.step {
		return ""
	}
	return fmt.Sprintf("tick %d didn't wake the session, still %s (the server log says why)", u.n, sess.Status)
}

func (s *Scheduler) forget(k key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.loops[k]; a != nil && a.timer != nil {
		a.timer.Stop()
	}
	delete(s.loops, k)
}

// end removes a loop and records that it ended. The caller holds the
// session's lock.
func (s *Scheduler) end(ctx context.Context, k key, reason string) {
	a, err := s.remove(ctx, k, &event.LoopEvent{Action: event.LoopEnded, ID: k.id, By: event.ByHarness, Reason: reason})
	switch {
	case err != nil:
		slog.Warn("loop: recording its end failed; it stops ticking now, but session metadata still lists it, "+
			"so the next start checks it again, unless a change to another of the session's loops drops it first",
			"run_id", k.run, "session_id", k.session, "loop", k.id, "reason", reason, "error", err)
	case a != nil:
		slog.Info("loop ended", "run_id", k.run, "session_id", k.session, "loop", k.id, "reason", reason)
	}
}

// remove takes a loop out of memory and writes evt with the session's
// remaining loops; it returns the loop (nil if there was none). The caller
// holds the session's lock.
func (s *Scheduler) remove(ctx context.Context, k key, evt *event.LoopEvent) (*armed, error) {
	s.mu.Lock()
	a := s.loops[k]
	if a == nil {
		s.mu.Unlock()
		return nil, nil
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	delete(s.loops, k)
	md := s.mirrorLocked(k.sess(), nil)
	s.mu.Unlock()
	if _, err := s.store.AppendEventWithMetadata(ctx, k.run, k.session, evt, md); err != nil {
		if evt.Action == event.LoopStop {
			s.mu.Lock()
			s.loops[k] = a // the stop didn't happen
			s.setTimerLocked(k, a, a.at)
			s.mu.Unlock()
		}
		return a, err
	}
	return a, nil
}

// mirrorLocked is the metadata update mirroring a session's active loops after
// change (null when it has none, so ListSessionsWithMetadata skips it).
func (s *Scheduler) mirrorLocked(sk sessionKey, change func(map[string]Loop)) map[string]any {
	byID := map[string]Loop{}
	for k, a := range s.loops {
		if k.sess() == sk {
			byID[k.id] = a.Loop
		}
	}
	if change != nil {
		change(byID)
	}
	if len(byID) == 0 {
		return map[string]any{MetadataKey: nil}
	}
	loops := slices.Collect(maps.Values(byID))
	slices.SortFunc(loops, func(a, b Loop) int { return cmpLoops("", a.ID, "", b.ID) })
	stored := make([]mirrored, len(loops))
	for i, l := range loops {
		stored[i] = mirrored(l)
	}
	return map[string]any{MetadataKey: stored}
}

// cancelledAncestor returns the session itself or its nearest ancestor that is
// cancelled, or "" if none is.
func (s *Scheduler) cancelledAncestor(ctx context.Context, runID, sessionID string) (string, error) {
	lineage, err := s.lineage(ctx, runID, sessionID)
	if err != nil {
		return "", err
	}
	return cancelledIn(lineage), nil
}

// lineage returns a session and its ancestors, nearest first.
func (s *Scheduler) lineage(ctx context.Context, runID, sessionID string) ([]db.SessionRecord, error) {
	var out []db.SessionRecord
	for id := sessionID; id != "" && len(out) < maxDepth; {
		sess, err := s.store.GetSession(ctx, runID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
		id = sess.ParentID
	}
	return out, nil
}

// cancelledIn returns the first cancelled session in lineage, or "".
func cancelledIn(lineage []db.SessionRecord) string {
	for _, sess := range lineage {
		if sess.Status == db.SessionCancelled {
			return sess.SessionID
		}
	}
	return ""
}

// subtree returns a session and every session under it.
func (s *Scheduler) subtree(ctx context.Context, runID, sessionID string) ([]string, error) {
	out := []string{sessionID}
	for i := 0; i < len(out); i++ {
		children, err := s.store.GetChildSessions(ctx, runID, out[i])
		if err != nil {
			return out, err
		}
		for _, c := range children {
			out = append(out, c.SessionID)
		}
	}
	return out, nil
}

// cancelReason is why a session's loop ended when cancelled, itself or an
// ancestor of it.
func cancelReason(cancelled, sessionID string) string {
	if cancelled == sessionID {
		return EndedSessionCancelled
	}
	return "session " + cancelled + " cancelled"
}

// decodeLoops reads the loops mirrored in session metadata.
func decodeLoops(meta map[string]any) ([]Loop, error) {
	raw, err := json.Marshal(meta[MetadataKey])
	if err != nil {
		return nil, err
	}
	var stored []mirrored
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	loops := make([]Loop, len(stored))
	for i, m := range stored {
		loops[i] = Loop(m)
	}
	return loops, nil
}

// cmpLoops orders loops by session, then numerically by id.
func cmpLoops(sa, ia, sb, ib string) int {
	if c := strings.Compare(sa, sb); c != 0 {
		return c
	}
	na, ea := strconv.Atoi(ia)
	nb, eb := strconv.Atoi(ib)
	if ea == nil && eb == nil && na != nb {
		return na - nb
	}
	return strings.Compare(ia, ib)
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
