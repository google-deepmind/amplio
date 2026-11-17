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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
)

// captureWarnings sends the default logger to a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// stopped makes a session "worker" with status, holding input (unread unless
// read), and returns the store.
func stopped(t *testing.T, status string, input event.Event, read bool) db.Store {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: "r", SessionID: "worker", AgentType: "standard_agent",
		Status: db.SessionOngoing}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, "r", "worker", input); err != nil {
		t.Fatal(err)
	}
	if read {
		if _, err := store.AdvanceStep(ctx, "r", "worker"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateSessionStatus(ctx, "r", "worker", status); err != nil {
		t.Fatal(err)
	}
	return store
}

func message(senderType string) event.Event {
	return &event.MessageEvent{Content: "one more thing", Sender: "boss", SenderType: senderType}
}

// A concluded or idle session that leaves holding an unread Input is
// reported; one that stopped on purpose (cancelled, crashed), an Input it
// read, an environment notification to a concluded session, one that arrived
// after the slot was released, and a session revived meanwhile aren't.
func TestWarnUndelivered(t *testing.T) {
	agentMsg, envMsg := event.SenderTypeAgent, event.SenderTypeEnvironment
	for _, c := range []struct {
		name       string
		status     string
		input      event.Event
		read       bool
		leftAgo    time.Duration // left = now - leftAgo (an hour: the input came after the release)
		registered bool
		warn       bool
	}{
		{"concluded, a message unread", db.SessionConcluded, message(agentMsg), false, 0, false, true},
		{"idle, a message unread", db.SessionIdle, message(agentMsg), false, 0, false, true},
		{"concluded, a user turn unread", db.SessionConcluded, &event.UserEvent{Content: "and?"}, false, 0, false, true},
		{"idle, a notification unread", db.SessionIdle, message(envMsg), false, 0, false, true},
		{"concluded, a child's result unread", db.SessionConcluded,
			&event.ChildResultEvent{ChildSessionID: "c", Verdict: db.SessionConcluded, Content: "done"}, false, 0, false, true},
		{"concluded, a cancelled child's result unread", db.SessionConcluded,
			&event.ChildResultEvent{ChildSessionID: "c", Verdict: db.SessionCancelled, Content: "parent worker concluded"}, false, 0, false, false},
		{"concluded, a notification unread", db.SessionConcluded, message(envMsg), false, 0, false, false},
		{"cancelled, a message unread", db.SessionCancelled, message(agentMsg), false, 0, false, false},
		{"crashed, a message unread", db.SessionCrashed, message(agentMsg), false, 0, false, false},
		{"concluded, the message read", db.SessionConcluded, message(agentMsg), true, 0, false, false},
		{"concluded, the message after the slot was released", db.SessionConcluded, message(agentMsg), false, time.Hour, false, false},
		{"concluded, revived meanwhile", db.SessionConcluded, message(agentMsg), false, 0, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureWarnings(t)
			store := stopped(t, c.status, c.input, c.read)
			reg := NewRegistry()
			if c.registered {
				if err := reg.Register("worker", NewHandle(func() {})); err != nil {
					t.Fatal(err)
				}
			}
			WarnUndelivered(store, reg, "r", "worker", time.Now().Add(-c.leftAgo))
			got := strings.Contains(logs.String(), "wasn't delivered")
			if got != c.warn {
				t.Errorf("warned %v, want %v; log:\n%s", got, c.warn, logs)
			}
			if got && (!strings.Contains(logs.String(), "session_id=worker") || !strings.Contains(logs.String(), "status="+c.status)) {
				t.Errorf("warning without the session or its status:\n%s", logs)
			}
		})
	}
}
