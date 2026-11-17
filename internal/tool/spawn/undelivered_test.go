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

package spawn

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
)

// leaving is the spawned child of TestSpawn_WarnsOfAnUndeliveredInput: it
// concludes, then holds its slot until leave is closed.
var leaving struct {
	sid              string
	concluded, leave chan struct{}
}

func init() {
	agent.Register("spawn_leaving_test_agent", func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		return &leavingChild{env: env, id: cfg.SessionID}, nil
	}, agent.Traits{})
}

type leavingChild struct {
	env *agent.Env
	id  string
}

func (a *leavingChild) SessionID() string { return a.id }

func (a *leavingChild) Run(ctx context.Context) error {
	// A real agent creates its session record as it starts.
	if err := a.env.Store.CreateSession(ctx, db.SessionRecord{RunID: a.env.RunID, SessionID: a.id,
		ParentID: "parent", AgentType: "spawn_leaving_test_agent", Status: db.SessionOngoing}); err != nil {
		return err
	}
	if _, err := a.env.Store.AdvanceStep(ctx, a.env.RunID, a.id); err != nil {
		return err
	}
	if err := a.env.Store.UpdateSessionStatus(ctx, a.env.RunID, a.id, db.SessionConcluded); err != nil {
		return err
	}
	leaving.sid = a.id
	close(leaving.concluded)
	<-leaving.leave
	return nil
}

// A spawned sub-agent that leaves holding a message that reached it as it
// concluded says so, as the run manager's sessions do.
func TestSpawn_WarnsOfAnUndeliveredInput(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	logged := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	env := modelEnv(t)
	leaving.concluded, leaving.leave = make(chan struct{}), make(chan struct{})
	args, _ := json.Marshal(Params{Task: "t", AgentType: "spawn_leaving_test_agent"})
	if res, err := makeExecutor(env, "parent")(context.Background(), args); err != nil || res.IsError {
		t.Fatalf("spawn: %v %+v", err, res)
	}
	select {
	case <-leaving.concluded:
	case <-time.After(5 * time.Second):
		t.Fatalf("the child never concluded; log:\n%s", logged())
	}
	if _, err := env.Store.AppendEvent(context.Background(), env.RunID, leaving.sid, &event.MessageEvent{
		Content: "one more thing", Sender: "parent", SenderType: event.SenderTypeAgent}); err != nil {
		t.Fatal(err)
	}
	close(leaving.leave)
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logged(), "wasn't delivered"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no warning for the undelivered message; log:\n%s", logged())
		}
	}
	if !strings.Contains(logged(), "session_id="+leaving.sid) {
		t.Errorf("warning without the child's session id:\n%s", logged())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
