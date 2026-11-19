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

	"amplio/internal/db"
	"amplio/internal/event"
)

// Requiring conclude ([agent] require_conclude). By default, a reply without
// tool calls from an autonomous agent is an attempt to conclude with its text
// (plainReply). With the setting on, only the conclude tool concludes: a reply
// gets a note asking for conclude, and the maxReplies-th reply in a row, with
// no tool call between them, fails the session; the note before it says so.
// An empty or output-limited reply keeps its own nudges.

// maxReplies is how many replies in a row without a tool call fail an agent
// that must conclude with the tool.
const maxReplies = 5

var errKeptReplying = errors.New("kept replying without calling conclude")

const replyNote = "A reply without tool calls doesn't finish your task: only conclude does. When you are done, " +
	"call conclude(result) with your final result; until then, keep working with your tools."

// requiresConclude reports whether only the conclude tool concludes this
// agent: the server requires it, and this is an autonomous agent that
// concludes with a result. A verifier's replies have rules of their own (they
// are never a verdict), a chatbot never concludes, and an agent without the
// tool has no other way to.
func (a *EventLoopAgent) requiresConclude() bool {
	if !a.env.RequireConclude || a.cfg.Interactive || a.givesVerdict() {
		return false
	}
	for _, t := range a.cfg.Tools {
		if t.Name == event.ConcludeTool {
			return true
		}
	}
	return false
}

// answerReply answers a reply without tool calls from an agent that must
// conclude with the tool (events is the context its turn answered): a note,
// or, for the maxReplies-th reply in a row, the error that fails the session.
func (a *EventLoopAgent) answerReply(ctx context.Context, events []db.EventRecord) error {
	n := repliesInARow(events) + 1 // with this one
	if n >= maxReplies {
		return fmt.Errorf("%w: %d replies in a row with no tool call ([agent] require_conclude)", errKeptReplying, n)
	}
	text := replyNote
	if n == maxReplies-1 {
		text += fmt.Sprintf(" This is reply %d in a row without a tool call: if your next turn is one too, "+
			"this session fails.", n)
	}
	_, err := a.env.Store.AppendEvent(ctx, a.env.RunID, a.cfg.SessionID,
		&event.UserEvent{Content: harness(text), Check: event.CheckReply})
	return err
}

// repliesInARow counts the replies already answered with a note (answerReply)
// since the agent's latest turn with tool calls, in events.
func repliesInARow(events []db.EventRecord) int {
	n := 0
	for i := len(events) - 1; i >= 0; i-- {
		switch e := events[i].Event.(type) {
		case *event.AssistantEvent:
			if len(e.ToolCalls) > 0 {
				return n
			}
		case *event.UserEvent:
			if e.Check == event.CheckReply {
				n++
			}
		}
	}
	return n
}
