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

package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"amplio/internal/util"
)

// Event is the sealed interface for all event types.
//
// ToText renders the event as a labeled, plain-text section for consumers
// (e.g. summarizer, session inspection tools). The format is a
// banner header — "==== TYPE · k=v · … ====" — followed by the content, with
// no closing marker (the next header or turn boundary delimits it). It
// deliberately avoids XML/JSON/tool-call syntax so the model never mistakes
// presented content for an action it should emit.
//
// ToText is *not* what the LLMs consume as their own conversation history.
// That is separately handled via typed projection to the message formats
// expected by each LLM API Provider.
type Event interface {
	eventTag() // unexported — only types in this package can implement
	ToText() string
	EventType() string
}

// ColumnFields holds DB column values injected after deserialization.
// These are never included in JSON serialization (they come from DB columns).
type ColumnFields struct {
	Step       int       `json:"-"`
	Generation int       `json:"-"`
	CreatedAt  time.Time `json:"-"`
}

// --- Nested types ---

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// Attachment is a binary artifact produced by a tool (today only image/* from
// view_file). Raw bytes live in Data in-flight (tool → event loop) and are
// written to the run's on-disk blob store before persistence; only the content
// key, mime type and size are persisted in the event log (Data is never
// serialized), keeping SQLite event/FTS rows small. The UI fetches the bytes via
// the blob HTTP route using BlobKey.
type Attachment struct {
	MimeType   string `json:"mime_type"`
	BlobKey    string `json:"blob_key,omitempty"`
	Size       int    `json:"size,omitempty"`
	SourceHint string `json:"source_hint,omitempty"`
	Data       []byte `json:"-"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Cache token counts (omitempty keeps zero-cache/old rows unchanged).
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// banner renders content as a labeled section: a "==== LABEL · k=v · … ===="
// header line followed by the content. Empty attrs are dropped. There is no
// closing marker by design (see the Event interface doc).
func banner(label string, attrs []string, content string) string {
	head := label
	for _, a := range attrs {
		if a != "" {
			head += " · " + a
		}
	}
	return fmt.Sprintf("==== %s ====\n%s", head, content)
}

// tsAttr returns a "name=<local-iso>" attribute, or "" if t is zero.
func tsAttr(name string, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return name + "=" + util.FormatLocalISO(t)
}

// --- Concrete event types ---

// SystemEvent markers: a stable tag identifying which system event this is, so
// readers (tests, UI, inspection) can match on a constant rather than a string
// literal that can silently drift from the producer. Persisted in the event
// JSON, so DO NOT change an existing value (it would orphan stored rows); add a
// new one instead.
const (
	MarkerSystemPrompt  = "system_prompt"  // the agent's system prompt (step-0 bootstrap)
	MarkerCLITools      = "cli_tools"      // available external CLI tools (step-0 bootstrap)
	MarkerInitialRecall = "initial_recall" // session-start recall seed: pinned + task-relevant skills/lessons
	MarkerWorkspace     = "workspace"      // the agent's working-tree description (step-0 bootstrap)
	MarkerAgentsMD      = "agents_md"      // operator AGENTS.md instructions (step-0 bootstrap)
	MarkerNewSession    = "new_session"    // the trailing "new session started" bootstrap marker
	MarkerCancelled     = "cancelled"      // session was cancelled (self-marker on terminate)
	MarkerError         = "error"          // a crash/error self-marker
)

type SystemEvent struct {
	ColumnFields
	Content string `json:"content"`
	Marker  string `json:"marker,omitempty"` // a Marker* constant; see above
}

func (*SystemEvent) eventTag()         {}
func (*SystemEvent) EventType() string { return "system" }

func (e *SystemEvent) ToText() string {
	var attrs []string
	if e.Marker != "" {
		attrs = append(attrs, e.Marker)
	}
	return banner("system", attrs, e.Content)
}

type UserEvent struct {
	ColumnFields
	Content string `json:"content"`
	// Check is set when the harness, not the operator, wrote this message: a
	// nudge at a conclusion check (CheckLeftovers, CheckEmptyTurn,
	// CheckOutputLimit). The model sees it as any user message; the loop finds it
	// by Check rather than by its text, and UIs show it as the harness's.
	Check string `json:"check,omitempty"`
	// Loop is set on a loop's tick: the operator's prompt, re-sent on the
	// loop's schedule (see LoopEvent). Still operator input, not the harness's.
	Loop *LoopTick `json:"loop,omitempty"`
	// Outcome is set on a verify message that tells the agent what its
	// verifier said (Check is CheckVerify).
	Outcome *VerifyOutcome `json:"outcome,omitempty"`
}

// LoopTick identifies a tick: which loop sent it, and its number (1 = first).
type LoopTick struct {
	ID string `json:"id"`
	N  int    `json:"n"`
	// Version is the verification version the tick started, for a loop that
	// verifies each tick (LoopEvent.Verify); 0 otherwise.
	Version int `json:"version,omitempty"`
}

// Conclusion checks that write a UserEvent, or answer a conclude call (see
// UserEvent.Check, ToolResultEvent.Check).
const (
	CheckLeftovers   = "leftovers"    // the end-of-run reminder: work still running
	CheckEmptyTurn   = "empty_turn"   // a final turn with no text
	CheckOutputLimit = "output_limit" // a final turn cut off by the output-token limit
	CheckVerify      = "verify"       // verification: a candidate given to a verifier, its outcome
	CheckConclude    = "conclude"     // a reply without tool calls while a conclusion awaits its verdict
	CheckReply       = "reply"        // a reply without tool calls, which doesn't conclude ([agent] require_conclude)
	// CheckWithdrawn tells a verifier that the candidate it has was withdrawn
	// (its version superseded, or verification turned off): it stands by for
	// the next one.
	CheckWithdrawn = "withdrawn"
)

// IsHarness reports whether the harness wrote this message. Events written
// before Check existed are recognised by the "[system] " prefix every harness
// nudge carries.
func (e *UserEvent) IsHarness() bool {
	return e.Check != "" || strings.HasPrefix(e.Content, "[system] ")
}

func (*UserEvent) eventTag()         {}
func (*UserEvent) EventType() string { return "user" }
func (e *UserEvent) ToText() string  { return banner("user", nil, e.Content) }

type AssistantEvent struct {
	ColumnFields
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Thoughts  string     `json:"thoughts,omitempty"`
	// StopReason is the provider's native finish/stop reason for this turn
	// (Anthropic: end_turn/max_tokens/tool_use/…; Gemini: STOP/MAX_TOKENS/…;
	// subprocess: bridge-defined). NOT normalized across providers — persisted
	// verbatim for inspection/debugging (e.g. diagnosing an accidental empty
	// no-tool conclusion). Omitted for events written before this field existed.
	StopReason string `json:"stop_reason,omitempty"`
	// StopMessage is the provider's detail on why generation stopped (see
	// llm.Response.StopMessage). Omitted when empty.
	StopMessage string `json:"stop_message,omitempty"`
	// Refusal is set when the provider declined to answer.
	Refusal       *Refusal       `json:"refusal,omitempty"`
	ProviderExtra map[string]any `json:"provider_extra,omitempty"`
	Usage         *Usage         `json:"usage,omitempty"`
}

type Refusal struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

func (*AssistantEvent) eventTag()         {}
func (*AssistantEvent) EventType() string { return "assistant" }

func (e *AssistantEvent) ToText() string {
	var body strings.Builder
	if e.Content != "" {
		body.WriteString(e.Content)
	}
	conclusion, concludes := e.ConcludeCall()
	for _, tc := range e.ToolCalls {
		if body.Len() > 0 {
			body.WriteByte('\n')
		}
		// A lighter rule so a nested call reads as part of the assistant block,
		// not a new top-level section.
		args := tc.Arguments
		if r := ConcludeResult(args); concludes && tc.ID == conclusion.ID && r != "" {
			args = r // the result as written, not JSON-escaped
		}
		fmt.Fprintf(&body, "---- tool_call · name=%s · id=%s ----\n%s", tc.Name, tc.ID, args)
	}
	// Surface a refusal so a refused turn doesn't read as a silent empty reply.
	if r := e.Refusal; r != nil {
		if body.Len() > 0 {
			body.WriteByte('\n')
		}
		category := r.Category
		if category == "" {
			category = "unspecified"
		}
		fmt.Fprintf(&body, "---- refusal · category=%s ----", category)
		if r.Explanation != "" {
			body.WriteString("\n" + r.Explanation)
		}
	}
	return banner("assistant", nil, body.String())
}

// IsNoToolAssistant reports whether e is an AssistantEvent with no tool calls —
// the "natural rest" tail predicate used by crash recovery (a stream ending in
// such an event means the agent produced its final reply and is done).
func IsNoToolAssistant(e Event) bool {
	a, ok := e.(*AssistantEvent)
	return ok && len(a.ToolCalls) == 0
}

// ConcludeTool is the tool an autonomous agent finishes with. Its "result"
// argument is the session's result; called without one, it waits for the
// verdict on the result it gave before. A verifier's conclude takes a verdict
// and findings instead.
const ConcludeTool = "conclude"

// ConcludeArgs are the conclude tool's arguments, in either form: a result, or
// a verifier's verdict (VerdictPass or VerdictFail) and findings.
type ConcludeArgs struct {
	Result   string `json:"result,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	Findings string `json:"findings,omitempty"`
}

// ConcludeResult reads the result from a conclude call's arguments; a
// verifier's verdict reads as its result text (FormatVerdict). "" when they
// give none (or don't parse). A call that gives both forms is refused, so it
// never concludes whichever this shows.
func ConcludeResult(args string) string {
	var a ConcludeArgs
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	if pass, findings, ok := VerdictArgs(args); ok && a.Result == "" {
		return FormatVerdict(pass, findings)
	}
	return a.Result
}

// ConcludeCall returns the turn's conclude call if that is its only call. A
// conclude call made alongside others fails on its own (the tool is
// exclusive): it is no attempt to conclude.
func (e *AssistantEvent) ConcludeCall() (ToolCall, bool) {
	if len(e.ToolCalls) == 1 && e.ToolCalls[0].Name == ConcludeTool {
		return e.ToolCalls[0], true
	}
	return ToolCall{}, false
}

// Offered returns the result this turn offers as the session's result: the
// text of a turn without tool calls (the way an agent without the conclude
// tool, or one that replies instead of calling it, concludes), or the result
// a conclude call gives. ok is false for a working turn and for a conclude
// call without a result.
func (e *AssistantEvent) Offered() (result string, ok bool) {
	if len(e.ToolCalls) == 0 {
		return e.Content, true
	}
	if tc, found := e.ConcludeCall(); found {
		if r := ConcludeResult(tc.Arguments); strings.TrimSpace(r) != "" {
			return r, true
		}
	}
	return "", false
}

type ToolResultEvent struct {
	ColumnFields
	Content     string       `json:"content"`
	ToolCallID  string       `json:"tool_call_id"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// IsError is the tool's own judgment that the call failed to do its job (a
	// parse error, target-not-found, unavailable backend, panic, …). It mirrors
	// tool.Result.IsError. omitempty keeps successful results byte-identical to
	// pre-field rows, so this is a backward-compatible additive change (absent key
	// → false → "not an error", the correct default for historical events).
	IsError bool `json:"is_error,omitempty"`
	// Check names the conclusion check that answered a conclude call instead
	// of letting it conclude (CheckLeftovers, CheckVerify), as UserEvent.Check
	// does for a reply without tool calls.
	Check string `json:"check,omitempty"`
	// Outcome is set on a conclude call's result that tells the agent what its
	// verifier said.
	Outcome *VerifyOutcome `json:"outcome,omitempty"`
}

func (*ToolResultEvent) eventTag()         {}
func (*ToolResultEvent) EventType() string { return "tool_result" }

func (e *ToolResultEvent) ToText() string {
	body := e.Content
	if len(e.Attachments) > 0 {
		body += fmt.Sprintf("\n[%d attachment(s) omitted]", len(e.Attachments))
	}
	return banner("tool_result", []string{"id=" + e.ToolCallID}, body)
}

type CompactionEvent struct {
	ColumnFields
	Content string `json:"content"`
}

func (*CompactionEvent) eventTag()         {}
func (*CompactionEvent) EventType() string { return "compaction" }
func (e *CompactionEvent) ToText() string {
	return banner("compaction", []string{tsAttr("at", e.CreatedAt)}, e.Content)
}

// MessageEvent sender types. SenderType classifies the sender for rendering/
// attribution. Both agent messages (send_message) and environment notifications
// ($AMPLIO_NOTIFY) are Input-class in db.Classify. SenderType additionally gates
// ONE thing at the wake path (runtime.NewCommitNotifier): an environment
// notification does not revive a deliberately finished session. See
// docs/internals/session_lifecycle.md.
const (
	SenderTypeAgent       = "agent"
	SenderTypeEnvironment = "environment"
)

// EnvironmentSenderID is the default Sender (identity, not type) for a
// MessageEvent from an external background script (e.g. the notify CLI) when no
// --from label is given. Distinct axis from SenderTypeEnvironment above
// (category): the two share the literal "environment" only by coincidence — a
// named notifier sets its own Sender while SenderType stays environment.
const EnvironmentSenderID = "environment"

// MessageEvent is an agent-to-agent message (via send_message) or an environment
// notification ($AMPLIO_NOTIFY). It is NOT used for operator/user input — that is
// a UserEvent (a plain user turn), so a chatbot responds to it naturally instead
// of trying to "reply" to the user via send_message.
//
// SenderType classifies the sender (agent vs environment); Sender identifies the
// specific sender: the sending session's ID for an agent message, or the script's
// name for an environment notification (so multiple notifiers are distinguishable).
type MessageEvent struct {
	ColumnFields
	Content    string `json:"content"`
	Sender     string `json:"sender,omitempty"`
	SenderType string `json:"sender_type,omitempty"`
}

func (*MessageEvent) eventTag()         {}
func (*MessageEvent) EventType() string { return "message" }

func (e *MessageEvent) ToText() string {
	var attrs []string
	if e.Sender != "" {
		attrs = append(attrs, "from="+e.Sender)
	}
	// Render the sender type only for non-agent senders; agent is the common
	// case and is left implicit.
	if e.SenderType != "" && e.SenderType != SenderTypeAgent {
		attrs = append(attrs, "type="+e.SenderType)
	}
	attrs = append(attrs, tsAttr("at", e.CreatedAt))
	return banner("message", attrs, e.Content)
}

type ChildResultEvent struct {
	ColumnFields
	Content        string `json:"content"`
	ChildSessionID string `json:"child_session_id"`
	Verdict        string `json:"verdict"` // concluded, crashed, cancelled
	// Notes are facts about how the child concluded, for its parent (e.g. what
	// it left running), kept out of Content, which is its result exactly as it
	// wrote it.
	Notes []ResultNote `json:"notes,omitempty"`
	// Leftover is the single note of events written before Notes existed; read
	// it through AllNotes.
	Leftover string `json:"leftover,omitempty"`
}

// ResultNote is one fact on a child_result: Kind is a machine name (NoteLeftover),
// Text the fact itself.
type ResultNote struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Kinds of ResultNote.
const (
	NoteLeftover    = "leftover"     // what the child left running when it concluded
	NoteVerified    = "verified"     // a verifier passed its conclusion
	NoteNotVerified = "not_verified" // its verification ran out of rounds
)

// NoteLabel is how a note kind reads in a header or a UI.
func NoteLabel(kind string) string {
	switch kind {
	case NoteLeftover:
		return "left running"
	case NoteVerified:
		return "verified by"
	case NoteNotVerified:
		return "not verified"
	default:
		return kind
	}
}

// AllNotes returns Notes, plus the legacy Leftover of events written before
// Notes existed.
func (e *ChildResultEvent) AllNotes() []ResultNote {
	if e.Leftover == "" {
		return e.Notes
	}
	return append([]ResultNote{{Kind: NoteLeftover, Text: e.Leftover}}, e.Notes...)
}

func (*ChildResultEvent) eventTag()         {}
func (*ChildResultEvent) EventType() string { return "child_result" }

func (e *ChildResultEvent) ToText() string {
	attrs := []string{"child=" + e.ChildSessionID, "verdict=" + e.Verdict, tsAttr("at", e.CreatedAt)}
	for _, n := range e.AllNotes() {
		attrs = append(attrs, NoteLabel(n.Kind)+": "+n.Text)
	}
	return banner("child_result", attrs, e.Content)
}

// RecoverEvent is the "resumed" marker appended by run-level Recover to a
// crash-recovered session's own stream (see docs/internals/session_lifecycle.md). Recover
// is the only producer (never normal agent flow) and sets Content to the marker
// text the LLM sees. It is a user-role event (providers require the last
// pre-assistant message to be a user/tool-result message) and classifies as Input.
type RecoverEvent struct {
	ColumnFields
	Content string `json:"content,omitempty"`
}

func (*RecoverEvent) eventTag()         {}
func (*RecoverEvent) EventType() string { return "recover" }

func (e *RecoverEvent) ToText() string { return banner("recover", nil, e.Content) }

// UnknownEvent stands in for a stored event whose type this binary doesn't
// know: one written by a newer amplio, read after a downgrade. Reading it
// rather than failing keeps the session readable. It is kept in the stream and
// marshals back to the stored JSON, but nothing projects it: not the model's
// context, the chat, the inspect and search tools, or the summaries. It
// classifies as a Notice and wakes nothing.
//
// Unmarshal produces it for an unknown type. A newer binary can also add a
// value or a field to a type this one knows; that is each type's decoder to
// handle (unknown fields are ignored; an unknown value of a field that drives
// behaviour, such as an action, can read as this placeholder too).
type UnknownEvent struct {
	ColumnFields
	Type string          // the stored "type"
	Raw  json.RawMessage // the stored JSON, verbatim
}

func (*UnknownEvent) eventTag()           {}
func (e *UnknownEvent) EventType() string { return e.Type }

// ToText names the type only: the content is a newer binary's, not this one's
// to interpret.
func (e *UnknownEvent) ToText() string { return banner("unknown", []string{"type=" + e.Type}, "") }

// IsUnknown reports whether e is an UnknownEvent placeholder.
func IsUnknown(e Event) bool {
	_, ok := e.(*UnknownEvent)
	return ok
}

// Who changed a verification or a loop (VerifyEvent.By, LoopEvent.By), besides
// a parent, named by its session id, and a loop (ByLoop).
const (
	ByOperator = "operator"
	ByHarness  = "harness"
)

// ByLoop is VerifyEvent.By for a version a loop's tick started: "loop 3", on
// the operator's behalf. A session id has no spaces, so it can't be one.
func ByLoop(id string) string { return "loop " + id }

// LoopOf returns the loop a VerifyEvent.By names (see ByLoop), or "".
func LoopOf(by string) string {
	id, ok := strings.CutPrefix(by, "loop ")
	if !ok {
		return ""
	}
	return id
}

// VerifyEvent records a change to a session's verification: a standing request
// that its next conclusion pass an independent verifier first (see
// internal/verify). The stream of these is the verification's state and
// history.
type VerifyEvent struct {
	ColumnFields
	Action string `json:"action"` // a Verify* action
	// Set by set and replace: what the verifier checks against ("" = the
	// agent's current request), its model ("" = the agent's own), and how many
	// rejections end it (0 = unbounded).
	Criteria  string `json:"criteria,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
	// By is who made the change: ByOperator, the parent's session id, or
	// ByHarness.
	By string `json:"by,omitempty"`
	// Verifier and Round are set by the harness's own actions: the verifier
	// session, and the round (1 + the rejections before it).
	Verifier string `json:"verifier,omitempty"`
	Round    int    `json:"round,omitempty"`
	// Reason is set by exhausted: why verification ended unverified (the
	// not_verified note's text), since it isn't always the rounds; and by
	// withdrawn and stale: which candidate, and why.
	Reason string `json:"reason,omitempty"`
	// Candidate is set by verifying: the conclusion given to the verifier, the
	// one a pass delivers.
	Candidate string `json:"candidate,omitempty"`
	// Outcome is set by verdict: what the verifier said, or how it stopped;
	// and by stale: the verdict that doesn't count.
	Outcome *VerifyOutcome `json:"outcome,omitempty"`
}

// VerifyEvent actions. Set, replace and clear are the operator's or parent's;
// the rest are the harness's. Every set or replace starts a new version of the
// verification (v1, v2, …), counted from the stream.
const (
	VerifySet       = "set"       // verification turned on: a new version
	VerifyReplace   = "replace"   // a new request while it was on: a new version, which supersedes the old one
	VerifyClear     = "clear"     // turned off before a verdict
	VerifyVerifying = "verifying" // a candidate conclusion given to the verifier
	VerifyRejected  = "rejected"  // the verifier rejected the latest candidate: one round
	VerifyPassed    = "passed"    // the verifier passed it; recorded with the conclusion
	VerifyExhausted = "exhausted" // the rounds ran out; recorded with the conclusion
	// The verifier finished (Outcome): its own terminal transition posts this
	// to the agent instead of a child_result, so its verdict reaches the agent
	// only as the harness tells it.
	VerifyVerdict = "verdict"
	// A newer version or a clear withdrew the candidate the verifier had; it
	// is never delivered. Written right after the change (Round: the round it
	// was in; Reason: which candidate, and why). Unlike the other records it
	// wakes a live agent, ending a conclude call that waits for that candidate.
	VerifyWithdrawn = "withdrawn"
	// A verifier's verdict that doesn't count (Outcome: whose, and which): on
	// a candidate a newer version or a clear withdrew before the harness acted
	// on it (Reason: which candidate, and what withdrew it). It changes nothing.
	VerifyStale = "stale"
)

// verifyActions are the VerifyEvent actions this binary knows. A stored
// VerifyEvent with another (a newer binary's) reads as an UnknownEvent
// placeholder: misread as a known one, it would change the state derived from
// the stream.
var verifyActions = map[string]bool{VerifySet: true, VerifyReplace: true, VerifyClear: true, VerifyVerifying: true,
	VerifyRejected: true, VerifyPassed: true, VerifyExhausted: true, VerifyVerdict: true, VerifyWithdrawn: true,
	VerifyStale: true}

func (*VerifyEvent) eventTag()         {}
func (*VerifyEvent) EventType() string { return "verify" }

func (e *VerifyEvent) ToText() string {
	attrs := []string{"action=" + e.Action}
	if e.By != "" {
		attrs = append(attrs, "by="+e.By)
	}
	if e.Verifier != "" {
		attrs = append(attrs, "verifier="+e.Verifier)
	}
	if e.Round > 0 {
		attrs = append(attrs, fmt.Sprintf("round=%d", e.Round))
	}
	var body []string
	if e.Action == VerifySet || e.Action == VerifyReplace {
		criteria := e.Criteria
		if criteria == "" {
			criteria = "(the agent's current request)"
		}
		model := e.Model
		if model == "" {
			model = "(the agent's own)"
		}
		rounds := "unbounded"
		if e.MaxRounds > 0 {
			rounds = fmt.Sprint(e.MaxRounds)
		}
		body = append(body, "criteria: "+criteria, "model: "+model, "max rounds: "+rounds)
	}
	if o := e.Outcome; o != nil {
		attrs = append(attrs, "verdict="+o.Verdict)
		if o.Findings != "" {
			body = append(body, o.Findings)
		}
	}
	if e.Reason != "" {
		body = append(body, "reason: "+e.Reason)
	}
	return banner("verify", attrs, strings.Join(body, "\n"))
}

// LoopEvent records a change to one of a session's loops: a prompt the
// operator asked to have re-sent on a schedule (internal/loop). It wakes
// nothing (db.IsRecordOnly), and the agent isn't shown it: its ticks,
// UserEvents with Loop set, wake the agent and name their loop.
type LoopEvent struct {
	ColumnFields
	Action string `json:"action"` // LoopSet, LoopEdit, LoopStop or LoopEnded
	ID     string `json:"id"`
	// Prompt and Schedule are the loop's after a set or an edit, and Summary
	// the schedule in words, as its ticks' labels give it.
	Prompt   string    `json:"prompt,omitempty"`
	Schedule *Schedule `json:"schedule,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	// Verify is the loop's verification request after a set or an edit (nil:
	// its ticks aren't verified); VerifyStopped marks the edit that stopped it.
	Verify        *LoopVerify `json:"verify,omitempty"`
	VerifyStopped bool        `json:"verify_stopped,omitempty"`
	By            string      `json:"by,omitempty"`     // ByOperator or ByHarness
	Reason        string      `json:"reason,omitempty"` // why it ended
}

// LoopVerify is a loop's verification request, its defaults filled in when
// the loop was set: each tick is a new task, so each starts a new version of
// the session's verification with it, just before the tick's message.
type LoopVerify struct {
	Criteria  string `json:"criteria,omitempty"` // "" = the agent's current request: the tick
	Model     string `json:"model,omitempty"`    // a resolved spec; "" = the agent's own
	MaxRounds int    `json:"max_rounds"`         // 0 = unbounded
}

// maxCriteriaRunes caps the criteria LoopVerify.String shows.
const maxCriteriaRunes = 200

// Event is the set (verify.Set makes it a replace if verification is on) by
// which loop id starts a tick's version.
func (v LoopVerify) Event(id string) *VerifyEvent {
	return &VerifyEvent{Action: VerifySet, Criteria: v.Criteria, Model: v.Model, MaxRounds: v.MaxRounds, By: ByLoop(id)}
}

// String is the request on one line: "criteria: …; model: …; up to 3
// rounds", long criteria cut short.
func (v LoopVerify) String() string {
	var parts []string
	if c := strings.Join(strings.Fields(v.Criteria), " "); c != "" {
		if r := []rune(c); len(r) > maxCriteriaRunes {
			c = string(r[:maxCriteriaRunes-1]) + "…"
		}
		parts = append(parts, "criteria: "+c)
	}
	if v.Model != "" {
		parts = append(parts, "model: "+v.Model)
	}
	switch v.MaxRounds {
	case 0:
		parts = append(parts, "unbounded rounds")
	case 1:
		parts = append(parts, "up to 1 round")
	default:
		parts = append(parts, fmt.Sprintf("up to %d rounds", v.MaxRounds))
	}
	return strings.Join(parts, "; ")
}

// LoopEvent actions.
const (
	LoopSet   = "set"
	LoopEdit  = "edit"
	LoopStop  = "stop"
	LoopEnded = "ended"
)

// loopActions are the LoopEvent actions this binary knows; a stored LoopEvent
// with another (a newer binary's) reads as an UnknownEvent placeholder, as an
// unknown VerifyEvent action does.
var loopActions = map[string]bool{LoopSet: true, LoopEdit: true, LoopStop: true, LoopEnded: true}

// Schedule says when a loop ticks. Kind names its evaluator; the one kind is
// ScheduleRRule, whose Spec is an iCalendar recurrence: a DTSTART line, then
// RRULE, RDATE and EXDATE lines.
type Schedule struct {
	Kind string `json:"kind"`
	Spec string `json:"spec"`
}

// ScheduleRRule is the iCalendar-recurrence Schedule kind.
const ScheduleRRule = "rrule"

func (*LoopEvent) eventTag()         {}
func (*LoopEvent) EventType() string { return "loop" }

func (e *LoopEvent) ToText() string {
	attrs := []string{"id=" + e.ID, "action=" + e.Action}
	if e.By != "" {
		attrs = append(attrs, "by="+e.By)
	}
	return banner("loop", attrs, e.Body())
}

// Body is the event's content without a header: the schedule in words (its
// spec on one line, in an event written before Summary), the verification
// each tick starts, and why it ended. It leaves out the prompt: agents read
// this through the inspection tools, and would take it for a request.
func (e *LoopEvent) Body() string {
	var body []string
	switch {
	case e.Summary != "":
		body = append(body, "schedule: "+e.Summary)
	case e.Schedule != nil:
		body = append(body, "schedule: "+strings.Join(strings.Fields(e.Schedule.Spec), " "))
	}
	switch {
	case e.Verify != nil:
		body = append(body, "verify each tick: "+e.Verify.String())
	case e.VerifyStopped:
		body = append(body, "verify each tick: stopped")
	}
	if e.Reason != "" {
		body = append(body, e.Reason)
	}
	return strings.Join(body, "\n")
}

// --- Serialization ---

// Marshal converts an Event to a JSON blob with a "type" discriminator.
// Column fields (Step, Generation, CreatedAt) are NOT included (json:"-").
func Marshal(e Event) ([]byte, error) {
	if u, ok := e.(*UnknownEvent); ok {
		return u.Raw, nil // already carries its "type"
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	typeBytes, _ := json.Marshal(e.EventType())
	m["type"] = typeBytes
	return json.Marshal(m)
}

// Unmarshal parses a JSON blob into a typed Event, or an UnknownEvent for a
// type this binary doesn't know. The caller must set ColumnFields (Step,
// Generation, CreatedAt) separately from DB columns.
func Unmarshal(data []byte) (Event, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("event unmarshal probe: %w", err)
	}

	switch probe.Type {
	case "system":
		var e SystemEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "user":
		var e UserEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "assistant":
		var e AssistantEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "tool_result":
		var e ToolResultEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "compaction":
		var e CompactionEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "message":
		var e MessageEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "child_result":
		var e ChildResultEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "recover":
		var e RecoverEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case "verify":
		var e VerifyEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		if !verifyActions[e.Action] {
			return &UnknownEvent{Type: probe.Type, Raw: append(json.RawMessage(nil), data...)}, nil
		}
		return &e, nil
	case "loop":
		var e LoopEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		if !loopActions[e.Action] {
			return &UnknownEvent{Type: probe.Type, Raw: append(json.RawMessage(nil), data...)}, nil
		}
		return &e, nil
	case "":
		return nil, fmt.Errorf("event unmarshal: missing 'type' field")
	default:
		return &UnknownEvent{Type: probe.Type, Raw: append(json.RawMessage(nil), data...)}, nil
	}
}
