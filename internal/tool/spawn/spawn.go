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

// Package spawn provides the tool for spawning sub-agent sessions.
package spawn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/util"
	"amplio/internal/verify"
	"amplio/internal/workspace"
)

type Params struct {
	Task          string          `json:"task" jsonschema:"required" jsonschema_description:"Task description for the sub-agent"`
	AgentType     string          `json:"agent_type,omitempty" jsonschema_description:"Agent type to spawn (default: standard_agent)"`
	WorkspaceMode string          `json:"workspace_mode,omitempty" jsonschema_description:"share (default): work in the same workspace as the parent. link: an isolated linked workspace (git/jj worktree or CitC link) sharing history but with independent file edits — use for parallel sub-agents that must not clobber each other."`
	Model         string          `json:"model,omitempty" jsonschema_description:"LLM for the sub-agent: a model spec, or a #nickname from the model menu. Omit it to use your own model; set it only when the operator names a model."`
	Verify        *verify.Request `json:"verify,omitempty" jsonschema_description:"Have an independent verifier check the sub-agent's conclusion before it reaches you (costs a full agent per check; for results that must be right). Rejected conclusions go back to the sub-agent until one passes or the rounds run out; the result you get is marked verified or not verified."`
}

func New(env *agent.Env, parentSessionID string) *tool.Tool {
	return &tool.Tool{
		Name: "spawn_agent",
		Description: "Spawn an autonomous sub-agent to work on a task in parallel. It runs independently and " +
			"notifies you when it completes. A concluded sub-agent can be re-engaged via `send_message`. " +
			"if it is not running, it will be revived with full prior context. If there is follow-up " +
			"work that benefits from prior context, re-engage an existing sub-agent instead of spawning a fresh one.",
		ParamType:       &Params{},
		SessionRequired: true,
		Execute:         makeExecutor(env, parentSessionID),
	}
}

func makeExecutor(env *agent.Env, parentSessionID string) tool.Executor {
	return func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
		params, errResult := tool.ParseArgs[Params](args)
		if errResult != nil {
			return errResult, nil
		}
		opts := Options{Task: params.Task, AgentType: params.AgentType, Model: params.Model, WorkspaceMode: params.WorkspaceMode}
		verifyInfo := ""
		if params.Verify != nil {
			e, err := params.Verify.Resolve(ctx, parentSessionID, env.Verify, env.ResolveLLM)
			if err != nil {
				return &tool.Result{Content: fmt.Sprintf("Error: %s", err), IsError: true}, nil
			}
			opts.Verify, verifyInfo = e, ", verified"
		}
		started, err := Start(ctx, env, parentSessionID, opts)
		if err != nil {
			return &tool.Result{Content: fmt.Sprintf("Error: %s", err), IsError: true}, nil
		}
		modelInfo, linkedInfo := "", ""
		if started.Model != "" {
			modelInfo = ", model=" + started.Model
		}
		if started.LinkedRoot != "" {
			linkedInfo = fmt.Sprintf(" in linked workspace %s", started.LinkedRoot)
		}
		return &tool.Result{
			Content: fmt.Sprintf("Spawned sub-agent %q (type=%s%s%s)%s at %s for task: %s",
				started.SessionID, started.AgentType, modelInfo, verifyInfo, linkedInfo,
				util.FormatLocalISO(time.Now()), util.TruncateRunes(params.Task, 80)),
		}, nil
	}
}

// Options describe a sub-agent to start.
type Options struct {
	Task          string
	AgentType     string // "" = standard_agent
	Model         string // "" = the parent's
	WorkspaceMode string // "link" for an isolated linked workspace; else the parent's
	// VerifierFor marks a verifier the harness starts for its parent.
	VerifierFor string
	// Verify turns verification on for the sub-agent from its first turn.
	Verify *event.VerifyEvent
}

// Started describes a sub-agent Start launched.
type Started struct {
	SessionID  string
	AgentType  string
	Model      string // the spec it runs on, if not the parent's
	LinkedRoot string // its linked workspace, if it got one
}

// Start launches a sub-agent of parentSessionID: spawn_agent, and the
// verifiers the harness starts. It returns once the sub-agent's goroutine is
// started; its session row is created there (docs/internals/session_lifecycle.md,
// Spawn).
func Start(ctx context.Context, env *agent.Env, parentSessionID string, o Options) (Started, error) {
	agentType := o.AgentType
	if agentType == "" {
		agentType = "standard_agent"
	}
	factory, err := agent.Get(agentType)
	if err != nil {
		return Started{}, err
	}
	if agent.IsVerifier(agentType) && o.VerifierFor == "" {
		return Started{}, fmt.Errorf("a %s checks another agent's conclusion and is started by verification; to have a "+
			"sub-agent's conclusion verified, pass verify to spawn_agent", agentType)
	}
	if o.Verify != nil && agent.IsInteractive(agentType) {
		return Started{}, fmt.Errorf("%s is an interactive agent type: it never concludes, so there is nothing to verify", agentType)
	}

	// Model: the parent's by default (the child gets a copy of its Env), or the
	// one asked for. Resolved before anything is allocated or created, so a bad
	// name costs nothing to refuse.
	started := Started{AgentType: agentType}
	childEnv := env
	if model := strings.TrimSpace(o.Model); model != "" {
		if env.ResolveLLM == nil {
			return Started{}, fmt.Errorf("this server cannot start sub-agents on another model; omit model to use your own")
		}
		spec, provider, err := env.ResolveLLM(ctx, model)
		if err != nil {
			return Started{}, fmt.Errorf("model %w", err)
		}
		e := *env
		e.LLM, e.LLMSpec = provider, spec
		childEnv = &e
		started.Model = spec
	}

	// Allocate a unique session ID from the run's shared allocator (seeded
	// from the DB, reserved in-memory under a lock — so it excludes cold
	// sessions and never collides with a concurrent spawn).
	sessionID, err := env.Names.Next(ctx)
	if err != nil {
		return Started{}, fmt.Errorf("allocating session id: %w", err)
	}
	started.SessionID = sessionID

	// Workspace: share the parent's by default, or create an isolated linked
	// workspace (worktree / CitC link) so parallel sub-agents don't clobber
	// each other's files. Named "<runID>-<session>" (workspace.LinkName), not
	// the bare session id: session ids repeat across runs, and a link's name
	// can live in a namespace shared by all runs (a CitC alias is per-user).
	if o.WorkspaceMode == "link" {
		linked, err := env.Workspace.CreateLinked(ctx, workspace.LinkName(env.RunID, sessionID))
		if err != nil {
			return Started{}, fmt.Errorf("creating linked workspace: %w", err)
		}
		e := *childEnv
		e.Workspace = linked
		childEnv = &e
		started.LinkedRoot = linked.Root()
	}

	// Claim the registry slot SYNCHRONOUSLY (before spawning the goroutine) so
	// the run-registry instance can't be orphaned by a concurrent RemoveIfEmpty,
	// and hand the resulting handle to the child via Config. Parent ctx is
	// context.Background() (NOT the parent's tool ctx) so the child keeps running
	// after the parent finishes/idle-exits (keep-alive). A freshly-allocated
	// session id can't already be registered, but guard defensively.
	childCtx, handle, release, ok := childEnv.Registry.RegisterAndContext(context.Background(), sessionID)
	if !ok {
		if childEnv.Registry.Closed() {
			return Started{}, fmt.Errorf("start session %q: %w", sessionID, session.ErrClosed)
		}
		return Started{}, fmt.Errorf("session %q already running", sessionID)
	}

	child, err := factory(childEnv, &agent.Config{
		SessionID:   sessionID,
		Task:        o.Task,
		ParentID:    parentSessionID,
		Handle:      handle,
		VerifierFor: o.VerifierFor,
		Verify:      o.Verify,
	})
	if err != nil {
		release() // release the claimed slot on a pre-launch failure
		return Started{}, fmt.Errorf("creating sub-agent: %w", err)
	}

	// Launch in a background goroutine on childCtx, a context independent of the
	// parent's tool ctx, so the parent finishing/idle-exiting does not cancel the
	// child (keep-alive). The child's loop posts its own result to the parent
	// atomically (TerminateAndNotifyParent) on conclude/crash/cancel; this runner
	// only backstops a panic that bypasses that path. release (cancel +
	// Unregister) is deferred FIRST so it runs LAST — after the panic backstop.
	go func() {
		defer func() {
			release()
			session.WarnUndelivered(childEnv.Store, childEnv.Registry, childEnv.RunID, sessionID, time.Now())
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("sub-agent panicked", "session", sessionID, "panic", r)
				msg := fmt.Sprintf("panic: %v", r)
				// -1: a panic backstop has no specific turn step; use current.
				// childEnv (not env) for consistency — they share Store/RunID today,
				// but only childEnv is guaranteed to describe the child.
				_, _ = childEnv.Store.TerminateAndNotifyParent(context.Background(),
					childEnv.RunID, sessionID, parentSessionID, db.SessionCrashed, msg, nil,
					db.SelfEvent{Event: &event.SystemEvent{Content: msg, Marker: event.MarkerError}, Step: -1})
			}
		}()
		if err := child.Run(childCtx); err != nil {
			slog.Error("sub-agent run error", "session", sessionID, "error", err)
		}
	}()
	return started, nil
}
