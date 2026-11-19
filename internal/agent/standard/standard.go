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
	"When the overarching task is completely finished, call `conclude` with your final " +
	"result: a concise, self-contained account of the final state; it is what whoever " +
	"asked receives. Call it on its own, once the results of your last actions are in. " +
	"If it reports that background jobs or sub-agents you started are still running, " +
	"decide whether to wait for them, stop them, or leave them, then call `conclude` again.\n\n" +
	"If your conclusion is verified, `conclude` waits for the verifier's verdict: a pass " +
	"delivers your result; a rejection comes back with the findings, to address before " +
	"you conclude again with a new result. Anything else that arrives meanwhile ends " +
	"the wait early: deal with it, then call `conclude()` without a result to keep " +
	"waiting. A verdict that reaches you while you are not waiting arrives as a message " +
	"saying where you stand."

const AgentType = "standard_agent"

func init() {
	agent.Register(AgentType, factory, agent.Traits{})
	agent.RegisterTools(AgentType, toolset.Lister(toolset.ConcludesWithResult))
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
		Conclusion:  toolset.ConcludesWithResult,
	})
	// Session-start seeding over whichever corpora exist (pinned skills plus
	// the task-relevant ones). Gated on the same condition as the recall TOOLS
	// (see toolset.Build): the index objects existing, not their being built yet.
	initialRecall := recall.Seeder(env.SkillIndex, env.LessonIndex)

	ag = eventloop.New(env, eventloop.Config{
		SessionID:    cfg.SessionID,
		Task:         cfg.Task,
		FirstMessage: cfg.FirstMessage,
		ParentID:     cfg.ParentID,
		Handle:       cfg.Handle,
		VerifierFor:  cfg.VerifierFor,
		Verify:       cfg.Verify,
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
