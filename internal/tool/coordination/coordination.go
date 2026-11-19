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

// Package coordination provides tools for multi-agent coordination:
// await_event (park until woken), send_message, and session_cancel.
package coordination

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/util"
	"amplio/internal/verify"
)

// Deps holds the dependencies needed by coordination tools.
type Deps struct {
	Store    db.Store
	RunID    string
	Registry *session.Registry
	// For send_message(verify): the [verify] defaults, and the resolver for a
	// verifier's model (nil: only the agent's own model).
	VerifyDefaults config.VerifyDefaults
	ResolveLLM     verify.ModelResolver
}

// --- await_event ---

// HandleFunc returns the session handle lazily (the handle is created
// during Run(), not at tool construction time).
type HandleFunc func() *session.Handle

// defaultAwaitTimeoutSeconds bounds a wait so a parked agent always re-evaluates
// rather than blocking forever (and so resume/lost-process detection makes
// progress). 5-minute default.
const defaultAwaitTimeoutSeconds = 300.0

type awaitParams struct {
	TimeoutSeconds float64 `json:"timeout,omitempty" jsonschema_description:"Max seconds to wait before returning (default 300). Returns early as soon as a new event arrives."`
}

func AwaitEvent(deps *Deps, sessionID string, getHandle HandleFunc) *tool.Tool {
	return &tool.Tool{
		Name: "await_event",
		Description: "Park this agent and wait for a new event in your session (e.g. a message from another agent or " +
			"the operator, or a result from a spawned child). Returns early as soon as something arrives, otherwise " +
			"after the timeout. Call it in the SAME step as the action that will produce the event — e.g. alongside " +
			"spawn_agent, send_message, or a background bash job — to act and wait in one turn.",
		ParamType: &awaitParams{},
		// Not exclusive: await_event may share a step with the action that will
		// produce the event (spawn_agent / send_message / a background bash job).
		// Its wake condition is a genuine future event — one at step T+1 or later
		// in the event store — NOT the notification counter. So the per-tool
		// results of its own step (written back at the call step T, and which now
		// commit mid-step as each tool finishes) never wake it, however the
		// concurrent appends interleave. The counter/WaitAfter is only a blocking
		// primitive: a wake not backed by a step-T+1 event just re-checks the
		// store and re-sleeps (mirrors waitForFollowUp's idle park).
		SessionRequired: true,
		Execute: func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
			params, errResult := tool.ParseArgs[awaitParams](args)
			if errResult != nil {
				return errResult, nil
			}
			handle := getHandle()
			if handle == nil {
				return &tool.Result{Content: "Error: session not initialized.", IsError: true}, nil
			}

			timeoutSeconds := params.TimeoutSeconds
			if timeoutSeconds <= 0 {
				timeoutSeconds = defaultAwaitTimeoutSeconds
			}
			deadline := time.Now().Add(time.Duration(timeoutSeconds * float64(time.Second)))

			sess, err := deps.Store.GetSession(ctx, deps.RunID, sessionID)
			if err != nil {
				return &tool.Result{Content: fmt.Sprintf("Error reading session: %s", err), IsError: true}, nil
			}
			// await is a tool call in step T; CurrentStep is T+1 (the loop
			// advanced the step before running tools). Wake only for events at
			// >= T+1: the agent's own tool results land back at T and must be
			// ignored. The store — not the counter — is the source of truth.
			step := sess.CurrentStep

			// Fast path: peer events already landed at T+1 during the LLM call —
			// return without flipping status. Records that wake no one
			// (db.IsRecordOnly) don't count, here or below.
			if n, _ := db.WakingEventsSince(ctx, deps.Store, deps.RunID, sessionID, step); n > 0 {
				return &tool.Result{Content: fmt.Sprintf(
					"New event(s) already present; returning immediately at %s without waiting.",
					util.FormatLocalISO(time.Now()))}, nil
			}

			// Conditional, here and below: never over a cancel that landed meanwhile.
			if set, err := deps.Store.SetStatusUnlessTerminal(ctx, deps.RunID, sessionID, db.SessionAwaiting); err == nil && !set {
				return &tool.Result{Content: "Wait ended: session terminated.", IsError: true}, nil
			}

			awoke := false
			for {
				// Snapshot the counter BEFORE the store check so a notify landing
				// between the check and WaitAfter still advances it and unblocks
				// the wait (closes the missed-notify race) — re-armed each loop.
				before := handle.Counter()
				if n, _ := db.WakingEventsSince(ctx, deps.Store, deps.RunID, sessionID, step); n > 0 {
					awoke = true
					break
				}
				remaining := time.Until(deadline)
				if remaining <= 0 {
					break // timed out
				}
				if _, waitErr := handle.WaitAfter(ctx, before, remaining); waitErr != nil {
					// Interrupted by ctx — a cancel, or process shutdown. A cancel
					// already set the terminal status; shutdown preserves awaiting
					// for recovery. Either way, do NOT restore ongoing.
					return &tool.Result{Content: "Wait interrupted.", IsError: true}, nil
				}
				// Woke on a notify (possibly our own step-T result, or the
				// assistant): loop and re-check the store for a real T+1 event.
			}

			// Normal wake/timeout. A cancel may have raced in via a notify: the
			// restore skips a terminal status, never clobbering it with ongoing.
			if set, err := deps.Store.SetStatusUnlessTerminal(context.Background(), deps.RunID, sessionID, db.SessionOngoing); err == nil && !set {
				return &tool.Result{Content: "Wait ended: session terminated.", IsError: true}, nil
			}
			if awoke {
				return &tool.Result{Content: fmt.Sprintf(
					"Awakened at %s: new event(s) arrived.", util.FormatLocalISO(time.Now()))}, nil
			}
			return &tool.Result{Content: fmt.Sprintf(
				"Timed out after %.0fs at %s; no new events arrived.",
				timeoutSeconds, util.FormatLocalISO(time.Now()))}, nil
		},
	}
}

// --- send_message ---

type sendMessageParams struct {
	SessionID string `json:"session_id" jsonschema:"required" jsonschema_description:"Target session ID to send the message to"`
	Content   string `json:"content" jsonschema:"required" jsonschema_description:"Message content"`
	// Two parameters rather than one that is an object or false: function
	// schemas with oneOf fare poorly on some providers.
	Verify      *verify.Request `json:"verify,omitempty" jsonschema_description:"Your own sub-agents only: have an independent verifier check the conclusion it reaches for this message before it is delivered to you (a new version of the request: it replaces a verification already on, and withdraws a conclusion still with the verifier). Costs a full agent per check; for results that must be right."`
	ClearVerify bool            `json:"clear_verify,omitempty" jsonschema_description:"Your own sub-agents only: turn its verification off, so its next conclusion is delivered as it is."`
}

func SendMessage(deps *Deps, fromSessionID string) *tool.Tool {
	return &tool.Tool{
		Name: "send_message",
		Description: "Send a message to another agent session; it appears in their event stream. " +
			"The recipient can be in ANY state: if it is currently live (running or parked) the message is " +
			"delivered to it; if it has stopped (idle, concluded, crashed, or cancelled) the message REVIVES it — " +
			"the agent restarts with your message appended to its full prior context and keeps working.",
		ParamType:       &sendMessageParams{},
		SessionRequired: true,
		Execute: func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
			params, errResult := tool.ParseArgs[sendMessageParams](args)
			if errResult != nil {
				return errResult, nil
			}

			// Verify the target exists so a typo'd id gets a clear message rather
			// than a generic store error from the append below.
			target, err := deps.Store.GetSession(ctx, deps.RunID, params.SessionID)
			if err != nil {
				return &tool.Result{
					Content: fmt.Sprintf("Error: no session %q in this run.", params.SessionID),
					IsError: true,
				}, nil
			}
			if refusal, err := toOwnVerifier(ctx, deps, fromSessionID, target, "message"); err != nil || refusal != "" {
				if err != nil {
					refusal = fmt.Sprintf("can't check who %s verifies: %s", target.SessionID, err)
				}
				return &tool.Result{Content: "Error: " + refusal + " The message was not sent.", IsError: true}, nil
			}
			// With a verification change, the message is written under its lock
			// once the change is in place (verify.SetThen), so the target wakes
			// to both: a conclude call it waits in settles only after the
			// message is there.
			sent := false
			send := func() {
				sent = true
				_, err = deps.Store.AppendEvent(ctx, deps.RunID, params.SessionID,
					&event.MessageEvent{Content: params.Content, Sender: fromSessionID, SenderType: event.SenderTypeAgent})
			}
			changed, cerr := changeVerification(ctx, deps, fromSessionID, target, params, send)
			if cerr != nil {
				return &tool.Result{Content: fmt.Sprintf("Error: %s. The message was not sent.", cerr), IsError: true}, nil
			}
			if !sent {
				send()
			}
			if err != nil {
				msg := fmt.Sprintf("Error sending message to %s: %s", params.SessionID, err)
				if changed != "" {
					// Applied before the send, which failed: say so, or a retry
					// would read as the first attempt.
					msg += ". The verification change went through all the same: " + strings.TrimPrefix(changed, "; ")
				}
				return &tool.Result{Content: msg, IsError: true}, nil
			}
			// Notify is automatic via the store notifier.
			return &tool.Result{Content: fmt.Sprintf("Message sent to %s at %s%s",
				params.SessionID, util.FormatLocalISO(time.Now()), changed)}, nil
		},
	}
}

// toOwnVerifier refuses a message or a cancel (verb) from a verified agent, or
// from one of its sub-agents, to that agent's verifier. A message revives a
// verifier that has given its verdict, and what it says then would count as a
// new verdict, so the agent could argue a rejection away; a cancel stops it,
// and stops in a row end the verification unverified. The agent's channel to
// its verifier is conclude. The verifier's own sub-agents may message or
// cancel it. The refusal is "" when the message or cancel may go.
func toOwnVerifier(ctx context.Context, deps *Deps, from string, target *db.SessionRecord, verb string) (string, error) {
	verified := verify.VerifierFor(target)
	if verified == "" {
		return "", nil
	}
	// The verifier is a sub-agent of the agent it verifies: walk up from the
	// sender to whichever of the two comes first.
	for id, depth := from, 0; id != "" && depth < maxAncestry; depth++ {
		switch id {
		case target.SessionID:
			return "", nil
		case verified:
			if from == verified {
				return fmt.Sprintf("%s is your verifier, started by amplio: you can't %s it. Call conclude() "+
					"to wait for its verdict, or conclude(result) to give it a newer one.", target.SessionID, verb), nil
			}
			return fmt.Sprintf("%s is the verifier of %s, which you work under, started by amplio: neither %s nor its "+
				"sub-agents can %s it; %s reaches it with conclude.", target.SessionID, verified, verified,
				verb, verified), nil
		}
		s, err := deps.Store.GetSession(ctx, deps.RunID, id)
		if err != nil {
			return "", err
		}
		id = s.ParentID
	}
	return "", nil
}

// maxAncestry bounds the walk up a session's ancestors.
const maxAncestry = 64

// changeVerification applies send_message's verify or clear_verify, which only
// the target's parent may pass, and says what it did. then, which writes the
// message, runs under the verification lock once a change is in place (see
// verify.SetThen); it doesn't run if nothing changed.
func changeVerification(ctx context.Context, deps *Deps, from string, target *db.SessionRecord, p *sendMessageParams, then func()) (string, error) {
	if p.Verify == nil && !p.ClearVerify {
		return "", nil
	}
	if p.Verify != nil && p.ClearVerify {
		return "", fmt.Errorf("pass verify or clear_verify, not both")
	}
	if err := verify.CheckParent(target, from); err != nil {
		return "", err
	}
	if p.ClearVerify {
		cleared, err := verify.ClearThen(ctx, deps.Store, deps.Registry, deps.RunID, target.SessionID, from, then)
		if err != nil || !cleared {
			return "; its verification was already off", err
		}
		return "; its verification is off", nil
	}
	e, err := p.Verify.Resolve(ctx, from, deps.VerifyDefaults, deps.ResolveLLM)
	if err != nil {
		return "", err
	}
	version, err := verify.SetThen(ctx, deps.Store, deps.Registry, deps.RunID, target.SessionID, e, then)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("; its conclusion will be verified before it reaches you (verification v%d)", version), nil
}

// --- session_cancel ---

type sessionCancelParams struct {
	SessionID string `json:"session_id" jsonschema:"required" jsonschema_description:"Session ID to cancel"`
	Reason    string `json:"reason,omitempty" jsonschema_description:"Why you are cancelling this session"`
}

func SessionCancel(deps *Deps, fromSessionID string) *tool.Tool {
	return &tool.Tool{
		Name: "session_cancel",
		Description: "Cancel another agent session and its sub-agents. The target is stopped " +
			"immediately — any in-flight work is discarded. The reason is recorded on the " +
			"target's stream and reported to its parent.",
		ParamType:       &sessionCancelParams{},
		SessionRequired: true,
		Execute: func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
			params, errResult := tool.ParseArgs[sessionCancelParams](args)
			if errResult != nil {
				return errResult, nil
			}
			// Not the verifier of this agent, or of one it works under: a stop
			// isn't a round, but stops in a row end the verification unverified,
			// so cancelling it would be a way around it.
			if target, err := deps.Store.GetSession(ctx, deps.RunID, params.SessionID); err == nil {
				refusal, err := toOwnVerifier(ctx, deps, fromSessionID, target, "cancel")
				if err != nil {
					return nil, err
				}
				if refusal != "" {
					return &tool.Result{Content: "Error: " + refusal, IsError: true}, nil
				}
			}

			reason := params.Reason
			if reason == "" {
				reason = "cancelled by peer agent"
			}

			// Canceller-driven (not cooperative): set status + write marker +
			// notify parent + interrupt the goroutine, recursively, right here.
			if err := session.CancelSession(deps.Store, deps.Registry, deps.RunID, params.SessionID, reason); err != nil {
				return &tool.Result{
					Content: fmt.Sprintf("Failed to cancel %s: %s", params.SessionID, err),
					IsError: true,
				}, nil
			}
			return &tool.Result{Content: fmt.Sprintf("Cancelled %s at %s",
				params.SessionID, util.FormatLocalISO(time.Now()))}, nil
		},
	}
}
