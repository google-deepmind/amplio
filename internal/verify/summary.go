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
	"log/slog"
	"sync"

	"amplio/internal/db"
	"amplio/internal/event"
)

// SummaryMetadataKey is the session-metadata key of a session's verification
// Summary.
const SummaryMetadataKey = "verify"

// Summary is what the UI and the API show of a session's verification without
// reading its stream: mirrored into the session's metadata (SummaryMetadataKey)
// after each change by a SummaryWriter. The stream stays the truth (see
// Derive); a missing or stale summary only costs a display.
type Summary struct {
	Status    string `json:"status"` // State.Status: on, verifying, passed, not_verified, off
	Version   int    `json:"version"`
	Round     int    `json:"round,omitempty"`      // State.Round
	MaxRounds int    `json:"max_rounds,omitempty"` // the request's (the last one, once ended); 0: unbounded
	Verifier  string `json:"verifier,omitempty"`   // the agent's verifier, to re-engage or inspect
	Model     string `json:"model,omitempty"`      // the request's model; "" is the agent's own
}

// Summarize is st's Summary, or nil for a session never verified.
func Summarize(st State) *Summary {
	if st.Version == 0 {
		return nil
	}
	s := &Summary{Status: st.Status(), Version: st.Version, Round: st.Round(), Verifier: st.Verifier}
	// The request in force, or once verification ended, the last one: "passed
	// in round 1 of 3" still reads against its limit.
	req := st.Request
	for i := len(st.History) - 1; req == nil && i >= 0; i-- {
		if a := st.History[i].Action; a == event.VerifySet || a == event.VerifyReplace {
			req = st.History[i].VerifyEvent
		}
	}
	if req != nil {
		s.MaxRounds, s.Model = req.MaxRounds, req.Model
	}
	return s
}

// SummaryFrom reads a session's Summary from its metadata (nil if absent):
// as stored (JSON: a map, numbers as float64), or as written in memory.
func SummaryFrom(meta map[string]any) *Summary {
	switch v := meta[SummaryMetadataKey].(type) {
	case *Summary:
		return v
	case Summary:
		return &v
	case map[string]any:
		num := func(k string) int {
			switch n := v[k].(type) {
			case float64:
				return int(n)
			case int:
				return n
			case int64:
				return int(n)
			}
			return 0
		}
		str := func(k string) string { s, _ := v[k].(string); return s }
		return &Summary{Status: str("status"), Version: num("version"), Round: num("round"),
			MaxRounds: num("max_rounds"), Verifier: str("verifier"), Model: str("model")}
	}
	return nil
}

// SummaryWriter keeps each session's Summary in step with its stream: Observe,
// a commit listener, notes a session whose stream took a VerifyEvent, and Run
// rewrites those sessions' summaries from their streams, one at a time. A
// session noted again while it is being rewritten is rewritten again after, so
// the last write always reads the latest stream.
type SummaryWriter struct {
	store   db.Store
	mu      sync.Mutex
	pending map[[2]string]bool
	wake    chan struct{}
	writing sync.Mutex     // one Refresh at a time, so the last to write read the latest stream
	active  sync.WaitGroup // drains under way (Drain waits for Run's)
	closed  bool           // Drain ran: nothing is written after it
}

// NewSummaryWriter returns a SummaryWriter over store; start its Run.
func NewSummaryWriter(store db.Store) *SummaryWriter {
	return &SummaryWriter{store: store, pending: map[[2]string]bool{}, wake: make(chan struct{}, 1)}
}

// Observe is the commit listener: it never blocks the write path.
func (w *SummaryWriter) Observe(runID, sessionID string, evt event.Event) {
	if _, ok := evt.(*event.VerifyEvent); ok {
		w.note(runID, sessionID)
	}
}

// OnSessionStatus is the session-status listener. A session's final verify
// record (passed, not verified) rides on its terminal write, which fires no
// commit listener for the session itself, so a terminal status is noted too.
func (w *SummaryWriter) OnSessionStatus(runID, sessionID, status string) {
	if db.SessionTerminalStatuses[status] {
		w.note(runID, sessionID)
	}
}

func (w *SummaryWriter) note(runID, sessionID string) {
	w.mu.Lock()
	w.pending[[2]string{runID, sessionID}] = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run rewrites the noted sessions' summaries until ctx is done or Drain
// stops the writer. It starts with a backfill (see Backfill).
func (w *SummaryWriter) Run(ctx context.Context) {
	w.Backfill(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
		if !w.drain(ctx) {
			return // Drain stopped the writer
		}
	}
}

// Drain stops the writer, waits for a rewrite Run has under way, and rewrites
// the summaries of the sessions noted so far: a process calls it on its way
// out (before closing the store), so a run's last change isn't left
// unwritten.
func (w *SummaryWriter) Drain(ctx context.Context) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true // no drain starts after this, so the Wait below races no Add
	w.mu.Unlock()
	w.active.Wait()
	w.mu.Lock()
	keys := w.takePending()
	w.mu.Unlock()
	w.refreshAll(ctx, keys)
}

// drain rewrites the noted sessions' summaries; false once Drain stopped the
// writer.
func (w *SummaryWriter) drain(ctx context.Context) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	keys := w.takePending()
	w.active.Add(1)
	w.mu.Unlock()
	defer w.active.Done()
	w.refreshAll(ctx, keys)
	return true
}

// takePending empties the noted set; the caller holds w.mu.
func (w *SummaryWriter) takePending() [][2]string {
	keys := make([][2]string, 0, len(w.pending))
	for k := range w.pending {
		keys = append(keys, k)
	}
	clear(w.pending)
	return keys
}

func (w *SummaryWriter) refreshAll(ctx context.Context, keys [][2]string) {
	for _, k := range keys {
		if err := w.Refresh(ctx, k[0], k[1]); err != nil && ctx.Err() == nil {
			slog.Warn("verification summary not written", "run_id", k[0], "session_id", k[1], "error", err)
		}
	}
}

// Refresh rewrites one session's Summary from its stream.
func (w *SummaryWriter) Refresh(ctx context.Context, runID, sessionID string) error {
	w.writing.Lock()
	defer w.writing.Unlock()
	sess, err := w.store.GetSession(ctx, runID, sessionID)
	if err != nil {
		return err
	}
	st, err := Load(ctx, w.store, runID, sessionID)
	if err != nil {
		return err
	}
	s := Summarize(st)
	if s == nil && SummaryFrom(sess.Metadata) == nil {
		return nil // never verified, and nothing to clear: no write
	}
	var v any // nil clears the key
	if s != nil {
		v = s
	}
	return w.store.MergeSessionMetadata(ctx, runID, sessionID, map[string]any{SummaryMetadataKey: v})
}

// Backfill writes the summary of each verified session that has none: one with
// a verifier (a session whose metadata names it under MetadataVerifierFor),
// verified before summaries were kept. A session whose verification never
// started a verifier gets its summary at its next VerifyEvent.
func (w *SummaryWriter) Backfill(ctx context.Context) {
	verifiers, err := w.store.ListSessionsWithMetadata(ctx, db.MetadataVerifierFor)
	if err != nil {
		slog.Warn("verification summaries: backfill skipped", "error", err)
		return
	}
	done := map[[2]string]bool{}
	for _, v := range verifiers {
		k := [2]string{v.RunID, VerifierFor(&v)}
		if k[1] == "" || done[k] {
			continue
		}
		done[k] = true
		sess, err := w.store.GetSession(ctx, k[0], k[1])
		if err != nil || SummaryFrom(sess.Metadata) != nil {
			continue
		}
		if err := w.Refresh(ctx, k[0], k[1]); err != nil && ctx.Err() == nil {
			slog.Warn("verification summary not backfilled", "run_id", k[0], "session_id", k[1], "error", err)
		}
	}
}
