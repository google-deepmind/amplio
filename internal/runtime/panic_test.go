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
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/verify"
	"amplio/internal/workspace/plain"
)

func init() {
	agent.Register("panic_test_agent", func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		return panicAgent{env: env, sid: cfg.SessionID}, nil
	}, agent.Traits{})
}

// panicAgent takes its session up again, as a revived agent does, then panics.
type panicAgent struct {
	env *agent.Env
	sid string
}

func (a panicAgent) Run(ctx context.Context) error {
	if err := a.env.Store.UpdateSessionStatus(ctx, a.env.RunID, a.sid, db.SessionOngoing); err != nil {
		return err
	}
	panic("boom")
}
func (a panicAgent) SessionID() string { return a.sid }

// A revived session whose goroutine panics is crashed, and its parent told,
// as a sub-agent's first life is by the spawn runner: a verifier's outcome
// reaches the agent waiting for it.
func TestRespawnSession_PanicCrashesAndTellsTheParent(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const runID = "run-panic"
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID, Config: config.RunConfig{
		Task: "t", Workspace: ".", LLM: "vertex-claude:run-model"}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []db.SessionRecord{
		{RunID: runID, SessionID: "worker", AgentType: "panic_test_agent", Status: db.SessionAwaiting},
		{RunID: runID, SessionID: "checker", AgentType: "panic_test_agent", Status: db.SessionConcluded,
			ParentID: "worker", Metadata: map[string]any{verify.MetadataVerifierFor: "worker"}},
	} {
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewRunManager(store, specFactory, NewRunRegistry(), plain.Factory)
	mgr.RespawnSession(runID, "checker")
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := store.GetSession(ctx, runID, "checker")
		if err == nil && s.Status == db.SessionCrashed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panicked verifier is %q, want crashed", s.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	events, err := store.GetEvents(ctx, runID, "worker", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var told *event.VerifyOutcome
	for _, e := range events {
		if v, ok := e.Event.(*event.VerifyEvent); ok && v.Action == event.VerifyVerdict {
			told = v.Outcome
		}
	}
	if told == nil || told.Verdict != db.SessionCrashed || told.Findings != "panic: boom" {
		t.Errorf("the agent's outcome record = %+v, want the verifier crashed by the panic", told)
	}
}
