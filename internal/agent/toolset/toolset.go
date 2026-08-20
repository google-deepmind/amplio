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

// Package toolset builds the working toolset shared by the standard agent and
// the chatbot — the two have always had the same tools, previously as two
// copies of one list.
//
// It exists to be called TWICE with different fullness: once by a factory with
// real deps, to build tools that run; and once with none of them, to describe
// the same tools for display. One list, so the trajectory viewer cannot drift
// from what an agent actually gets — a second list maintained by hand would be
// wrong within a release.
package toolset

import (
	"amplio/internal/agent"
	"amplio/internal/agent/critic"
	"amplio/internal/db"
	"amplio/internal/lessons"
	"amplio/internal/llm"
	"amplio/internal/session"
	"amplio/internal/skills"
	"amplio/internal/tool"
	"amplio/internal/tool/bash"
	"amplio/internal/tool/coordination"
	"amplio/internal/tool/editfile"
	"amplio/internal/tool/inspect"
	"amplio/internal/tool/recall"
	"amplio/internal/tool/sessionsearch"
	"amplio/internal/tool/spawn"
	"amplio/internal/tool/viewfile"
)

// Deps is everything the toolset can use. A display-only caller leaves the
// runtime fields nil: every constructor here captures its deps in a closure
// rather than dereferencing them, so the tools build fine and only their
// SCHEMAS are meaningful. Nothing in this package may start reading a dep at
// construction time without breaking that.
type Deps struct {
	RunID       string
	SessionID   string
	Cwd         string
	ArtifactDir string

	Store    db.Store          // nil for a display listing
	Registry *session.Registry // nil for a display listing
	Env      *agent.Env        // nil for a display listing (spawn captures it)
	Handle   func() *session.Handle

	SkillIndex  *skills.Index
	LessonIndex *lessons.Index
}

// Build returns the tools in the order an agent receives them.
func Build(d Deps) []*tool.Tool {
	coordDeps := &coordination.Deps{Store: d.Store, RunID: d.RunID, Registry: d.Registry}
	inspectDeps := &inspect.Deps{Store: d.Store, RunID: d.RunID}

	tools := []*tool.Tool{
		bash.New(d.Cwd, d.RunID, d.SessionID),
		viewfile.New(d.Cwd, d.ArtifactDir),
		editfile.New(d.Cwd, d.ArtifactDir),
		coordination.SendMessage(coordDeps, d.SessionID),
		coordination.SessionCancel(coordDeps),
		coordination.AwaitEvent(coordDeps, d.SessionID, d.Handle),
		inspect.SessionList(inspectDeps),
		inspect.SessionSteps(inspectDeps),
		inspect.SessionPeek(inspectDeps),
		inspect.SessionSummary(inspectDeps),
		critic.ViewRunReport(d.Store, d.RunID),
		sessionsearch.New(d.Store, d.RunID),
		spawn.New(d.Env, d.SessionID),
	}
	// Recall rides on the index OBJECTS existing, not on them being built: the
	// skill index builds in a background goroutine that may finish after an agent
	// is constructed, and the tools gate per-corpus with IsBuilt at call time, so
	// an unbuilt index just skips that corpus until it is ready.
	if d.SkillIndex != nil || d.LessonIndex != nil {
		tools = append(tools, recall.Search(d.SkillIndex, d.LessonIndex), recall.Load(d.SkillIndex, d.LessonIndex))
	}
	return tools
}

// Defs describes the toolset for display. Registered as the tool lister for
// both agent types; see agent.ToolLister for why this is a live view rather
// than a record of what a past session had.
func Defs(tc agent.ToolContext) []llm.ToolDef {
	tools := Build(Deps{
		RunID:       tc.RunID,
		SessionID:   tc.SessionID,
		Cwd:         tc.Cwd,
		ArtifactDir: tc.ArtifactDir,
		SkillIndex:  tc.SkillIndex,
		LessonIndex: tc.LessonIndex,
	})
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, t.Def())
	}
	return defs
}
