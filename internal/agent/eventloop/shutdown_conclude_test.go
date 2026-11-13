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

package eventloop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/workspace/plain"
)

// cancelAtCheck cancels the session context once FinalizeStep commits, which is
// the window shutdown opens: session.Registry.CancelAll ctx-cancels but leaves
// DB status alone so RecoverRun can resume. The wrapped store is the real one
// and no error is injected; only the timing of the cancel is forced.
type cancelAtCheck struct {
	db.Store
	cancel    context.CancelFunc
	runID     string
	sessionID string
	once      sync.Once
}

func (s *cancelAtCheck) FinalizeStep(ctx context.Context, runID, sessionID string, step int, evts []event.Event) error {
	if err := s.Store.FinalizeStep(ctx, runID, sessionID, step, evts); err != nil {
		return err
	}
	s.once.Do(func() {
		// current_step is already bumped here, so this lands at exactly the step
		// the during-generation check looks at.
		_, _ = s.AppendEvent(context.Background(), s.runID, s.sessionID,
			&event.ChildResultEvent{ChildSessionID: "child-1", Verdict: db.SessionCrashed})
		s.cancel()
	})
	return nil
}

// failCount fails the during-generation count once, with the error shape
// db.Tag produces at the store boundary. Injected, unlike cancelAtCheck: it
// covers recordFailure's db.ErrStore branch, which the shutdown case does not
// reach. The loop's only GetEventCount call site is that check.
type failCount struct {
	db.Store
	once sync.Once
}

func (s *failCount) GetEventCount(ctx context.Context, runID, sessionID string, opts db.EventFilter) (int, error) {
	first := false
	s.once.Do(func() { first = true })
	if first {
		return 0, fmt.Errorf("%w: %w", db.ErrStore, errors.New("transient read failure"))
	}
	return s.Store.GetEventCount(ctx, runID, sessionID, opts)
}

// runTurn runs one bare no-tool turn (the shape that concludes an autonomous
// agent) and reports the session's persisted status.
func runTurn(t *testing.T, ctx context.Context, store db.Store, runID string, reg *session.Registry) string {
	t.Helper()
	ag := newT(testCfg{
		RunID: runID, SessionID: "main-agent", Task: "work",
		SystemPrompt: "You are a helpful agent.",
		LLM: &llm.MockProvider{Model: "test-model",
			Responses: []llm.Response{{Content: "Done for now.", StopReason: "end_turn"}}},
		Store: store, Registry: reg, Tools: []*tool.Tool{}, Workspace: plain.New("/tmp"),
	})
	_ = ag.Run(ctx)
	sess, err := store.GetSession(context.Background(), runID, "main-agent")
	if err != nil {
		t.Fatal(err)
	}
	return sess.Status
}

// A shutdown landing on the during-generation check must not conclude the
// session. Concluding is terminal: db.IsSpine excludes concluded, so RecoverRun
// skips the session and the queued event is lost.
func TestEventLoop_ShutdownAtConclusionCheckLeavesSessionRecoverable(t *testing.T) {
	store, runID, reg := testSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelAtCheck{Store: store, cancel: cancel, runID: runID, sessionID: "main-agent"}

	if got := runTurn(t, ctx, w, runID, reg); got != db.SessionOngoing {
		t.Fatalf("status = %q, want %q: shutdown must leave the session recoverable", got, db.SessionOngoing)
	}
}

// A failed event count is not a count of zero: an unreadable queue must leave
// the session ongoing-but-dead for Recover rather than concluding.
func TestEventLoop_StoreErrorAtConclusionCheckLeavesSessionRecoverable(t *testing.T) {
	store, runID, reg := testSetup(t)
	w := &failCount{Store: store}

	if got := runTurn(t, context.Background(), w, runID, reg); got != db.SessionOngoing {
		t.Fatalf("status = %q, want %q: a store error must not read as an empty queue", got, db.SessionOngoing)
	}
}
