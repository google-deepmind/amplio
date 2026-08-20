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

package chatbot

import (
	"context"
	_ "embed"

	"amplio/internal/agent"
	"amplio/internal/agent/eventloop"
	"amplio/internal/agent/toolset"
	"amplio/internal/blob"
	"amplio/internal/briefing"
	"amplio/internal/cli"
	"amplio/internal/config"
	"amplio/internal/session"
	"amplio/internal/tool/bash"
)

//go:embed chatbot_core.md
var corePrompt string

//go:embed chatbot_root.md
var rootPreamble string

//go:embed chatbot_sidecar.md
var sidecarPreamble string

var (
	rootPrompt    = rootPreamble + "\n\n" + corePrompt
	sidecarPrompt = sidecarPreamble + "\n\n" + corePrompt
)

// systemPromptFor picks the role-appropriate prompt. The chatbot is a sidecar
// iff the run already has another top-level (parentless) session that isn't a
// chatbot — i.e. an autonomous main-agent. Otherwise it's the run's root.
func systemPromptFor(env *agent.Env, sessionID string) string {
	sessions, err := env.Store.ListSessions(context.Background(), env.RunID)
	if err != nil {
		return rootPrompt // safe default; root prompt has no shared-workspace claims
	}
	for _, s := range sessions {
		if s.ParentID == "" && s.SessionID != sessionID && s.AgentType != AgentType {
			return sidecarPrompt
		}
	}
	return rootPrompt
}

// AgentType is the registry name (shared via config so the server can request it
// without importing this package).
const AgentType = config.ChatbotAgentType

func init() {
	agent.Register(AgentType, factory, agent.Traits{Interactive: true})
	agent.RegisterTools(AgentType, toolset.Defs)
}

func factory(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
	cwd := env.Workspace.Root()

	var ag *eventloop.EventLoopAgent

	// Same toolset as the standard agent — literally, now: a chatbot can work
	// directly or delegate to spawned sub-agents, and two hand-maintained copies
	// of one list drift.
	tools := toolset.Build(toolset.Deps{
		RunID:       env.RunID,
		SessionID:   cfg.SessionID,
		Cwd:         cwd,
		ArtifactDir: config.ArtifactDir(env.RunID),
		Store:       env.Store,
		Registry:    env.Registry,
		Env:         env,
		Handle:      func() *session.Handle { return ag.Handle() },
		SkillIndex:  env.SkillIndex,
		LessonIndex: env.LessonIndex,
	})

	// Operator-selected briefings, appended last so they read as additions to the
	// harness's own guidance rather than interleaved with it. A run's root agent
	// also receives scope:root briefings; sub-agents do not.
	briefings := briefing.ForRun(context.Background(), env.Store, env.RunID, cfg.ParentID == "")

	ag = eventloop.New(env, eventloop.Config{
		SessionID:    cfg.SessionID,
		Task:         cfg.Task,
		FirstMessage: cfg.FirstMessage,
		ParentID:     cfg.ParentID,
		Handle:       cfg.Handle,
		AgentType:    AgentType,
		SystemPrompt: systemPromptFor(env, cfg.SessionID) +
			eventloop.EnvironmentPromptSnippet() +
			eventloop.ToolUsageStrategyPromptSnippet +
			eventloop.SubAgentStrategyPromptSnippet +
			bash.ArtifactDirPromptSnippet +
			briefings,
		Tools:       tools,
		Interactive: true,
		CLITools:    cli.DefaultTools(),
		BlobStore:   blob.NewStore(config.BlobDir(env.RunID)),
	})

	return ag, nil
}
