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
	"slices"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/session"
)

// TestCommitNotifier_EnvRevivalGate verifies that an environment notification
// ($AMPLIO_NOTIFY) does not respawn a cold TERMINAL session (concluded/crashed/
// cancelled), while parked sessions and non-env inputs still respawn. An empty
// RunRegistry makes every target "cold" (Get returns nil), isolating the gate.
func TestCommitNotifier_EnvRevivalGate(t *testing.T) {
	env := &event.MessageEvent{Content: "job done", SenderType: event.SenderTypeEnvironment}
	agentMsg := &event.MessageEvent{Content: "hi", SenderType: event.SenderTypeAgent}
	userMsg := &event.UserEvent{Content: "keep going"}
	tick := &event.UserEvent{Content: "[loop 1 · every 1m · tick 2] report", Loop: &event.LoopTick{ID: "1", N: 2}}
	loopSet := &event.LoopEvent{Action: event.LoopSet, ID: "1", By: event.ByOperator}

	tests := []struct {
		name        string
		status      string // "" = leave unset (unreadable → fail-open)
		evt         event.Event
		wantRespawn bool
	}{
		{"env→concluded suppressed", db.SessionConcluded, env, false},
		{"env→cancelled suppressed", db.SessionCancelled, env, false},
		{"env→crashed suppressed", db.SessionCrashed, env, false},
		{"env→idle revives", db.SessionIdle, env, true},
		{"env→awaiting revives", db.SessionAwaiting, env, true},
		{"env→unreadable revives (fail-open)", "", env, true},
		{"agent msg→concluded revives", db.SessionConcluded, agentMsg, true},
		{"user input→concluded revives", db.SessionConcluded, userMsg, true},
		{"loop tick→concluded revives", db.SessionConcluded, tick, true},
		{"loop tick→cancelled revives", db.SessionCancelled, tick, true},
		{"loop change→idle stays", db.SessionIdle, loopSet, false},
		{"loop change→concluded stays", db.SessionConcluded, loopSet, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var respawned []string
			respawn := func(_, sid string) { respawned = append(respawned, sid) }
			statusOf := func(_, _ string) (string, bool) {
				if tc.status == "" {
					return "", false
				}
				return tc.status, true
			}
			notify := NewCommitNotifier(NewRunRegistry(), respawn, statusOf, nil)
			notify("run", "sess", tc.evt)

			got := len(respawned) == 1
			if got != tc.wantRespawn {
				t.Errorf("respawn=%v, want %v (respawned=%v)", got, tc.wantRespawn, respawned)
			}
		})
	}
}

// A nil StatusFunc disables env-terminal gating (backward-compatible: every
// Input-class event revives a cold session, as before).
func TestCommitNotifier_NilStatusFuncNoGating(t *testing.T) {
	var respawned []string
	notify := NewCommitNotifier(NewRunRegistry(),
		func(_, sid string) { respawned = append(respawned, sid) }, nil, nil)
	notify("run", "sess", &event.MessageEvent{Content: "x", SenderType: event.SenderTypeEnvironment})
	if len(respawned) != 1 {
		t.Errorf("nil statusOf must not gate; respawned=%v", respawned)
	}
}

// A placeholder for a newer binary's event wakes nothing: not a live session,
// nor a cold one.
func TestCommitNotifier_UnknownEventWakesNothing(t *testing.T) {
	runReg := NewRunRegistry()
	live := session.NewHandle(func() {})
	if err := runReg.GetOrCreate("run").Register("live", live); err != nil {
		t.Fatal(err)
	}
	var respawned []string
	notify := NewCommitNotifier(runReg, func(_, sid string) { respawned = append(respawned, sid) }, nil, nil)

	before := live.Counter()
	newer := &event.UnknownEvent{Type: "teleport", Raw: []byte(`{"type":"teleport"}`)}
	notify("run", "live", newer)
	notify("run", "cold", newer)
	if live.Counter() != before {
		t.Error("the placeholder woke a live session")
	}
	if len(respawned) != 0 {
		t.Errorf("the placeholder respawned %v", respawned)
	}
}

// Record-only events wake no one: a VerifyEvent (like a ToolResultEvent)
// leaves a live session's waiter alone and respawns no cold one, while any
// other event wakes the live session.
func TestCommitNotifier_RecordOnlyWakesNoOne(t *testing.T) {
	runReg := NewRunRegistry()
	h := session.NewHandle(func() {})
	if err := runReg.GetOrCreate("run").Register("live", h); err != nil {
		t.Fatal(err)
	}
	var respawned []string
	notify := NewCommitNotifier(runReg, func(_, sid string) { respawned = append(respawned, sid) }, nil, nil)
	before := h.Counter()
	for _, evt := range []event.Event{
		&event.VerifyEvent{Action: event.VerifySet, By: event.ByOperator},
		&event.VerifyEvent{Action: event.VerifyClear, By: "boss"},
		&event.ToolResultEvent{ToolCallID: "t1"},
	} {
		notify("run", "live", evt)
		notify("run", "cold", evt)
	}
	if h.Counter() != before || len(respawned) != 0 {
		t.Errorf("record-only events woke the live session (counter %d→%d) or respawned %v", before, h.Counter(), respawned)
	}
	notify("run", "live", &event.ChildResultEvent{ChildSessionID: "v", Verdict: db.SessionCancelled})
	if h.Counter() == before {
		t.Error("a child_result didn't wake the live session")
	}
}

// A verifier's outcome wakes a live agent (its wait for the verdict). A cold
// one it revives only when that agent crashed with the candidate the verdict
// judges: that turn has the verdict to read. Nothing else: not a stop, not a
// concluded agent, not a verdict on what isn't pending.
func TestCommitNotifier_VerifierOutcomeRevival(t *testing.T) {
	runReg := NewRunRegistry()
	h := session.NewHandle(func() {})
	if err := runReg.GetOrCreate("run").Register("live", h); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{"crashed": db.SessionCrashed, "pending-elsewhere": db.SessionCrashed,
		"concluded": db.SessionConcluded}
	statusOf := func(_, sid string) (string, bool) { s, ok := statuses[sid]; return s, ok }
	awaits := func(_, sid, verifier string) bool { return sid != "pending-elsewhere" && verifier == "v" }
	var respawned []string
	notify := NewCommitNotifier(runReg, func(_, sid string) { respawned = append(respawned, sid) }, statusOf, awaits)
	pass := event.VerifierOutcome("v", "PASS: right", "", time.Time{})
	stop := event.VerifierOutcome("v", "llm call: 404", db.SessionCrashed, time.Time{})
	for _, evt := range []event.Event{pass, stop} {
		before := h.Counter()
		notify("run", "live", evt)
		if h.Counter() == before {
			t.Errorf("%s didn't wake the live agent", evt.ToText())
		}
	}
	for _, sid := range []string{"crashed", "pending-elsewhere", "concluded"} {
		notify("run", sid, pass)
		notify("run", sid, stop)
	}
	notify("run", "crashed", event.VerifierOutcome("other", "PASS: right", "", time.Time{}))
	if !slices.Equal(respawned, []string{"crashed"}) {
		t.Errorf("respawned %v, want only the crashed agent, once: by the verdict on its pending candidate", respawned)
	}
	// Without awaitsVerdict, no outcome record revives anyone.
	NewCommitNotifier(runReg, func(_, sid string) { t.Errorf("respawned %s without awaitsVerdict", sid) }, statusOf, nil)(
		"run", "crashed", pass)
}

// A loop's change is record-only too, so it doesn't wake a live session; a
// tick does.
func TestCommitNotifier_LoopEventDoesNotWakeLiveSession(t *testing.T) {
	runReg := NewRunRegistry()
	handle := session.NewHandle(func() {})
	if err := runReg.GetOrCreate("run").Register("sess", handle); err != nil {
		t.Fatal(err)
	}
	notify := NewCommitNotifier(runReg, func(_, _ string) { t.Error("respawned a live session") }, nil, nil)
	before := handle.Counter()
	notify("run", "sess", &event.LoopEvent{Action: event.LoopSet, ID: "1", By: event.ByOperator})
	if handle.Counter() != before {
		t.Error("a loop event woke the live session")
	}
	notify("run", "sess", &event.UserEvent{Content: "[loop 1 · every 1m · tick 1] p", Loop: &event.LoopTick{ID: "1", N: 1}})
	if handle.Counter() == before {
		t.Error("a tick didn't wake the live session")
	}
}
