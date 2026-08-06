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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- transition policy (no I/O) ---

func TestMonitorState_Transitions(t *testing.T) {
	m := newMonitorState()
	// First sample of a working run is silent: nothing has changed yet.
	if line := m.observe("r", "ongoing"); line != "" {
		t.Errorf("first observation of an active run printed %q, want silence", line)
	}
	if m.done([]string{"r"}) {
		t.Error("an ongoing run is not done")
	}
	if line := m.observe("r", "ongoing"); line != "" {
		t.Errorf("unchanged status printed %q", line)
	}
	if got, want := m.observe("r", "crashed"), "r\tongoing\tcrashed"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !m.done([]string{"r"}) {
		t.Error("a crashed run should be done")
	}
	// A restart un-finishes it: that is the whole point of evaluating done()
	// over the set at each poll rather than retiring runs one by one.
	if got, want := m.observe("r", "ongoing"), "r\tcrashed\tongoing"; got != want {
		t.Errorf("restart transition = %q, want %q", got, want)
	}
	if m.done([]string{"r"}) {
		t.Error("a restarted run is active again")
	}
	if got, want := m.observe("r", "concluded"), "r\tongoing\tconcluded"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if err := m.exitErr([]string{"r"}); err != nil {
		t.Errorf("a run that ended concluded should exit 0, got %v", err)
	}
}

func TestMonitorState_AlreadyFinishedReportsOnce(t *testing.T) {
	m := newMonitorState()
	if got, want := m.observe("r", "concluded"), "r\t-\tconcluded"; got != want {
		t.Errorf("got %q, want %q — an already-finished run must still report", got, want)
	}
	if line := m.observe("r", "concluded"); line != "" {
		t.Errorf("repeat printed %q", line)
	}
}

func TestMonitorState_ExitCodes(t *testing.T) {
	code := func(final map[string]string) int {
		ids := make([]string, 0, len(final))
		m := newMonitorState()
		for id, st := range final {
			ids = append(ids, id)
			m.observe(id, st)
		}
		err := m.exitErr(ids)
		if err == nil {
			return 0
		}
		var ce *codedError
		if !errors.As(err, &ce) {
			t.Fatalf("want a codedError, got %v", err)
		}
		return ce.code
	}
	tests := []struct {
		name  string
		final map[string]string
		want  int
	}{
		{"all concluded", map[string]string{"a": "concluded", "b": "concluded"}, 0},
		{"one crashed", map[string]string{"a": "concluded", "b": "crashed"}, monitorExitCrashed},
		{"one cancelled", map[string]string{"a": "concluded", "b": "cancelled"}, monitorExitCancelled},
		{"crashed outranks cancelled", map[string]string{"a": "cancelled", "b": "crashed"}, monitorExitCrashed},
		// An id we could not watch outranks everything: the caller must not be
		// told 0 (or even 2) when part of its batch was never observed.
		{"unwatchable outranks crashed", map[string]string{"a": "crashed", "b": statusUnsupported}, 1},
		{"not found", map[string]string{"a": statusNotFound}, 1},
		{"deleted mid-watch is terminal, not an error", map[string]string{"a": statusDeleted}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := code(tc.final); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- run-status extraction ---

func TestPrimaryRootStatus(t *testing.T) {
	detail := func(sessions ...map[string]string) []byte {
		b, err := json.Marshal(map[string]any{"sessions": sessions})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sess := func(id, parent, agentType, status string) map[string]string {
		return map[string]string{"session_id": id, "parent_id": parent, "agent_type": agentType, "status": status}
	}
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"main-agent wins", detail(
			sess("other-root", "", "standard_agent", "concluded"),
			sess("main-agent", "", "standard_agent", "ongoing")), "ongoing"},
		{"children are ignored", detail(
			sess("main-agent", "", "standard_agent", "ongoing"),
			sess("kid", "main-agent", "standard_agent", "crashed")), "ongoing"},
		// A chatbot sidecar parks idle forever; letting it decide would hang the
		// watch on an autonomous run that has actually finished.
		{"chatbot sidecar ignored", detail(
			sess("main-agent", "", "standard_agent", "concluded"),
			sess("chatty-bot", "", "chatbot", "idle")), "concluded"},
		{"interactive-only run is unsupported", detail(
			sess("chatty-bot", "", "chatbot", "idle")), statusUnsupported},
		{"no sessions at all", detail(), statusUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := primaryRootStatus(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- end to end against a scripted server ---

// scriptedServer serves /api/runs/{id} from a per-run queue of statuses; the
// last entry repeats. "404" means the run is gone.
func scriptedServer(t *testing.T, script map[string][]string) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/runs/")
		mu.Lock()
		defer mu.Unlock()
		seq, ok := script[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		i := min(calls[id], len(seq)-1)
		calls[id]++
		status := seq[i]
		if status == "404" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		agentType := "standard_agent"
		if status == "chatbot" {
			agentType, status = "chatbot", "idle"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{
			{"session_id": "main-agent", "parent_id": "", "agent_type": agentType, "status": status},
		}})
	})
}

func runMonitorCmd(t *testing.T, args ...string) (stdout string, exit int) {
	t.Helper()
	cmd := clientMonitorCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		return out.String(), 0
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return out.String(), ce.code
	}
	t.Fatalf("unexpected error: %v (stderr %s)", err, errOut.String())
	return "", 0
}

func TestMonitor_EndToEnd(t *testing.T) {
	scriptedServer(t, map[string][]string{
		"finished": {"concluded"},
		"working":  {"ongoing", "ongoing", "crashed"},
		"restart":  {"ongoing", "crashed", "ongoing", "concluded"},
		"gone":     {"ongoing", "404"},
		"chat":     {"chatbot"},
	})
	stdout, exit := runMonitorCmd(t, "--interval=1ms", "--timeout=30s",
		"finished", "working", "restart", "gone", "chat", "nosuchrun")
	got := strings.Split(strings.TrimSpace(stdout), "\n")
	want := []string{
		"finished\t-\tconcluded",  // already done: reported once, with "-"
		"chat\t-\tunsupported",    // interactive: reported, then skipped
		"nosuchrun\t-\tnot_found", // typo: reported, then skipped
		"working\tongoing\tcrashed",
		"restart\tongoing\tcrashed",
		"gone\tongoing\tdeleted",
		"restart\tcrashed\tongoing", // the restarter got it: back under watch
		"restart\tongoing\tconcluded",
	}
	if !sameSet(got, want) {
		t.Errorf("transitions:\ngot:\n  %s\nwant (any order):\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// unwatchable ids (chat, nosuchrun) outrank the crash in `working`.
	if exit != 1 {
		t.Errorf("exit = %d, want 1", exit)
	}
}

func TestMonitor_ExitsWhenAllCrashed(t *testing.T) {
	scriptedServer(t, map[string][]string{"a": {"ongoing", "crashed"}, "b": {"crashed"}})
	stdout, exit := runMonitorCmd(t, "--interval=1ms", "--timeout=10s", "a", "b")
	if exit != monitorExitCrashed {
		t.Errorf("exit = %d, want %d", exit, monitorExitCrashed)
	}
	if !strings.Contains(stdout, "a\tongoing\tcrashed") || !strings.Contains(stdout, "b\t-\tcrashed") {
		t.Errorf("missing transitions:\n%s", stdout)
	}
}

func TestMonitor_Timeout(t *testing.T) {
	scriptedServer(t, map[string][]string{"a": {"ongoing"}})
	start := time.Now()
	_, exit := runMonitorCmd(t, "--interval=5ms", "--timeout=30ms", "a")
	if exit != monitorExitTimeout {
		t.Errorf("exit = %d, want %d", exit, monitorExitTimeout)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout did not bound the wait")
	}
}

func TestMonitor_FlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--timeout=51h", "a"},
		{"--timeout=0", "a"},
		{"--interval=0", "a"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			scriptedServer(t, map[string][]string{"a": {"concluded"}})
			if _, exit := runMonitorCmd(t, args...); exit != 1 {
				t.Errorf("exit = %d, want 1 for %v", exit, args)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
		if count[s] < 0 {
			return false
		}
	}
	return true
}
