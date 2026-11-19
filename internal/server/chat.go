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

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/toolsummary"
	"amplio/internal/verify"
)

// chatBubble is a projected chat message. Tool-result bodies never cross the
// wire (the chat view doesn't render them); instead each tool call carries a
// server-computed completion flag.
type chatBubble struct {
	EventID  string `json:"event_id"`
	Kind     string `json:"kind"` // "operator" | "chatbot" | "agent" | "environment" | "child_result" | "verdict"
	Content  string `json:"content"`
	Thoughts string `json:"thoughts,omitempty"`
	From     string `json:"from,omitempty"`
	// Verdict is set for kind=="child_result": the sub-agent's terminal status
	// (concluded | crashed | cancelled), so the client can badge it; and for
	// kind=="verdict": PASS | FAIL, or the status of a verifier that stopped.
	Verdict string `json:"verdict,omitempty"`
	// Notes, also only for child_result: facts about how the sub-agent concluded
	// (e.g. what it left running), each with a label to show.
	Notes     []chatNote     `json:"notes,omitempty"`
	Step      int            `json:"step"`
	CreatedAt time.Time      `json:"created_at"`
	ToolCalls []chatToolCall `json:"tool_calls"`
	// Rewrite is a plain-prose restatement of Content, when one exists (see
	// internal/responserewrite). Sent alongside the original, never instead of
	// it: the client offers both and the original stays the record.
	Rewrite string `json:"rewrite,omitempty"`
	// Refusal is set when the provider declined this turn (see llm.Refusal), so
	// the client can say why instead of showing a blank or missing reply.
	Refusal *chatRefusal `json:"refusal,omitempty"`
	// StopNotice is set when the turn stopped abnormally (see stopNoticeFor).
	StopNotice *stopNotice `json:"stop_notice,omitempty"`
	// Loop is set on a loop's tick (an operator bubble): which loop, which tick.
	Loop *event.LoopTick `json:"loop,omitempty"`
	// Outcome is set for kind=="verdict": what the session's verifier said,
	// as the harness told the session (From is the verifier, Content its
	// findings).
	Outcome *event.VerifyOutcome `json:"outcome,omitempty"`
	// Stale is set for kind=="verdict" when that verdict was on a candidate a
	// newer version (or a clear) withdrew: told as information, and stale.
	Stale bool `json:"stale,omitempty"`
}

// chatRefusal is the display part of event.Refusal: the provider's category
// and explanation, verbatim (the native payload stays in the event log). Both
// are always sent; "" means the provider gave none.
type chatRefusal struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

type chatToolCall struct {
	ID        string `json:"id"` // tool-call id; lets the UI fetch the call+result detail on demand
	Name      string `json:"name"`
	Verb      string `json:"verb,omitempty"` // action label for bash (e.g. "search"); empty for other tools
	Completed bool   `json:"completed"`
	Errored   bool   `json:"errored,omitempty"` // tool reported IsError; chip renders red
	Detail    string `json:"detail,omitempty"`  // short arg summary / target for the chip (e.g. a filename)
}

// phaseCard is a closed (summarized) phase, rendered collapsed above the live
// bubbles so long chats stay bounded.
type phaseCard struct {
	StartStep int    `json:"start_step"`
	EndStep   int    `json:"end_step"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
}

// chatUsage is the latest assistant turn's token usage, for the status bar.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatFeed struct {
	Messages   []chatBubble `json:"messages"`
	PhaseCards []phaseCard  `json:"phase_cards"`
	Usage      *chatUsage   `json:"usage"`
}

// handleChat projects a session into the chat view: rolled-up phase cards plus
// the live conversation bubbles (operator + chatbot, plus inbound
// agent/environment messages), excluding tool-result bodies and other
// non-conversational events.
//
// Two modes:
//
//   - LIVE (no range params): everything at or below the phase boundary is
//     rolled into cards and omitted from the bubbles, so a long chat stays
//     bounded. This is what the chat page renders.
//   - RANGED (from_step/to_step): the read-only session-log viewer browsing ONE
//     phase. No rollup, no cards (the client already has the phase index from
//     the trajectory endpoint), and no usage — a historical slice has no
//     "latest turn".
//
// The projection itself is identical in both modes and works for any session,
// not just a chatbot: an autonomous agent's turns render as the same bubbles.
// Tool results are pinned to their call's step (see docs/internals/step_model.md), so a
// range never splits a call from its result.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")

	from, to := stepRange(r)
	ranged := from != nil || to != nil

	boundary := -1 // ranged: nothing is rolled up, so nothing is skipped below
	cards := []phaseCard{}
	if !ranged {
		var c []phaseCard
		boundary, c = s.phaseState(r.Context(), id, sid)
		if c != nil {
			cards = c
		}
	}
	recs, err := s.store.GetEvents(r.Context(), id, sid, db.EventFilter{StartStep: from, EndStep: to})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// One read for the whole feed. A failure here is not fatal: the rewrites are
	// an optional overlay, and a chat that renders without them is still correct.
	rewrites, err := s.store.ListResponseRewrites(r.Context(), id, sid)
	if err != nil {
		slog.Warn("chat: response rewrites unavailable", "run_id", id, "session_id", sid, "error", err)
		rewrites = nil
	}

	// A tool call is "done" once any tool_result for its id exists in the session;
	// errored if that result carried the tool's IsError flag.
	completed := make(map[string]bool)
	errored := make(map[string]bool)
	for _, rec := range recs {
		if tr, ok := rec.Event.(*event.ToolResultEvent); ok {
			completed[tr.ToolCallID] = true
			if tr.IsError {
				errored[tr.ToolCallID] = true
			}
		}
	}

	stale := &staleVerdicts{store: s.store, runID: id, sessionID: sid}
	msgs := make([]chatBubble, 0, len(recs))
	for _, rec := range recs {
		if rec.Step <= boundary {
			continue // rolled up into a phase card
		}
		switch ev := rec.Event.(type) {
		case *event.UserEvent:
			if ev.Outcome != nil {
				msgs = append(msgs, stale.bubble(r.Context(), rec, ev.Outcome))
				continue
			}
			// The harness writes user-role nudges too (conclusion checks such as
			// the end-of-run reminder): shown as amplio's, not as the operator's.
			kind, from := "operator", "user"
			switch {
			case ev.IsHarness():
				kind, from = "environment", "amplio"
			case ev.Loop != nil:
				from = "loop " + ev.Loop.ID // still the operator's: their prompt, re-sent
			}
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: kind, Content: ev.Content,
				From: from, Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: []chatToolCall{}, Loop: ev.Loop,
			})
		case *event.AssistantEvent:
			// An empty turn has nothing to show — unless it was refused or
			// stopped abnormally, which the operator must see (both usually leave
			// exactly this empty turn).
			notice := stopNoticeFor(ev)
			if ev.Content == "" && len(ev.ToolCalls) == 0 && ev.Refusal == nil && notice == nil {
				continue
			}
			var refusal *chatRefusal
			if ev.Refusal != nil {
				refusal = &chatRefusal{Category: ev.Refusal.Category, Explanation: ev.Refusal.Explanation}
			}
			tcs := make([]chatToolCall, 0, len(ev.ToolCalls))
			for _, tc := range ev.ToolCalls {
				verb, detail := toolsummary.ToolChip(tc.Name, tc.Arguments)
				tcs = append(tcs, chatToolCall{
					ID: tc.ID, Name: tc.Name, Verb: verb, Completed: completed[tc.ID], Errored: errored[tc.ID], Detail: detail,
				})
			}
			// A rewrite exists only for conclusions, and only for a session whose
			// model opted in; absent is the normal case and means "show the
			// original, offer no toggle".
			rewrite := rewrites[rec.Step].Text
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: "chatbot", Content: withConclusion(ev),
				Thoughts: ev.Thoughts, Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: tcs, Rewrite: rewrite, Refusal: refusal, StopNotice: notice,
			})
		case *event.MessageEvent:
			// Inbound message: another agent (send_message) or the environment
			// (amplio notify). Environment bodies are typically shell output, so
			// the client renders them as <pre> rather than markdown.
			kind := "agent"
			if ev.SenderType == event.SenderTypeEnvironment {
				kind = "environment"
			}
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: kind, Content: ev.Content,
				From: ev.Sender, Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: []chatToolCall{},
			})
		case *event.ChildResultEvent:
			// A spawned sub-agent finished: its terminal result posted back to this
			// (parent) session. Surface it with the verdict so the client can badge
			// concluded vs crashed/cancelled distinctly.
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: "child_result", Content: ev.Content,
				From: ev.ChildSessionID, Verdict: ev.Verdict, Notes: chatNotes(ev.AllNotes()),
				Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: []chatToolCall{},
			})
		case *event.ToolResultEvent:
			// Tool results stay out of the chat, but a conclude call's result
			// that tells what the verifier said is the verdict the session got.
			if ev.Outcome != nil {
				msgs = append(msgs, stale.bubble(r.Context(), rec, ev.Outcome))
			}
		case *event.LoopEvent:
			// A loop was set, changed or ended: amplio's note, not a turn.
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: "environment", Content: s.loopEventText(ev),
				From: "amplio", Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: []chatToolCall{},
			})
		case *event.VerifyEvent:
			// The harness's own actions come with its check messages; the
			// operator's or parent's changes have only this event.
			if text := verifyEventText(ev); text != "" {
				msgs = append(msgs, chatBubble{
					EventID: rec.EventID, Kind: "environment", Content: text,
					From: "amplio", Step: rec.Step, CreatedAt: rec.CreatedAt,
					ToolCalls: []chatToolCall{},
				})
			}
		case *event.CompactionEvent:
			// Context compaction: the prior turns were rolled into a summary at
			// this boundary. Surface it as a distinct seam (rendered as a divider
			// the user can expand), not a chat bubble — it's meta, not a turn.
			msgs = append(msgs, chatBubble{
				EventID: rec.EventID, Kind: "compaction", Content: ev.Content,
				Step: rec.Step, CreatedAt: rec.CreatedAt,
				ToolCalls: []chatToolCall{},
			})
		}
	}
	// Latest assistant turn's usage drives the LIVE status-bar token count. A
	// ranged (historical) slice has no "latest turn" to report, so it stays nil.
	var usage *chatUsage
	for i := len(recs) - 1; !ranged && i >= 0; i-- {
		a, ok := recs[i].Event.(*event.AssistantEvent)
		if !ok {
			continue
		}
		if a.Usage != nil {
			usage = &chatUsage{a.Usage.PromptTokens, a.Usage.CompletionTokens, a.Usage.TotalTokens}
		}
		break // newest assistant event (with or without usage)
	}

	writeJSON(w, http.StatusOK, chatFeed{Messages: msgs, PhaseCards: cards, Usage: usage})
}

// phaseState returns the phase boundary (events at or before it are rolled into
// cards) and the cards themselves. The newest closed phase is kept inline as
// live bubbles, so the boundary is the second-largest phase end_step; fewer than
// two phases means nothing is rolled up.
func (s *Server) phaseState(ctx context.Context, runID, sid string) (int, []phaseCard) {
	obs, err := s.store.GetObservations(ctx, runID, db.ObsFilter{
		Kind: "phase_summary", SessionID: sid,
	})
	if err != nil || len(obs) < 2 {
		return -1, nil
	}
	sort.Slice(obs, func(i, j int) bool { return obsInt(obs[i], "end_step", 0) < obsInt(obs[j], "end_step", 0) })

	rollable := obs[:len(obs)-1] // drop the newest closed phase (stays inline)
	cards := make([]phaseCard, 0, len(rollable))
	boundary := -1
	for _, o := range rollable {
		end := obsInt(o, "end_step", 0)
		cards = append(cards, phaseCard{
			StartStep: obsInt(o, "start_step", end),
			EndStep:   end,
			Title:     obsStr(o, "title"),
			Summary:   obsStr(o, "summary"),
		})
		if end > boundary {
			boundary = end
		}
	}
	return boundary, cards
}

// obsInt reads an integer from an observation's Data bag, tolerating the
// float64 that JSON round-trips produce.
func obsInt(o db.ObservationRecord, key string, def int) int {
	switch v := o.Data[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

func obsStr(o db.ObservationRecord, key string) string {
	if s, ok := o.Data[key].(string); ok {
		return s
	}
	return ""
}

// withConclusion is an assistant turn's text followed by the result its
// conclude call gives, if it makes one: the session's result reads as the
// turn's message, as a reply without tool calls does.
func withConclusion(a *event.AssistantEvent) string {
	tc, ok := a.ConcludeCall()
	if !ok {
		return a.Content
	}
	r := strings.TrimSpace(event.ConcludeResult(tc.Arguments))
	switch {
	case r == "":
		return a.Content
	case strings.TrimSpace(a.Content) == "":
		return r
	}
	return a.Content + "\n\n" + r
}

// staleVerdicts tells which verdicts told as information were stale (on a
// withdrawn candidate), read from the session's verification once, the first
// time a verdict told as information needs it.
type staleVerdicts struct {
	store            db.Store
	runID, sessionID string
	stale            map[string]bool // verifier + At
}

func staleKey(o *event.VerifyOutcome) string {
	return o.Verifier + "@" + o.At.UTC().Format(time.RFC3339Nano)
}

// bubble is o's verdict bubble, marked stale when it is.
func (sv *staleVerdicts) bubble(ctx context.Context, rec db.EventRecord, o *event.VerifyOutcome) chatBubble {
	b := verdictBubble(rec, o)
	if !o.Earlier {
		return b
	}
	if sv.stale == nil {
		sv.stale = map[string]bool{}
		if st, err := verify.Load(ctx, sv.store, sv.runID, sv.sessionID); err == nil {
			for i, e := range st.History {
				if e.Action == event.VerifyVerdict && e.Outcome != nil && (e.Stale || slices.Contains(st.Late, i)) {
					sv.stale[staleKey(e.Outcome)] = true
				}
			}
		}
	}
	b.Stale = sv.stale[staleKey(o)]
	return b
}

// verdictBubble shows what a session's verifier said, as the harness told it
// (a verify message, or a conclude call's result). The verifier's own record
// of its outcome isn't shown: a verdict for an earlier conclusion was never
// told.
func verdictBubble(rec db.EventRecord, o *event.VerifyOutcome) chatBubble {
	return chatBubble{EventID: rec.EventID, Kind: "verdict", Content: o.Findings, From: o.Verifier,
		Verdict: o.Verdict, Outcome: o, Step: rec.Step, CreatedAt: rec.CreatedAt, ToolCalls: []chatToolCall{}}
}

// chatNote is a child_result note as the chat shows it.
type chatNote struct {
	Kind  string `json:"kind"` // an event.Note* kind, for styling
	Label string `json:"label"`
	Text  string `json:"text"`
}

func chatNotes(notes []event.ResultNote) []chatNote {
	out := make([]chatNote, 0, len(notes))
	for _, n := range notes {
		out = append(out, chatNote{Kind: n.Kind, Label: event.NoteLabel(n.Kind), Text: n.Text})
	}
	return out
}

// verifyEventText is an operator's or parent's change to a verification as a
// short note for the chat ("verification set by the operator" and what it
// checks), or "" for the harness's actions and for a version a loop's tick
// started (the tick's bubble shows it).
func verifyEventText(e *event.VerifyEvent) string {
	verb, ok := map[string]string{
		event.VerifySet: "set", event.VerifyReplace: "replaced", event.VerifyClear: "cleared",
	}[e.Action]
	if !ok || event.LoopOf(e.By) != "" {
		return ""
	}
	head := "verification " + verb
	switch e.By {
	case event.ByOperator:
		head += " by the operator"
	case "", event.ByHarness:
	default:
		head += " by " + e.By
	}
	if e.Action == event.VerifyClear {
		return head
	}
	criteria, model, rounds := e.Criteria, e.Model, "unbounded rounds"
	if criteria == "" {
		criteria = "the agent's current request"
	}
	if model == "" {
		model = "the agent's own"
	}
	switch {
	case e.MaxRounds == 1:
		rounds = "1 round"
	case e.MaxRounds > 1:
		rounds = fmt.Sprintf("up to %d rounds", e.MaxRounds)
	}
	return head + "\nagainst: " + criteria + "\nverifier model: " + model + " · " + rounds
}
