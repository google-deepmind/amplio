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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/tool"
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
		&event.UnknownEvent{Type: "teleport", Raw: []byte(`{"type":"teleport","criteria":"flux tests pass"}`)},
	} {
		if _, err := s.AppendEvent(ctx, runID, "s1", e); err != nil {
			t.Fatal(err)
		}
	}

	got := New(s, runID).ParseAndExecute(ctx, `{"query":"flux"}`).Content
	if !strings.Contains(got, "1 result(s)") || !strings.Contains(got, "type=assistant") || strings.Contains(got, "teleport") {
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

// newStore returns an in-memory store and a run with a session "s1" holding an
// assistant event for each text.
func newStore(tb testing.TB, texts ...string) (db.Store, string) {
	tb.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	ctx := tb.Context()
	runID := db.NewRunID()
	if err := s.CreateRun(ctx, db.RunRecord{RunID: runID, CreatedAt: time.Now().UTC()}); err != nil {
		tb.Fatal(err)
	}
	if err := s.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: "s1", Status: db.SessionOngoing, CreatedAt: time.Now().UTC()}); err != nil {
		tb.Fatal(err)
	}
	for _, text := range texts {
		if _, err := s.AppendEvent(ctx, runID, "s1", &event.AssistantEvent{Content: text}); err != nil {
			tb.Fatal(err)
		}
	}
	return s, runID
}

// searchFor runs session_search with the query and, if not empty, syntax.
func searchFor(t *testing.T, store db.Store, runID, query, syntax string) *tool.Result {
	t.Helper()
	args := map[string]string{"query": query}
	if syntax != "" {
		args["syntax"] = syntax
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, runID).ParseAndExecute(t.Context(), string(b))
}

// The default syntax takes words as written, punctuation and all, where FTS5
// would fail on most of these.
func TestSessionSearch_SimpleSyntax(t *testing.T) {
	store, runID := newStore(t, "wrote final_result.json: accuracy 0.7443", "the sub-agent ran -readonly, not as root", "unrelated chatter")
	for _, q := range []string{"final_result.json", "0.7443", "sub-agent -readonly", `"accuracy 0.7443"`, "NOT root"} {
		got := searchFor(t, store, runID, q, "")
		if got.IsError || !strings.Contains(got.Content, "1 result(s)") {
			t.Errorf("query %q: want one result:\n%s", q, got.Content)
		}
	}
	// A phrase matches only in sequence.
	if got := searchFor(t, store, runID, `"0.7443 accuracy"`, ""); !strings.HasPrefix(got.Content, "No results") {
		t.Errorf("phrase out of order: want no results:\n%s", got.Content)
	}
}

// syntax=fts5 passes the query to FTS5 as written, operators and errors alike.
func TestSessionSearch_FTS5Syntax(t *testing.T) {
	store, runID := newStore(t, "wrote final_result.json", "final chatter")
	got := searchFor(t, store, runID, "fin* NOT chatter", "fts5")
	if got.IsError || !strings.Contains(got.Content, "1 result(s)") || !strings.Contains(got.Content, "final_result.json") {
		t.Errorf("fts5 operators: want the final_result.json event:\n%s", got.Content)
	}
	got = searchFor(t, store, runID, "final_result.json", "fts5")
	if !got.IsError || !strings.Contains(got.Content, "syntax=fts5 passes the query to SQLite FTS5 as written; the default simple syntax takes plain words.") {
		t.Errorf("fts5 syntax error: want an error with the hint:\n%s", got.Content)
	}
	if got := searchFor(t, store, runID, "alpha", "regex"); !got.IsError {
		t.Errorf("unknown syntax: want an error:\n%s", got.Content)
	}
}

// When no event has every term of a simple query, the tool shows the events
// with the most of them, and says so.
func TestSessionSearch_AnyTermFallback(t *testing.T) {
	store, runID := newStore(t, "alpha beta", "alpha gamma delta", "epsilon")
	got := searchFor(t, store, runID, "alpha delta zeta", "").Content
	want := "No event matches all 3 terms of \"alpha delta zeta\"; closest matches (any of the terms, best first):\n"
	deltaFirst := strings.Index(got, "alpha gamma delta") < strings.Index(got, "alpha beta")
	if !strings.HasPrefix(got, want) || strings.Count(got, "type=assistant") != 2 || !deltaFirst {
		t.Errorf("want the two alpha events, the one with delta first, under the label:\n%s", got)
	}
	for _, tc := range []struct{ query, syntax, want string }{
		{"alpha zeta", "simple", "No event matches all 2 terms"}, // The default, named.
		{"zeta omega", "", "No results"},                         // No term matches.
		{"zeta", "", "No results"},                               // One term.
		{"alpha OR beta zeta", "", "No results"},                 // Has OR.
		{"alpha zeta", "fts5", "No results"},                     // Not the simple syntax.
	} {
		if got := searchFor(t, store, runID, tc.query, tc.syntax).Content; !strings.HasPrefix(got, tc.want) {
			t.Errorf("query %q syntax %q: want %q:\n%s", tc.query, tc.syntax, tc.want, got)
		}
	}
}

// A simple query with nothing to search for says so, rather than failing.
func TestSessionSearch_NothingSearchable(t *testing.T) {
	store, runID := newStore(t, "alpha")
	for _, q := range []string{"", "-", "... %", `""`, "OR", `"" OR -`} {
		got := searchFor(t, store, runID, q, "")
		if got.IsError || !strings.HasPrefix(got.Content, "Nothing to search for in ") {
			t.Errorf("query %q: want the nothing-to-search-for message:\n%s", q, got.Content)
		}
	}
}
