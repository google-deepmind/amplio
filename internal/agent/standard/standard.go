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

package standard

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
	"amplio/internal/tool/recall"
)

//go:embed standard_agent.md
var systemPrompt string

// completionSnippet ends the assembled prompt: how an autonomous agent concludes.
const completionSnippet = "\n\n## Completion\n\n" +
	"When the overarching task is completely finished, give your final summary in " +
	"plain text with NO tool calls — the absence of tool calls signals completion. " +
	"Be concise and direct about the final state."

const AgentType = "standard_agent"

func init() {
	agent.Register(AgentType, factory, agent.Traits{})
	agent.RegisterTools(AgentType, toolset.Defs)
}

func factory(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
	cwd := env.Workspace.Root()

	// Use a pointer so the closure can reference it after assignment.
	var ag *eventloop.EventLoopAgent

	artifactDir := config.ArtifactDir(env.RunID)
	// Operator-selected briefings, appended last so they read as additions to the
	// harness's own guidance rather than interleaved with it. A run's root agent
	// also receives scope:root briefings; sub-agents do not.
	briefings := briefing.ForRun(context.Background(), env.Store, env.RunID, cfg.ParentID == "")

	tools := toolset.Build(toolset.Deps{
		RunID:       env.RunID,
		SessionID:   cfg.SessionID,
		Cwd:         cwd,
		ArtifactDir: artifactDir,
		Store:       env.Store,
		Registry:    env.Registry,
		Env:         env,
		Handle:      func() *session.Handle { return ag.Handle() },
		SkillIndex:  env.SkillIndex,
		LessonIndex: env.LessonIndex,
	})
	// Task-relevant seeding over whichever corpora are built. Gated on the same
	// condition as the recall TOOLS (see toolset.Build): the index objects
	// existing, not their being built yet.
	var initialRecall func(ctx context.Context, task string) string
	if env.SkillIndex != nil || env.LessonIndex != nil {
		sIx, lIx := env.SkillIndex, env.LessonIndex
		initialRecall = func(ctx context.Context, task string) string {
			return recall.InitialContent(ctx, sIx, lIx, task)
		}
	}

	ag = eventloop.New(env, eventloop.Config{
		SessionID:    cfg.SessionID,
		Task:         cfg.Task,
		FirstMessage: cfg.FirstMessage,
		ParentID:     cfg.ParentID,
		Handle:       cfg.Handle,
		AgentType:    AgentType,
		SystemPrompt: systemPrompt +
			eventloop.ExecutionPrinciplesPromptSnippet +
			eventloop.EnvironmentPromptSnippet() +
			eventloop.ToolUsageStrategyPromptSnippet +
			eventloop.SubAgentStrategyPromptSnippet +
			completionSnippet +
			bash.ArtifactDirPromptSnippet +
			briefings,
		Tools:         tools,
		InitialRecall: initialRecall,
		CLITools:      cli.DefaultTools(),
		BlobStore:     blob.NewStore(config.BlobDir(env.RunID)),
	})

	return ag, nil
}
