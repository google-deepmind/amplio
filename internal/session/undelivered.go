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

package session

import (
	"context"
	"log/slog"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
)

// WarnUndelivered logs a session whose goroutine has just released its slot
// (at left) while, concluded or idle, it holds an Input it never read. The
// commit notifier wakes a session that is still registered rather than
// reviving it, so an Input that lands while a session stops (during its last
// model call, or between its final status and the release of its slot) wakes
// only the leaving goroutine, and stays unread until something else revives
// the session. This is a diagnostic only, to show whether that window is hit
// in practice; nothing is redelivered.
//
// A cancelled or crashed session keeps its unread Inputs by design, and an
// environment notification doesn't revive a concluded one (the notifier's
// rule), so neither is reported. An Input committed after left went through
// the notifier to a session without a goroutine, which revives it.
func WarnUndelivered(store db.Store, reg *Registry, runID, sessionID string, left time.Time) {
	if store == nil || (reg != nil && reg.IsRegistered(sessionID)) {
		return // revived already
	}
	ctx := context.Background()
	sess, err := store.GetSession(ctx, runID, sessionID)
	if err != nil || (sess.Status != db.SessionConcluded && sess.Status != db.SessionIdle) {
		return
	}
	step := sess.CurrentStep
	recs, err := store.GetEvents(ctx, runID, sessionID, db.EventFilter{StartStep: &step})
	if err != nil {
		return
	}
	var unread []db.EventRecord
	for _, rec := range recs {
		if !db.IsInput(rec.Event) || rec.CreatedAt.After(left) {
			continue
		}
		if m, ok := rec.Event.(*event.MessageEvent); ok && m.SenderType == event.SenderTypeEnvironment &&
			sess.Status == db.SessionConcluded {
			continue
		}
		unread = append(unread, rec)
	}
	if len(unread) == 0 || (reg != nil && reg.IsRegistered(sessionID)) {
		return
	}
	first := unread[0]
	slog.Warn("an input reached the session as it stopped, and wasn't delivered",
		"run_id", runID, "session_id", sessionID, "status", sess.Status, "inputs", len(unread),
		"first", first.Event.EventType(), "first_at", first.CreatedAt, "status_changed_at", sess.StatusChangedAt)
}
