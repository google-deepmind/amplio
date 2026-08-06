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

// Package briefing holds the operator-selected prompt context a run's agents
// receive up front.
//
// It sits between two things that already exist. A SKILL is pulled on demand by
// the agent (recall_load) and lasts one call. AGENTS.md is operator-authored
// standing text, per data dir or workspace, delivered as a step-0 event. A
// BRIEFING is chosen per run by the operator and appended to the system prompt:
// an API manual, a workflow, a piece of advice.
//
// It is deliberately NOT the harness's own prompt sections (tool usage, artifact
// dir, sub-agent strategy, the corp environment guidance). Those stay
// unconditional string constants in the agent packages — a briefing is the
// selectable kind only.
package briefing

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Scope decides which agents in a run receive a briefing. Most carry technical
// or workflow information that reads the same to any agent, hence the default.
type Scope string

const (
	ScopeAll  Scope = "all"  // every agent in the run, sub-agents included
	ScopeRoot Scope = "root" // the run's root agent only
)

// Source records where a briefing came from, for display and for the override
// warning. Not part of its identity.
type Source string

const (
	SourceBuiltIn Source = "built-in"
	SourceUser    Source = "user"
)

// Briefing is one parsed markdown file. Name is the id: it is stored in a run's
// config, so it must stay stable across file renames and moves — which is why
// it comes from the front matter and never from the path.
type Briefing struct {
	Name        string
	Description string
	Scope       Scope
	Body        string
	Source      Source
	Path        string // origin, for diagnostics
}

var (
	mu       sync.RWMutex
	registry = map[string]Briefing{}
)

// Register adds a built-in briefing. Called from init() — including the
// build-tagged files that carry corp-only briefings, which simply do not exist
// in an OSS binary. Panics on a duplicate name, since that is a build-time bug.
func Register(b Briefing) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[b.Name]; dup {
		panic(fmt.Sprintf("briefing %q registered twice", b.Name))
	}
	b.Source = SourceBuiltIn
	if b.Scope == "" {
		b.Scope = ScopeAll
	}
	registry[b.Name] = b
}

// LoadDir scans <root> recursively for *.md and adds what it finds, so an
// operator can add a briefing without rebuilding. A user briefing OVERRIDES a
// built-in of the same name (loudly): replacing shipped text is a legitimate
// thing to want. Within the directory the first file wins by sorted path, which
// matches how duplicate skills are resolved.
//
// A missing directory is normal, not an error.
func LoadDir(root string) {
	entries, err := collect(root)
	if err != nil {
		slog.Warn("briefings: directory unreadable; ignoring", "path", root, "error", err)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]string{}
	for _, b := range entries {
		if prev, dup := seen[b.Name]; dup {
			slog.Warn("briefings: duplicate name; keeping first", "name", b.Name, "kept", prev, "dropped", b.Path)
			continue
		}
		seen[b.Name] = b.Path
		if old, exists := registry[b.Name]; exists && old.Source == SourceBuiltIn {
			slog.Warn("briefings: user file overrides a built-in", "name", b.Name, "path", b.Path)
		}
		registry[b.Name] = b
	}
}

func collect(root string) ([]Briefing, error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil // no briefings dir: the normal case
	}
	var out []Briefing
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil //nolint:nilerr // one unreadable entry must not stop the scan
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("briefings: unreadable file", "path", path, "error", err)
			return nil
		}
		b, ok := Parse(path, raw)
		if !ok {
			return nil
		}
		b.Source = SourceUser
		out = append(out, b)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// Parse reads one briefing file. A malformation is a WARN and a skip: one bad
// file must not stop the rest from loading (the rule scanSkills follows).
func Parse(path string, raw []byte) (Briefing, bool) {
	const delim = "---"
	text := string(raw)
	if !strings.HasPrefix(text, delim+"\n") && !strings.HasPrefix(text, delim+"\r\n") {
		slog.Warn("briefing: no YAML frontmatter at top", "path", path)
		return Briefing{}, false
	}
	_, rest, _ := strings.Cut(text, "\n")
	end := strings.Index(rest, "\n"+delim)
	if end < 0 {
		slog.Warn("briefing: unterminated YAML frontmatter", "path", path)
		return Briefing{}, false
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Scope       string `yaml:"scope"`
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		slog.Warn("briefing: bad YAML frontmatter", "path", path, "error", err)
		return Briefing{}, false
	}
	if fm.Name == "" {
		slog.Warn("briefing: frontmatter has no name", "path", path)
		return Briefing{}, false
	}
	scope := Scope(fm.Scope)
	switch scope {
	case "":
		scope = ScopeAll
	case ScopeAll, ScopeRoot:
	default:
		slog.Warn("briefing: unknown scope; treating as all", "path", path, "scope", fm.Scope)
		scope = ScopeAll
	}
	body := strings.TrimLeft(rest[end+len("\n"+delim):], "\r\n")
	return Briefing{Name: fm.Name, Description: fm.Description, Scope: scope, Body: body, Path: path}, true
}

// List returns every known briefing, ordered by name.
func List() []Briefing {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Briefing, 0, len(registry))
	for _, b := range registry {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve validates a caller's selection into the run's briefing list, once, at
// run creation. Briefings are opt-in per run: no selection means none, so there
// is nothing to interpret later.
//
// Unknown names are dropped and returned separately: a typo should not fail run
// creation, but it must not pass silently either.
func Resolve(selected []string) (names []string, unknown []string) {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[string]bool{}
	for _, n := range selected {
		if seen[n] {
			continue
		}
		seen[n] = true
		if _, ok := registry[n]; ok {
			names = append(names, n)
		} else {
			unknown = append(unknown, n)
		}
	}
	sort.Strings(names)
	return names, unknown
}

// Compose returns the text to append to an agent's system prompt: the named
// briefings, filtered by scope, in LIBRARY order rather than selection order —
// stable ordering keeps a provider's cached prefix stable and settles any
// argument about precedence.
//
// Names that no longer resolve (a deleted user file) are reported, not fatal:
// the agent runs without that section rather than failing to start.
func Compose(names []string, isRoot bool) (text string, missing []string) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var b strings.Builder
	for _, br := range List() { // List() is name-ordered
		if !want[br.Name] {
			continue
		}
		delete(want, br.Name)
		if br.Scope == ScopeRoot && !isRoot {
			continue
		}
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(br.Body, "\n"))
	}
	for n := range want {
		missing = append(missing, n)
	}
	sort.Strings(missing)
	return b.String(), missing
}
