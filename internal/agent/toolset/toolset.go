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
	"amplio/internal/tool/conclude"
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

	// Conclusion decides the conclude tool: none for an agent that goes idle.
	Conclusion Conclusion
}

// Conclusion is how an agent type finishes, which decides its conclude tool.
type Conclusion int

const (
	GoesIdle             Conclusion = iota // interactive: no conclude tool
	ConcludesWithResult                    // conclude(result)
	ConcludesWithVerdict                   // a verifier's conclude(verdict, findings)
)

// Build returns the tools in the order an agent receives them.
func Build(d Deps) []*tool.Tool {
	coordDeps := &coordination.Deps{Store: d.Store, RunID: d.RunID, Registry: d.Registry}
	if d.Env != nil {
		coordDeps.VerifyDefaults, coordDeps.ResolveLLM = d.Env.Verify, d.Env.ResolveLLM
	}
	inspectDeps := &inspect.Deps{Store: d.Store, RunID: d.RunID}

	tools := []*tool.Tool{
		bash.New(d.Cwd, d.RunID, d.SessionID),
		viewfile.New(d.Cwd, d.ArtifactDir),
		editfile.New(d.Cwd, d.ArtifactDir),
		coordination.SendMessage(coordDeps, d.SessionID),
		coordination.SessionCancel(coordDeps, d.SessionID),
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
	switch d.Conclusion {
	case GoesIdle: // a turn without tool calls rests it
	case ConcludesWithResult:
		tools = append(tools, conclude.New())
	case ConcludesWithVerdict:
		tools = append(tools, conclude.NewVerdict())
	}
	return tools
}

// Lister describes the toolset for display, as Build gives it to an agent
// type that finishes as c says. Registered as the tool lister for every agent
// type built on this toolset; see agent.ToolLister for why this is a live view
// rather than a record of what a past session had.
func Lister(c Conclusion) agent.ToolLister {
	return func(tc agent.ToolContext) []llm.ToolDef {
		tools := Build(Deps{
			RunID:       tc.RunID,
			SessionID:   tc.SessionID,
			Cwd:         tc.Cwd,
			ArtifactDir: tc.ArtifactDir,
			SkillIndex:  tc.SkillIndex,
			LessonIndex: tc.LessonIndex,
			Conclusion:  c,
		})
		defs := make([]llm.ToolDef, 0, len(tools))
		for _, t := range tools {
			defs = append(defs, t.Def())
		}
		return defs
	}
}
