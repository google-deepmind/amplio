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

// Package conclude is the tool an autonomous agent finishes with, in two
// forms: with a result, and a verifier's with a verdict. The agent's event loop
// runs it (internal/agent/eventloop/conclude.go): its result is written with
// the conclusion it ends in, and a conclusion check may hold it, or make it
// wait for a verifier's verdict.
package conclude

import (
	"context"
	"encoding/json"

	"amplio/internal/event"
	"amplio/internal/tool"
)

// Params are the tool's arguments; the JSON matches event.ConcludeArgs.
type Params struct {
	Result string `json:"result,omitempty" jsonschema_description:"Your final deliverable, sent directly to the requester (your parent agent or the operator). Write a concise, self-contained summary of what you did and what you found. Omit this field **only** when you have already submitted a result and are waiting for the verifier's verdict on it."`
}

// New returns the conclude tool.
func New() *tool.Tool {
	return &tool.Tool{
		Name: event.ConcludeTool,
		Description: "Finish your task, deliver `result` to the requester, and end this session. " +
			"Call `conclude` alone in its own turn after all prior action results have returned. " +
			"Instead of ending the session, this call may return an actionable notice: " +
			"(1) background jobs or sub-agents still running; " +
			"(2) new input that arrived while concluding (handle it, then call `conclude` again); or " +
			"(3) verifier findings to address (if verification fails). " +
			"When verification is enabled, `conclude` blocks until the verifier renders a verdict: " +
			"a PASS immediately ends the session with `result`, while a FAIL returns the verifier's findings. " +
			"If other input arrives while waiting, this call returns early so you can inspect it—" +
			"call `conclude()` with no `result` to resume waiting on your existing candidate, " +
			"or pass a new `result` if changes are needed. " +
			"If the request is updated (starting a new verification version), your pending `result` is withdrawn; " +
			"fulfill the updated request and call `conclude` with a new `result`.",
		ParamType: &Params{},
		// Alone in its turn: what it concludes with must not depend on results
		// still to come.
		Exclusive:       true,
		SessionRequired: true,
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			return &tool.Result{Content: "Error: conclude is run by the agent loop, which didn't take this call.", IsError: true}, nil
		},
	}
}

// VerdictParams are a verifier's conclude arguments; the JSON matches
// event.ConcludeArgs.
type VerdictParams struct {
	Verdict  string `json:"verdict" jsonschema:"required,enum=PASS,enum=FAIL" jsonschema_description:"PASS if the conclusion meets the criteria, as your checks showed; FAIL otherwise, including when you couldn't establish that it does."`
	Findings string `json:"findings" jsonschema:"required" jsonschema_description:"What you checked and found. For a PASS, the evidence (commands run, their output, what you read). For a FAIL, concretely what is missing or wrong, for the agent to fix."`
}

// NewVerdict returns a verifier's conclude tool: it delivers a verdict on the
// conclusion it checked.
func NewVerdict() *tool.Tool {
	return &tool.Tool{
		Name: event.ConcludeTool,
		Description: "Finish your verification, deliver your verdict on the candidate conclusion to the target agent, " +
			"and end this session. Call `conclude` alone in its own turn after all check results have returned; " +
			"a plain-text reply without tool calls does not submit a verdict. " +
			"Instead of ending the session, this call may return an actionable notice: " +
			"(1) work you left running (call `conclude` again to conclude anyway); or " +
			"(2) new input that arrived while concluding, such as a newer candidate " +
			"(evaluate it first, then call `conclude` with your updated verdict). " +
			"If the candidate you are checking has been withdrawn, do not call `conclude`: end your turn " +
			"with a brief plain-text reply instead (a verdict on a withdrawn candidate is ignored), " +
			"and the next candidate will wake you.",
		ParamType:       &VerdictParams{},
		Exclusive:       true,
		SessionRequired: true,
		Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
			return &tool.Result{Content: "Error: conclude is run by the agent loop, which didn't take this call.", IsError: true}, nil
		},
	}
}
