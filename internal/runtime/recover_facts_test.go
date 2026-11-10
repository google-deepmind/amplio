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
	"strings"
	"testing"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/proctree"
	"amplio/internal/workspace/plain"
)

// recoverMarker returns the content of the session's RecoverEvent.
func recoverMarker(t *testing.T, store db.Store, runID, sid string) string {
	t.Helper()
	evts, _ := store.GetEvents(context.Background(), runID, sid, db.EventFilter{})
	for _, e := range evts {
		if r, ok := e.Event.(*event.RecoverEvent); ok {
			return r.Content
		}
	}
	t.Fatalf("%s has no RecoverEvent", sid)
	return ""
}

// recoverWith runs RecoverRun over a fresh run whose sessions all need a
// marker, with scan standing in for the process table.
func recoverWith(t *testing.T, scan proctree.Snapshot) (db.Store, string) {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const runID = "run-facts"
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct{ sid, status, parent string }{
		{"boss", db.SessionOngoing, ""},
		{"calm-heron", db.SessionAwaiting, "boss"}, // running sub-agent of boss
		{"done-owl", db.SessionConcluded, "boss"},  // finished sub-agent: not running
		{"solo", db.SessionOngoing, ""},
	} {
		if err := store.CreateSession(ctx, db.SessionRecord{RunID: runID, SessionID: s.sid,
			AgentType: "recover_test_agent", Status: s.status, ParentID: s.parent}); err != nil {
			t.Fatal(err)
		}
		// A tail that isn't at rest, so recovery writes a marker.
		if err := store.AppendEventAtStep(ctx, runID, s.sid, 1, &event.UserEvent{Content: "ctx"}); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil },
		NewRunRegistry(), plain.Factory)
	scans := 0
	mgr.scanProcs = func(string) proctree.Snapshot { scans++; return scan }
	if _, err := mgr.RecoverRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Errorf("process table scanned %d times for one recovery, want 1", scans)
	}
	return store, runID
}

// After a restart, the recovery marker tells each agent what it still has
// running: its background jobs (with the handle that stops them) and its
// running sub-agents, or plainly that nothing survived.
func TestRecoverRun_MarkerSaysWhatIsStillRunning(t *testing.T) {
	bench := &proctree.Process{PID: 48213, PGID: 48213, SessionID: "boss", Elapsed: 2834,
		Cmdline: "./bench.sh --iters 50", Orphan: true,
		Children: []*proctree.Process{{PID: 48214, PGID: 48213, SessionID: "boss", Cmdline: "python bench.py"}}}
	store, runID := recoverWith(t, proctree.Snapshot{Supported: true, Roots: []*proctree.Process{bench}})

	boss := recoverMarker(t, store, runID, "boss")
	for _, want := range []string{
		"[RESUMED at ",
		"Still running from before the restart:",
		"- background: process group 48213, 47 min: ./bench.sh --iters 50 (+1 child process)",
		"- sub-agent: calm-heron (awaiting)",
		"kill -- -<process group>",
	} {
		if !strings.Contains(boss, want) {
			t.Errorf("boss's marker lacks %q:\n%s", want, boss)
		}
	}
	if strings.Contains(boss, "done-owl") {
		t.Errorf("boss's marker lists a finished sub-agent:\n%s", boss)
	}
	for _, sid := range []string{"solo", "calm-heron"} {
		if m := recoverMarker(t, store, runID, sid); !strings.Contains(m, "Nothing you started is still running") {
			t.Errorf("%s's marker = %q, want it to say nothing is running", sid, m)
		}
	}
}

// Without process information (off Linux) the marker says so rather than
// claiming nothing is running.
func TestRecoverRun_MarkerWithoutProcessInfo(t *testing.T) {
	store, runID := recoverWith(t, proctree.Snapshot{Supported: false})
	solo := recoverMarker(t, store, runID, "solo")
	if strings.Contains(solo, "Nothing you started") || !strings.Contains(solo, "background processes: unknown") {
		t.Errorf("marker = %q, want background processes reported as unknown", solo)
	}
	if boss := recoverMarker(t, store, runID, "boss"); !strings.Contains(boss, "- sub-agent: calm-heron (awaiting)") {
		t.Errorf("sub-agents should still be listed without process info:\n%s", boss)
	}
}
