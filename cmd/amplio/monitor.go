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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/db"

	"github.com/spf13/cobra"
)

// monitor exit codes. 1 (usage) outranks the rest on purpose: a run that could
// not be watched means the caller did not get what it asked for, and it must not
// learn that from a 0. The crashes are reported line-by-line as they happen, so
// reaction logic does not depend on the exit code to see them.
const (
	monitorExitCrashed   = 2
	monitorExitCancelled = 3
	monitorExitTimeout   = 124

	// A monitor is a background process nobody is watching. Bounding it keeps a
	// forgotten watcher from outliving the work by days.
	maxMonitorTimeout = 50 * time.Hour

	// Synthetic statuses: not session states, but outcomes a caller must see.
	statusUnsupported = "unsupported"
	statusNotFound    = "not_found"
	statusDeleted     = "deleted"
	statusUnknown     = "-"
)

func clientMonitorCmd() *cobra.Command {
	var (
		interval time.Duration
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "monitor <run-id>...",
		Short: "Watch runs until they finish, reporting each status change",
		Long: "Poll the named runs and print every status change as it is observed," +
			" then exit once all of them are finished at the same poll. Built for an" +
			" unattended watcher: launch it in the background, react to the lines it" +
			" streams, and let its exit code summarize the batch.\n\n" +
			"Output is one tab-separated line per change: <run_id> <from> <to>. A run" +
			" that is already finished when monitoring starts reports once with '-' as" +
			" its previous status; runs that are still working start silent. Statuses" +
			" are SAMPLED every --interval, so a change that starts and ends inside one" +
			" interval is never seen.\n\n" +
			"A restarted run is picked back up automatically: 'crashed' is terminal for" +
			" the run, not for the watch, so crashed -> ongoing -> concluded all report" +
			" as long as the batch has not already finished.\n\n" +
			"Interactive (chatbot) runs are not supported — they park waiting for a" +
			" human, so there is nothing to wait for. Such an id, or one that does not" +
			" exist, reports once and is skipped; the others are still watched, and the" +
			" exit code becomes 1 so no script believes it watched them all.\n\n" +
			"Exit: 0 all concluded · 2 any crashed · 3 any cancelled · 124 timeout ·" +
			" 1 nothing watchable / bad id / usage.",
		Example: "  amplio client monitor $(cat group-a.ids) > group-a.tsv\n" +
			"  amplio client monitor $IDS | while IFS=$'\\t' read -r run from to; do\n" +
			"    [ \"$to\" = crashed ] && ( sleep 3600; amplio client restart \"$run\" ) &\n" +
			"  done",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval <= 0 {
				return &codedError{1, "--interval must be positive"}
			}
			if timeout <= 0 || timeout > maxMonitorTimeout {
				return &codedError{1, fmt.Sprintf("--timeout must be between 0 and %s", maxMonitorTimeout)}
			}
			return runMonitor(cmd, dedupe(args), interval, timeout)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Minute, "How often to sample run status")
	cmd.Flags().DurationVar(&timeout, "timeout", 12*time.Hour, "Give up after this long (max 50h)")
	return cmd
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// monitorState tracks what has been reported for each run, so a poll emits a
// line only when the answer changed. It holds no I/O: the polling loop feeds it
// observations, which makes the whole transition/exit policy unit-testable.
type monitorState struct {
	last     map[string]string // last status REPORTED (not merely observed)
	seen     map[string]bool
	terminal map[string]bool // currently in a terminal state (can flip back on restart)
	bad      map[string]bool // unwatchable: interactive, unknown id
}

func newMonitorState() *monitorState {
	return &monitorState{
		last: map[string]string{}, seen: map[string]bool{},
		terminal: map[string]bool{}, bad: map[string]bool{},
	}
}

// observe records one sample and returns the line to print, or "" for silence.
// The first sample of a run is silent unless it is already an answer (terminal
// or unwatchable): there is no transition to report, and a batch of thirty runs
// should not open with thirty lines saying "ongoing".
func (m *monitorState) observe(runID, status string) string {
	first := !m.seen[runID]
	m.seen[runID] = true
	m.terminal[runID] = db.SessionTerminalStatuses[status] || status == statusDeleted
	if status == statusUnsupported || status == statusNotFound {
		m.bad[runID] = true
	}
	if first {
		m.last[runID] = status
		if m.terminal[runID] || m.bad[runID] {
			return fmt.Sprintf("%s\t%s\t%s", runID, statusUnknown, status)
		}
		return ""
	}
	if m.last[runID] == status {
		return ""
	}
	from := m.last[runID]
	m.last[runID] = status
	return fmt.Sprintf("%s\t%s\t%s", runID, from, status)
}

// done reports whether every run is finished AT THIS MOMENT. Evaluated over the
// whole set rather than per run, which is what lets a restarted run rejoin the
// watch: its crash does not retire it while the others are still going.
func (m *monitorState) done(ids []string) bool {
	for _, id := range ids {
		if !m.terminal[id] && !m.bad[id] {
			return false
		}
	}
	return true
}

func (m *monitorState) active(ids []string) []string {
	var out []string
	for _, id := range ids {
		if !m.terminal[id] && !m.bad[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// exitErr turns the final state into the process exit code.
func (m *monitorState) exitErr(ids []string) error {
	var crashed, cancelled, unwatchable int
	for _, id := range ids {
		switch {
		case m.bad[id]:
			unwatchable++
		case m.last[id] == db.SessionCrashed:
			crashed++
		case m.last[id] == db.SessionCancelled:
			cancelled++
		}
	}
	switch {
	case unwatchable > 0:
		return &codedError{1, fmt.Sprintf("%d of %d run(s) could not be watched", unwatchable, len(ids))}
	case crashed > 0:
		return &codedError{monitorExitCrashed, fmt.Sprintf("%d of %d run(s) crashed", crashed, len(ids))}
	case cancelled > 0:
		return &codedError{monitorExitCancelled, fmt.Sprintf("%d of %d run(s) cancelled", cancelled, len(ids))}
	}
	return nil
}

func runMonitor(cmd *cobra.Command, ids []string, interval, timeout time.Duration) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "monitoring %d run(s) every %s (timeout %s)\n", len(ids), interval, timeout)

	state := newMonitorState()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		for _, id := range ids {
			if state.bad[id] {
				continue // unwatchable: reported once, never polled again
			}
			status, err := monitorPoll(cmd.Context(), id, state.seen[id])
			if err != nil {
				// A server that restarts overnight must not kill the watcher:
				// warn and try again next tick.
				fmt.Fprintf(errOut, "poll %s: %v (retrying)\n", id, err)
				continue
			}
			if line := state.observe(id, status); line != "" {
				fmt.Fprintln(out, line)
			}
		}
		if state.done(ids) {
			return state.exitErr(ids)
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			fmt.Fprintf(errOut, "timeout after %s; still active: %s\n", timeout, strings.Join(state.active(ids), " "))
			return &codedError{monitorExitTimeout, fmt.Sprintf("timed out after %s with %d run(s) still active", timeout, len(state.active(ids)))}
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-ticker.C:
		}
	}
}

// monitorPoll returns the run's current status, or one of the synthetic ones.
// seen distinguishes "this id never existed" (a caller typo, worth an exit code)
// from "the run was deleted while we watched" (an end state, not an error).
func monitorPoll(ctx context.Context, runID string, seen bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, resp, err := clientRequest(ctx, http.MethodGet, "/api/runs/"+runID, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()                                 //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		if seen {
			return statusDeleted, nil
		}
		return statusNotFound, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("server returned %s", resp.Status)
	}
	return primaryRootStatus(body)
}

// primaryRootStatus picks the status that represents the run: its autonomous
// spine. Interactive roots are skipped — a chatbot sidecar attached to an
// autonomous run must not hold the watch open, and a run that is ONLY
// interactive has nothing to wait for at all.
func primaryRootStatus(detail []byte) (string, error) {
	var d struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			ParentID  string `json:"parent_id"`
			AgentType string `json:"agent_type"`
			Status    string `json:"status"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(detail, &d); err != nil {
		return "", fmt.Errorf("parse run detail: %w", err)
	}
	var fallback string
	found := false
	for _, s := range d.Sessions {
		if s.ParentID != "" || agent.IsInteractive(s.AgentType) {
			continue
		}
		if s.SessionID == config.RootAgentSessionID {
			return s.Status, nil
		}
		if !found {
			fallback, found = s.Status, true
		}
	}
	if !found {
		return statusUnsupported, nil
	}
	return fallback, nil
}
