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

// Package verifier is the agent the harness starts to check another agent's
// conclusion before it is delivered (internal/verify): the standard agent's
// tools, its own prompt, and a conclude that takes a typed verdict.
package verifier

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
	"amplio/internal/verify"
)

//go:embed verifier.md
var systemPrompt string

func init() {
	agent.Register(verify.AgentType, factory, agent.Traits{Verifier: true})
	agent.RegisterTools(verify.AgentType, toolset.Lister(toolset.ConcludesWithVerdict))
}

func factory(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
	var ag *eventloop.EventLoopAgent
	tools := toolset.Build(toolset.Deps{
		RunID:       env.RunID,
		SessionID:   cfg.SessionID,
		Cwd:         env.Workspace.Root(),
		ArtifactDir: config.ArtifactDir(env.RunID),
		Store:       env.Store,
		Registry:    env.Registry,
		Env:         env,
		Handle:      func() *session.Handle { return ag.Handle() },
		SkillIndex:  env.SkillIndex,
		LessonIndex: env.LessonIndex,
		Conclusion:  toolset.ConcludesWithVerdict,
	})
	ag = eventloop.New(env, eventloop.Config{
		SessionID:   cfg.SessionID,
		Task:        cfg.Task,
		ParentID:    cfg.ParentID,
		Handle:      cfg.Handle,
		VerifierFor: cfg.VerifierFor,
		AgentType:   verify.AgentType,
		SystemPrompt: systemPrompt +
			eventloop.EnvironmentPromptSnippet() +
			eventloop.ToolUsageStrategyPromptSnippet +
			bash.ArtifactDirPromptSnippet +
			briefing.ForRun(context.Background(), env.Store, env.RunID, false),
		Tools:         tools,
		InitialRecall: recall.Seeder(env.SkillIndex, env.LessonIndex),
		CLITools:      cli.DefaultTools(),
		BlobStore:     blob.NewStore(config.BlobDir(env.RunID)),
	})
	return ag, nil
}
