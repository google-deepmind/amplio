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

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"amplio/internal/config"
	"amplio/internal/db/sqlite"
	"amplio/internal/embed/local"
)

// [skills].pinned marks matching dirs (compared after cleaning, so a trailing
// slash still matches) and reports the entries that name no configured dir.
func TestSkillSources_Pinned(t *testing.T) {
	sources, unknown := skillSources(
		[]string{"/team/skills", "/home/me/mine"},
		config.SkillsConfig{Pinned: []string{"/home/me/mine/", "/home/me/typo"}, Blocked: []string{"noisy"}},
	)
	if len(sources) != 2 || sources[0].Pinned || !sources[1].Pinned {
		t.Fatalf("sources = %+v, want only /home/me/mine pinned", sources)
	}
	if !slices.Equal(sources[0].Blocked, []string{"noisy"}) || !slices.Equal(sources[1].Blocked, []string{"noisy"}) {
		t.Errorf("the blocklist must still apply to every source: %+v", sources)
	}
	if !slices.Equal(unknown, []string{"/home/me/typo"}) {
		t.Errorf("unknown pinned = %v, want [/home/me/typo]", unknown)
	}
}

// "local" is the built-in embedder, or BM25 only when this binary was built
// without its table; either way it is never lent.
func TestResolveRecallEmbedder_Local(t *testing.T) {
	r := resolveRecallEmbedder(context.Background(), config.EmbedModelLocal)
	if !r.local || r.warning != "" {
		t.Fatalf("resolve(local) = %+v, want local with no warning", r)
	}
	if local.Available() {
		if r.embedder == nil || r.mode != "local: potion-base-32M" {
			t.Errorf("table compiled in: embedder=%v mode=%q", r.embedder, r.mode)
		}
	} else if r.embedder != nil || !strings.Contains(r.mode, "BM25 only") {
		t.Errorf("no table: embedder=%v mode=%q, want nil and BM25 only", r.embedder, r.mode)
	}
	if got := r.detail("12 skills"); !strings.HasPrefix(got, "local") || !strings.HasSuffix(got, ", 12 skills") {
		t.Errorf("detail = %q", got)
	}
}

// An API embedder that cannot be created falls back to local, loudly, instead
// of switching recall off.
func TestResolveRecallEmbedder_FallbackOnCreateError(t *testing.T) {
	t.Setenv("VERTEXAI_PROJECT", "")
	for _, spec := range []string{"vertex:text-embedding-005", "nope:model"} {
		r := resolveRecallEmbedder(context.Background(), spec)
		if !r.local || !strings.Contains(r.warning, spec) || !strings.Contains(r.warning, "unavailable") {
			t.Errorf("resolve(%q) = %+v, want a local fallback with a warning naming the spec", spec, r)
		}
	}
}

// A working API embedder is used as-is: cosine ranking, no mode label, lent.
func TestResolveRecallEmbedder_API(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	r := resolveRecallEmbedder(context.Background(), "openai{base_url="+srv.URL+"}:nomic-embed-text")
	if r.embedder == nil || r.local || r.mode != "" || r.warning != "" {
		t.Errorf("resolve(openai) = %+v, want the API embedder unchanged", r)
	}
	if got := r.detail("12 skills"); got != "12 skills" {
		t.Errorf("API detail = %q, want the count alone (unchanged status line)", got)
	}
}

func TestSetupRecall_Modes(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := buildManager(store)
	t.Setenv("VERTEXAI_PROJECT", "")
	cfg := config.Config{Skills: config.SkillsConfig{Dirs: []string{}}}

	cfg.EmbedModel = config.EmbedModelNone
	if s, l, e := setupRecall(context.Background(), mgr, store, cfg); s != nil || l != nil || e != nil {
		t.Errorf("embed_model=none: got %v %v %v, want recall off", s, l, e)
	}
	for _, spec := range []string{config.EmbedModelLocal, "vertex:text-embedding-005"} {
		cfg.EmbedModel = spec
		_, l, lend := setupRecall(context.Background(), mgr, store, cfg)
		if l == nil || !l.IsBuilt() {
			t.Errorf("embed_model=%s: lesson recall not up", spec)
		}
		if lend != nil {
			t.Errorf("embed_model=%s: the local embedder must not be lent", spec)
		}
	}
}

func TestPrintRecallStatus_LocalModes(t *testing.T) {
	var b bytes.Buffer
	printRecallStatus(&b, recallStatus{
		embedModel: "vertex:text-embedding-005",
		warning:    "embed model vertex:text-embedding-005 unavailable (no creds); using the built-in local embedder instead",
		skills:     recallEnabled("local: potion-base-32M, 873 skills"),
		knowledge:  recallEnabled("local: potion-base-32M"),
	})
	for _, want := range []string{
		"Recall subsystem (embed model vertex:text-embedding-005):",
		"! embed model vertex:text-embedding-005 unavailable",
		"skills    available (local: potion-base-32M, 873 skills)",
		"knowledge available (local: potion-base-32M)",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, b.String())
		}
	}
}
