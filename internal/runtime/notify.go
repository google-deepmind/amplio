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

package runtime

import (
	"amplio/internal/db"
	"amplio/internal/event"
)

// RespawnFunc is called when a respawn-worthy event arrives for a session that
// has no active goroutine.
type RespawnFunc func(runID, sessionID string)

// StatusFunc reports a session's current status (ok=false if it can't be read).
// The commit notifier uses it to gate environment revival of finished sessions.
// It may be nil (then no env-revival gating is applied).
type StatusFunc func(runID, sessionID string) (status string, ok bool)

// AwaitsVerdictFunc reports whether a session's verification has a candidate
// with verifier, waiting for its verdict. The commit notifier uses it to revive
// a crashed agent with its verifier's verdict (see NewCommitNotifier). It may
// be nil (then no outcome record revives anyone).
type AwaitsVerdictFunc func(runID, sessionID, verifier string) bool

// envUnrevivableStatuses is the set of session statuses that an ENVIRONMENT
// notification ($AMPLIO_NOTIFY) must NOT revive — the *finished* states, where
// the session has stopped for good. Deliberately scoped to THIS policy. In
// particular `crashed` is the debatable member — a crash isn't a deliberate
// stop — so drop it from this set (only) if env should be allowed to nudge a
// crashed agent back.
//
// The PARKED states (idle/awaiting) are intentionally ABSENT: an idle
// interactive agent and an awaiting agent are waiting, not finished, so a
// background job SHOULD still wake them.
var envUnrevivableStatuses = map[string]bool{
	db.SessionConcluded: true,
	db.SessionCancelled: true,
	db.SessionCrashed:   true,
}

// NewCommitNotifier returns a db.CommitListener that bridges committed events
// to the per-run session registries. It is the synchronous, lossless wake path
// (installed via Store.SetCommitListener):
//
//   - If the target session is live, every event wakes it (bumps its waiter),
//     except a record-only one (db.IsRecordOnly).
//   - If the target session is cold, only an Input-class event (db.IsInput)
//     revives it; other events just persist.
//   - EXCEPTION: an environment notification ($AMPLIO_NOTIFY) does NOT revive a
//     cold TERMINAL session (concluded/crashed/cancelled). See the gate below.
//   - EXCEPTION: a verifier's verdict (a VerifyEvent verdict record, a Notice)
//     revives a CRASHED agent whose candidate it judges (awaitsVerdict): its
//     revived turn has the verdict to read, and acts on it, as a sub-agent's
//     result revives a crashed parent. It revives nothing else: a turn with
//     nothing pending would have nothing to read.
func NewCommitNotifier(runReg *RunRegistry, respawn RespawnFunc, statusOf StatusFunc, awaitsVerdict AwaitsVerdictFunc) db.CommitListener {
	return func(runID, sessionID string, evt event.Event) {
		// A ToolResultEvent is the session's OWN turn output, never an external
		// wake. Results are now appended one-by-one as each tool finishes, so
		// they commit MID-STEP while an await_event sibling is parked — waking on
		// them would make await return immediately on its own step's results
		// (the bug this guards). await waits for FUTURE external events, which
		// arrive as UserEvent / ChildResultEvent, not ToolResultEvent. Nor do a
		// change to a verification (a VerifyEvent other than a verifier's
		// outcome) or a newer binary's event wake anyone (db.IsRecordOnly lists
		// them all). (The UI still updates: that rides the separate
		// EventAppended bus, not this wake path.)
		if db.IsRecordOnly(evt) {
			return
		}
		reg := runReg.Get(runID)
		if reg != nil && reg.Notify(sessionID) {
			return // woke a live session
		}
		if respawn == nil {
			return
		}
		if !db.IsInput(evt) {
			if verdictRevives(runID, sessionID, evt, statusOf, awaitsVerdict) {
				respawn(runID, sessionID)
			}
			return
		}
		// Environment notifications ($AMPLIO_NOTIFY) must not resurrect a cold
		// TERMINAL state that is a deliberate stop.
		if isEnvNotification(evt) && statusOf != nil {
			if status, ok := statusOf(runID, sessionID); ok && envUnrevivableStatuses[status] {
				return // persist as Notice; leave the finished session at rest
			}
		}
		respawn(runID, sessionID)
	}
}

// isEnvNotification reports whether evt is an $AMPLIO_NOTIFY environment message
// (SenderType==environment), as opposed to an agent-to-agent send_message.
func isEnvNotification(evt event.Event) bool {
	m, ok := evt.(*event.MessageEvent)
	return ok && m.SenderType == event.SenderTypeEnvironment
}

// verdictRevives reports whether evt, a cold session's Notice, is a
// verifier's verdict for the crashed agent's pending candidate.
func verdictRevives(runID, sessionID string, evt event.Event, statusOf StatusFunc, awaitsVerdict AwaitsVerdictFunc) bool {
	v, ok := evt.(*event.VerifyEvent)
	if !ok || v.Action != event.VerifyVerdict || v.Outcome == nil || v.Outcome.Stopped() ||
		statusOf == nil || awaitsVerdict == nil {
		return false
	}
	status, ok := statusOf(runID, sessionID)
	return ok && status == db.SessionCrashed && awaitsVerdict(runID, sessionID, v.Outcome.Verifier)
}
