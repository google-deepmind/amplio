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

package toolset

import (
	"strings"
	"testing"

	"amplio/internal/agent"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/lessons"
	"amplio/internal/session"
	"amplio/internal/skills"
	"amplio/internal/tool"
)

func names(tools []*tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// The whole point of one builder: what the viewer describes must be what an
// agent gets. A tool that appeared only when a Store was present would make the
// display quietly wrong, so the set must not depend on the runtime deps.
func TestBuild_SetIsIndependentOfRuntimeDeps(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	ix := &skills.Index{}
	lx := &lessons.Index{}
	live := Build(Deps{
		RunID: "r1", SessionID: "s1", Cwd: "/w", ArtifactDir: "/a",
		Store: store, Registry: session.NewRegistry(), Env: &agent.Env{Store: store, RunID: "r1"},
		Handle: func() *session.Handle { return nil }, SkillIndex: ix, LessonIndex: lx,
	})
	display := Build(Deps{
		RunID: "r1", SessionID: "s1", Cwd: "/w", ArtifactDir: "/a",
		SkillIndex: ix, LessonIndex: lx,
	})
	if strings.Join(names(live), ",") != strings.Join(names(display), ",") {
		t.Errorf("set differs by deps:\n live=%v\n disp=%v", names(live), names(display))
	}
	if len(live) == 0 {
		t.Fatal("no tools built")
	}
}

// Building with nothing must not panic: every constructor has to capture its
// deps rather than read them. A future tool that dereferences a Store while
// building would break the viewer, and this is where that shows up.
func TestBuild_NilDepsDoNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Build with nil deps panicked: %v — a constructor is dereferencing a dep", r)
		}
	}()
	if got := Build(Deps{Cwd: "/w"}); len(got) == 0 {
		t.Fatal("no tools built")
	}
}

// Recall is the one conditional member, and it rides on the index OBJECTS, not
// on their being built — that asymmetry is easy to "fix" wrongly later.
func TestBuild_RecallGating(t *testing.T) {
	without := names(Build(Deps{Cwd: "/w"}))
	with := names(Build(Deps{Cwd: "/w", SkillIndex: &skills.Index{}}))
	if len(with) != len(without)+2 {
		t.Errorf("with an index: %d tools, without: %d, want two more (search + load)", len(with), len(without))
	}
	for _, n := range without {
		if strings.HasPrefix(n, "recall_") {
			t.Errorf("recall tool %q offered with no corpus", n)
		}
	}
}

// Only an autonomous agent type finishes with conclude; an interactive one
// goes idle, so offering it the tool would end a conversation. A verifier's
// conclude takes a verdict. The display lister agrees with what the factory
// builds.
func TestBuild_ConcludePerConclusion(t *testing.T) {
	concludeParams := func(tools []*tool.Tool) string {
		for _, tl := range tools {
			if tl.Name == event.ConcludeTool {
				return string(tl.Def().Schema)
			}
		}
		return ""
	}
	if p := concludeParams(Build(Deps{Cwd: "/w"})); p != "" {
		t.Error("an interactive agent type is offered conclude")
	}
	if p := concludeParams(Build(Deps{Cwd: "/w", Conclusion: ConcludesWithResult})); !strings.Contains(p, `"result"`) {
		t.Errorf("an autonomous agent type's conclude = %s, want conclude(result)", p)
	}
	if p := concludeParams(Build(Deps{Cwd: "/w", Conclusion: ConcludesWithVerdict})); !strings.Contains(p, `"verdict"`) ||
		strings.Contains(p, `"result"`) {
		t.Errorf("a verifier's conclude = %s, want conclude(verdict, findings)", p)
	}
	tc := agent.ToolContext{Cwd: "/w"}
	listed := func(c Conclusion) string {
		for _, d := range Lister(c)(tc) {
			if d.Name == event.ConcludeTool {
				return string(d.Schema)
			}
		}
		return ""
	}
	if listed(GoesIdle) != "" || !strings.Contains(listed(ConcludesWithResult), `"result"`) ||
		!strings.Contains(listed(ConcludesWithVerdict), `"verdict"`) {
		t.Errorf("listers: idle %q, result %q, verdict %q", listed(GoesIdle), listed(ConcludesWithResult),
			listed(ConcludesWithVerdict))
	}
}

// The lister is what the API serves: every entry needs the three fields a
// viewer renders, or the page shows blanks.
func TestDefs_Complete(t *testing.T) {
	defs := Lister(ConcludesWithResult)(agent.ToolContext{
		RunID: "r1", SessionID: "s1", Cwd: "/work/repo", ArtifactDir: "/a",
		SkillIndex: &skills.Index{},
	})
	if len(defs) == 0 {
		t.Fatal("no defs")
	}
	var bash string
	for _, d := range defs {
		if d.Name == "" {
			t.Error("a def has no name")
		}
		if d.Description == "" {
			t.Errorf("%s has no description", d.Name)
		}
		if len(d.Schema) == 0 || !strings.HasPrefix(string(d.Schema), "{") {
			t.Errorf("%s has no JSON schema: %q", d.Name, string(d.Schema))
		}
		if d.Name == "bash" {
			bash = d.Description
		}
	}
	// The workspace path is the one per-session fact in a description, and the
	// reason the viewer passes a cwd at all.
	if !strings.Contains(bash, "/work/repo") {
		t.Errorf("bash description does not carry the cwd: %q", bash)
	}
}
