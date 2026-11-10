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
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/workspace/plain"
)

// A headless interrupt cancels the run, and the process exits a moment later.
// A notification in that moment (from an agent's background job, or a
// harness) is stored, but must not revive the cancelled session: its goroutine
// has exited and its run's registry is gone, so without the close the notifier
// would respawn it into a fresh registry.
func TestCancelAll_NotificationDoesNotReviveSession(t *testing.T) {
	var runs int32
	gate := make(chan struct{})
	close(gate) // park at once
	// Agent types register process-wide, once each: a fresh one per run of the
	// test (-count).
	agentType := fmt.Sprintf("cancel_all_park_agent_%d", time.Now().UnixNano())
	agent.Register(agentType, func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		return &parkAgent{sid: cfg.SessionID, reg: env.Registry, handle: cfg.Handle, gate: gate,
			runs: &runs, awakened: make(chan string, 8)}, nil
	}, agent.Traits{})

	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const runID, sid = "run-interrupted", "main-agent"
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, db.SessionRecord{
		RunID: runID, SessionID: sid, AgentType: agentType, Status: db.SessionAwaiting,
	}); err != nil {
		t.Fatal(err)
	}
	runReg := NewRunRegistry()
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil }, runReg, plain.Factory)
	store.SetCommitListener(NewCommitNotifier(runReg, mgr.RespawnSession, mgr.SessionStatus))

	mgr.RespawnSession(runID, sid)
	waitFor(t, func() bool { return atomic.LoadInt32(&runs) == 1 }, "the session to start")
	runReg.CancelAll()
	waitFor(t, func() bool { return runReg.Get(runID) == nil }, "the cancelled session to exit")

	if _, err := store.AppendEvent(ctx, runID, sid, &event.MessageEvent{
		Content: "time is up", Sender: "harness", SenderType: event.SenderTypeEnvironment,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // time for a (buggy) respawn to start
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Errorf("the notification revived the cancelled session: %d goroutines, want 1", n)
	}
	if runReg.IsRunActive(runID) {
		t.Error("the run is active again after CancelAll")
	}
	events, err := store.GetEvents(ctx, runID, sid, db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("want the notification stored (for a resume): %d events", len(events))
	}
}
