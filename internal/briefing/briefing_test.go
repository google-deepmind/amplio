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

package briefing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapRegistry gives one test an isolated library.
func swapRegistry(t *testing.T, entries ...Briefing) {
	t.Helper()
	mu.Lock()
	prev := registry
	registry = map[string]Briefing{}
	for _, b := range entries {
		if b.Scope == "" {
			b.Scope = ScopeAll
		}
		registry[b.Name] = b
	}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		registry = prev
		mu.Unlock()
	})
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	b, ok := Parse("x.md", []byte("---\nname: run-manager\ndescription: d\nscope: root\n---\n\n## Body\n\ntext\n"))
	if !ok {
		t.Fatal("parse failed")
	}
	if b.Name != "run-manager" || b.Description != "d" || b.Scope != ScopeRoot {
		t.Errorf("frontmatter = %+v", b)
	}
	if !strings.HasPrefix(b.Body, "## Body") {
		t.Errorf("body = %q, want the markdown after the closing delimiter", b.Body)
	}
}

func TestParse_Defaults(t *testing.T) {
	b, ok := Parse("x.md", []byte("---\nname: n\n---\nbody\n"))
	if !ok {
		t.Fatal("parse failed")
	}
	if b.Scope != ScopeAll {
		t.Errorf("scope = %q, want all by default", b.Scope)
	}
}

func TestParse_Rejects(t *testing.T) {
	for name, src := range map[string]string{
		"no frontmatter": "## just markdown\n",
		"unterminated":   "---\nname: n\nbody\n",
		"no name":        "---\ndescription: d\n---\nbody\n",
		"bad yaml":       "---\nname: [unclosed\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := Parse("x.md", []byte(src)); ok {
				t.Error("want rejection")
			}
		})
	}
}

func TestParse_UnknownScopeFallsBack(t *testing.T) {
	b, ok := Parse("x.md", []byte("---\nname: n\nscope: sideways\n---\nbody\n"))
	if !ok || b.Scope != ScopeAll {
		t.Errorf("scope = %q, want a fallback to all", b.Scope)
	}
}

// Identity is the frontmatter name, never the path: a briefing must survive
// being moved or renamed, because runs store the name.
func TestLoadDir_RecursiveAndPathIndependent(t *testing.T) {
	swapRegistry(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "workflow", "deep", "optimizer_research.md"),
		"---\nname: workflow/optimizer-research\ndescription: d\n---\nbody\n")
	write(t, filepath.Join(dir, "flat.md"), "---\nname: flat\n---\nbody\n")
	write(t, filepath.Join(dir, "notes.txt"), "not a briefing")
	LoadDir(dir)

	got := List()
	if len(got) != 2 {
		t.Fatalf("loaded %d briefings, want 2: %+v", len(got), got)
	}
	if got[0].Name != "flat" || got[1].Name != "workflow/optimizer-research" {
		t.Errorf("names = %q, %q (want name-ordered, path-independent)", got[0].Name, got[1].Name)
	}
	if got[1].Source != SourceUser {
		t.Errorf("source = %q, want user", got[1].Source)
	}
}

func TestLoadDir_MissingDirIsFine(t *testing.T) {
	swapRegistry(t)
	LoadDir(filepath.Join(t.TempDir(), "nope"))
	if len(List()) != 0 {
		t.Error("a missing directory should load nothing, not fail")
	}
}

// A user file replacing a shipped briefing is a legitimate override.
func TestLoadDir_UserOverridesBuiltIn(t *testing.T) {
	swapRegistry(t, Briefing{Name: "run-manager", Body: "shipped", Source: SourceBuiltIn})
	dir := t.TempDir()
	write(t, filepath.Join(dir, "mine.md"), "---\nname: run-manager\n---\nmine\n")
	LoadDir(dir)
	got := List()
	if len(got) != 1 || strings.TrimSpace(got[0].Body) != "mine" || got[0].Source != SourceUser {
		t.Errorf("override failed: %+v", got)
	}
}

func TestResolve(t *testing.T) {
	swapRegistry(t, Briefing{Name: "a"}, Briefing{Name: "b"})
	if got, _ := Resolve(nil); len(got) != 0 {
		t.Errorf("no selection = %v, want none: briefings are opt-in", got)
	}
	if got, _ := Resolve([]string{"b"}); strings.Join(got, ",") != "b" {
		t.Errorf("got %v, want exactly what was asked for", got)
	}
	got, unknown := Resolve([]string{"b", "typo", "b"})
	if strings.Join(got, ",") != "b" {
		t.Errorf("got %v, want the duplicate collapsed", got)
	}
	if strings.Join(unknown, ",") != "typo" {
		t.Errorf("unknown = %v, want the typo reported rather than silently dropped", unknown)
	}
}

func TestCompose_ScopeAndOrder(t *testing.T) {
	swapRegistry(t,
		Briefing{Name: "a-all", Body: "AAA", Scope: ScopeAll},
		Briefing{Name: "b-root", Body: "BBB", Scope: ScopeRoot},
		Briefing{Name: "c-all", Body: "CCC", Scope: ScopeAll},
	)
	// Selection order must not affect output order: library order wins, so the
	// composed prefix is stable across runs that picked the same set.
	root, missing := Compose([]string{"c-all", "b-root", "a-all"}, true)
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if got := strings.Index(root, "AAA"); got > strings.Index(root, "CCC") {
		t.Errorf("composed out of library order: %q", root)
	}
	if !strings.Contains(root, "BBB") {
		t.Error("root agent should receive a scope:root briefing")
	}
	sub, _ := Compose([]string{"c-all", "b-root", "a-all"}, false)
	if strings.Contains(sub, "BBB") {
		t.Error("a sub-agent must not receive a scope:root briefing")
	}
	if !strings.Contains(sub, "AAA") || !strings.Contains(sub, "CCC") {
		t.Error("a sub-agent should receive scope:all briefings")
	}
}

// A name stored on a run whose file has since been deleted must not stop the
// agent from starting.
func TestCompose_MissingNameIsReportedNotFatal(t *testing.T) {
	swapRegistry(t, Briefing{Name: "here", Body: "HERE"})
	text, missing := Compose([]string{"here", "gone"}, true)
	if !strings.Contains(text, "HERE") {
		t.Error("the surviving briefing should still compose")
	}
	if strings.Join(missing, ",") != "gone" {
		t.Errorf("missing = %v, want [gone]", missing)
	}
}

// The shipped briefings must satisfy the same parser an operator's file does.
func TestBuiltinsAreWellFormed(t *testing.T) {
	found := false
	for _, b := range List() {
		if b.Source != SourceBuiltIn {
			continue
		}
		found = true
		if b.Description == "" {
			t.Errorf("built-in %q has no description (the UI lists it)", b.Name)
		}
		if strings.TrimSpace(b.Body) == "" {
			t.Errorf("built-in %q has an empty body", b.Name)
		}
	}
	if !found {
		t.Error("no built-in briefings registered")
	}
}
