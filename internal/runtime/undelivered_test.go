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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/workspace/plain"
)

// leavingAgent concludes the way a session does (it reads its events and
// writes its final status), then holds its slot until leave is closed: the
// window in which the notifier finds it still registered, and only wakes the
// leaving goroutine.
type leavingAgent struct {
	env  *agent.Env
	sid  string
	gate *leaveGate
}

type leaveGate struct {
	concluded chan struct{}
	leave     chan struct{}
}

var leaveGates sync.Map // run ID -> *leaveGate

const leavingAgentType = "undelivered_leaving_agent"

func init() {
	agent.Register(leavingAgentType, func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		g, _ := leaveGates.Load(env.RunID)
		return &leavingAgent{env: env, sid: cfg.SessionID, gate: g.(*leaveGate)}, nil
	}, agent.Traits{})
}

func (a *leavingAgent) SessionID() string { return a.sid }

func (a *leavingAgent) Run(ctx context.Context) error {
	if _, err := a.env.Store.AdvanceStep(ctx, a.env.RunID, a.sid); err != nil {
		return err
	}
	if err := a.env.Store.UpdateSessionStatus(ctx, a.env.RunID, a.sid, db.SessionConcluded); err != nil {
		return err
	}
	close(a.gate.concluded)
	<-a.gate.leave
	return nil
}

// A message that reaches a session between its conclusion and the release of
// its slot isn't delivered (the notifier wakes the leaving goroutine), and the
// session goroutine says so as it leaves.
func TestSessionGoroutine_WarnsOfAnUndeliveredInput(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	runID := fmt.Sprintf("run-undelivered-%d", time.Now().UnixNano())
	gate := &leaveGate{concluded: make(chan struct{}), leave: make(chan struct{})}
	leaveGates.Store(runID, gate)
	t.Cleanup(func() { leaveGates.Delete(runID) })
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "worker",
		AgentType: leavingAgentType, Status: db.SessionAwaiting}); err != nil {
		t.Fatal(err)
	}
	runReg := NewRunRegistry()
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil }, runReg, plain.Factory)
	store.SetCommitListener(NewCommitNotifier(runReg, mgr.RespawnSession, mgr.SessionStatus))
	mgr.RespawnSession(runID, "worker")

	<-gate.concluded
	if _, err := store.AppendEvent(ctx, runID, "worker", &event.MessageEvent{
		Content: "one more thing", Sender: "boss", SenderType: event.SenderTypeAgent}); err != nil {
		t.Fatal(err)
	}
	close(gate.leave)
	deadline := time.Now().Add(5 * time.Second)
	for runReg.Get(runID) != nil { // the goroutine's last step drops the run's registry
		if time.Now().After(deadline) {
			t.Fatal("the session goroutine never left")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "wasn't delivered") || !strings.Contains(buf.String(), "session_id=worker") {
		t.Errorf("no warning for the undelivered message; log:\n%s", buf.String())
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
