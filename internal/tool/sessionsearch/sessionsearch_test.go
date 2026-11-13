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

package sessionsearch

import (
	"context"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
)

// The full-text index covers a newer binary's events too (it indexes the raw
// JSON); the tool shows only the events this binary knows.
func TestSessionSearch_SkipsUnknownEvents(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	runID := db.NewRunID()
	_ = s.CreateRun(ctx, db.RunRecord{RunID: runID, CreatedAt: time.Now().UTC()})
	_ = s.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "s1", Status: db.SessionOngoing, CreatedAt: time.Now().UTC()})
	for _, e := range []event.Event{
		&event.AssistantEvent{Content: "the flux tests pass"},
		&event.UnknownEvent{Type: "verify", Raw: []byte(`{"type":"verify","criteria":"flux tests pass"}`)},
	} {
		if _, err := s.AppendEvent(ctx, runID, "s1", e); err != nil {
			t.Fatal(err)
		}
	}

	got := New(s, runID).ParseAndExecute(ctx, `{"query":"flux"}`).Content
	if !strings.Contains(got, "1 result(s)") || !strings.Contains(got, "type=assistant") || strings.Contains(got, "verify") {
		t.Errorf("want only the assistant event:\n%s", got)
	}
}

// A newer binary's events that outrank the known matches don't take their
// places: the tool reads on until it has limit known ones, and no more.
func TestSessionSearch_UnknownEventsDontCrowdOutKnownOnes(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	runID := db.NewRunID()
	_ = s.CreateRun(ctx, db.RunRecord{RunID: runID, CreatedAt: time.Now().UTC()})
	_ = s.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "s1", Status: db.SessionOngoing, CreatedAt: time.Now().UTC()})
	for range 9 {
		if _, err := s.AppendEvent(ctx, runID, "s1", &event.UnknownEvent{Type: "teleport",
			Raw: []byte(`{"type":"teleport","note":"flux flux flux flux"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{"one flux here", "two: flux again", "three, flux at last"} {
		if _, err := s.AppendEvent(ctx, runID, "s1", &event.AssistantEvent{Content: text}); err != nil {
			t.Fatal(err)
		}
	}

	got := New(s, runID).ParseAndExecute(ctx, `{"query":"flux","limit":2}`).Content
	if !strings.Contains(got, "2 result(s)") || strings.Count(got, "type=assistant") != 2 || strings.Contains(got, "teleport") {
		t.Errorf("limit 2: want two assistant events:\n%s", got)
	}
	got = New(s, runID).ParseAndExecute(ctx, `{"query":"flux","limit":5}`).Content
	if !strings.Contains(got, "3 result(s)") || strings.Count(got, "type=assistant") != 3 {
		t.Errorf("limit 5: want all three assistant events:\n%s", got)
	}
}
