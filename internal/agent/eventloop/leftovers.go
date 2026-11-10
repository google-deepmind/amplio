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
	"fmt"
	"strings"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/proctree"
)

// End-of-run reminder. Concluding is final for an autonomous agent: its running
// sub-agents are cancelled, and notifications from its background processes
// reach no one afterwards (the environment never wakes a concluded agent). So
// when an agent is about to conclude with work still running, it gets one
// reminder instead, listing what is running and what concluding will do, and
// decides: wait, stop it, or conclude anyway. Facts only, and bounded: the
// attempt right after a reminder always concludes. Interactive agents never
// get here; they go idle, and notifications wake them.

// reminderPrefix opens the reminder. justReminded finds reminders by their
// Check; the prefix identifies those written before Check existed.
const reminderPrefix = "[system] Before you conclude, these are still running:"

// conclusionCheck runs the checks for an autonomous agent about to conclude,
// given the context its final turn answered. It returns the message that holds
// the conclusion for a turn, or nil to let it through. One check today: work
// still running (the end-of-run reminder). Further checks (e.g. verification)
// go here, in order.
func (a *EventLoopAgent) conclusionCheck(ctx context.Context, events []db.EventRecord) *event.UserEvent {
	if !justReminded(events) {
		if w := a.runningWork(ctx); !w.empty() {
			return &event.UserEvent{Content: w.reminder(), Check: event.CheckLeftovers}
		}
	}
	return nil
}

// scanProcs is the run's process table; a var so tests can stand one in.
var scanProcs = proctree.Scan

// maxReminderJobs bounds the background jobs a reminder lists.
const maxReminderJobs = 10

// runningWork is what a session still has running.
type runningWork struct {
	jobs      []proctree.Job // its background processes (top-most of each tree)
	subAgents []string       // its running sub-agents
}

func (w runningWork) empty() bool { return len(w.jobs) == 0 && len(w.subAgents) == 0 }

func (a *EventLoopAgent) runningWork(ctx context.Context) runningWork {
	var w runningWork
	if snap := scanProcs(a.env.RunID); snap.Supported {
		w.jobs = proctree.JobsOf(snap.Roots, a.cfg.SessionID)
	}
	children, err := a.env.Store.GetChildSessions(ctx, a.env.RunID, a.cfg.SessionID)
	if err != nil {
		a.logger.Warn("list sub-agents for the end-of-run reminder", "error", err)
	}
	for _, c := range children {
		if c.Status == db.SessionOngoing || c.Status == db.SessionAwaiting {
			w.subAgents = append(w.subAgents, c.SessionID)
		}
	}
	return w
}

// reminder is the message that holds a conclusion for one turn.
func (w runningWork) reminder() string {
	var b strings.Builder
	b.WriteString(reminderPrefix + "\n")
	for i, j := range w.jobs {
		if i == maxReminderJobs {
			fmt.Fprintf(&b, "- and %d more background jobs\n", len(w.jobs)-i)
			break
		}
		b.WriteString("- background process you started: " + j.Describe() + "\n")
	}
	if len(w.subAgents) > 0 {
		b.WriteString("- sub-agents: " + strings.Join(w.subAgents, ", ") + "\n")
	}
	// Only what applies: a reminder about sub-agents alone shouldn't talk about
	// background processes, nor one about processes alone about session_cancel.
	jobs, subs := len(w.jobs) > 0, len(w.subAgents) > 0
	var ends, stop []string
	if subs {
		ends = append(ends, "running sub-agents are cancelled")
	}
	if jobs {
		ends = append(ends, "notifications from background processes will no longer reach you")
		stop = append(stop, "kill -- -<process group>")
	}
	if subs {
		stop = append(stop, "session_cancel")
	}
	b.WriteString("Concluding ends this session: " + strings.Join(ends, ", and ") + ".")
	if jobs {
		b.WriteString(" Background processes are not stopped.")
	}
	b.WriteString("\nIf you need their results, wait with await_event. If you don't, stop them (" +
		strings.Join(stop, ", ") + ")")
	if jobs {
		b.WriteString(", or leave them running on purpose and just conclude")
	}
	b.WriteString(".\n")
	b.WriteString("When you conclude, your final message is your result: restate your full " +
		"conclusion there, not only a reply to this note.")
	return b.String()
}

// notes are what the parent's child_result says this session left running:
// its background processes. Its sub-agents are cancelled by the conclusion
// itself, so they aren't left running.
func (w runningWork) notes() []event.ResultNote {
	switch n := len(w.jobs); n {
	case 0:
		return nil
	case 1:
		return []event.ResultNote{{Kind: event.NoteLeftover, Text: "1 background process group"}}
	default:
		return []event.ResultNote{{Kind: event.NoteLeftover, Text: fmt.Sprintf("%d background process groups", n)}}
	}
}

// justReminded reports whether a reminder came after the agent's last turn,
// i.e. this conclusion attempt answers one, so it concludes whatever is still
// running. The scan stops at the agent's previous turn, so a reminder from an
// earlier round (before a conclusion, or before tool calls) never counts.
// Anything that arrived after the reminder (a notification, an operator or
// parent message) doesn't re-arm it: this turn was produced with the reminder
// in its context, so the agent concluded knowing what is still running.
func justReminded(events []db.EventRecord) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch e := events[i].Event.(type) {
		case *event.AssistantEvent:
			return false
		case *event.UserEvent:
			if e.Check == event.CheckLeftovers || strings.HasPrefix(e.Content, reminderPrefix) {
				return true
			}
		}
	}
	return false
}
