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

package skills

import (
	"context"
	"math"
	"slices"
	"testing"

	"amplio/internal/db/sqlite"
	"amplio/internal/embed"
	"amplio/internal/textrank"
	"amplio/internal/vec"
)

var modeCorpus = []Entry{
	{Name: "spanner", Description: "query spanner sql databases"},
	{Name: "bigquery", Description: "run sql queries over large tables"},
	{Name: "gmail", Description: "read and send email messages"},
	{Name: "bazel", Description: "build run and test code with bazel"},
	{Name: "pprof", Description: "profile cpu and memory of a running binary"},
}

func buildModeIndex(t *testing.T, emb embed.Embedder) (*Index, Cache) {
	t.Helper()
	dir := t.TempDir()
	for _, e := range modeCorpus {
		writeSkill(t, dir, e.Name, e.Description, "# "+e.Name)
	}
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := NewDBCache(store)
	ix := NewIndex([]Source{{Name: "test", Path: dir}}, emb, cache)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ix, cache
}

// With the local embedder, a hit's score is exactly the per-query min-max
// fusion of its cosine and BM25 scores, and hits come in score order.
func TestIndex_HybridRanksByFusion(t *testing.T) {
	ctx := context.Background()
	emb := embed.HybridMock{}
	ix, _ := buildModeIndex(t, emb)
	const query = "run sql over spanner tables"

	docs := make([]string, len(modeCorpus))
	cos := make([]float64, len(modeCorpus))
	qv, _ := emb.Embed(ctx, []string{query})
	for i, e := range modeCorpus {
		docs[i] = embedText(e)
		dv, _ := emb.Embed(ctx, []string{docs[i]})
		cos[i] = vec.Dot(vec.Normalize(dv[0]), vec.Normalize(qv[0]))
	}
	fused := textrank.Fuse(cos, textrank.NewBM25(docs).Scores(query), embed.HybridMockAlpha)
	want := map[string]float64{}
	for i, e := range modeCorpus {
		want[e.Name] = fused[i]
	}

	hits, err := ix.Search(ctx, query, len(modeCorpus))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != len(modeCorpus) {
		t.Fatalf("got %d hits, want every skill (the cosine signal covers all)", len(hits))
	}
	for i, h := range hits {
		if math.Abs(h.Score-want[h.Entry.Name]) > 1e-12 {
			t.Errorf("%s: score %v, want fused %v", h.Entry.Name, h.Score, want[h.Entry.Name])
		}
		if i > 0 && h.Score > hits[i-1].Score {
			t.Errorf("hits out of order: %v", hitNames(hits))
		}
	}
	if top := hits[0].Entry.Name; top != "spanner" && top != "bigquery" {
		t.Errorf("top hit %s, want one of the sql skills", top)
	}
	if got, _ := ix.Search(ctx, "   ", 3); got != nil {
		t.Errorf("a query with no signal returned %v", hitNames(got))
	}
}

// Without an embedder (a binary built without the table) recall ranks by BM25
// alone: no embedding anywhere, only skills sharing a term with the query, and
// the cache still serves a fast startup.
func TestIndex_LexicalMode(t *testing.T) {
	ctx := context.Background()
	ix, cache := buildModeIndex(t, nil)
	if !ix.IsBuilt() || ix.Size() != len(modeCorpus) {
		t.Fatalf("built=%v size=%d", ix.IsBuilt(), ix.Size())
	}
	hits, err := ix.Search(ctx, "SQL databases", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitNames(hits); !slices.Equal(got, []string{"spanner", "bigquery"}) {
		t.Errorf("hits = %v, want [spanner bigquery] (both match sql; spanner also databases)", got)
	}
	if hits[0].Score != 1 || hits[1].Score <= 0 || hits[1].Score >= 1 {
		t.Errorf("scores = %v, %v; want 1 and in (0, 1)", hits[0].Score, hits[1].Score)
	}
	for _, q := range []string{"kubernetes deployment", "the of and", " "} {
		if got, err := ix.Search(ctx, q, 10); err != nil || got != nil {
			t.Errorf("Search(%q) = %v, %v; want nothing", q, hitNames(got), err)
		}
	}

	// A fresh index hydrates from the lexical cache with no scan and searches.
	ix2 := NewIndex([]Source{{Name: "test", Path: t.TempDir()}}, nil, cache)
	if n := ix2.LoadCached(ctx); n != len(modeCorpus) {
		t.Fatalf("LoadCached = %d, want %d", n, len(modeCorpus))
	}
	if got, _ := ix2.Search(ctx, "email", 3); !slices.Equal(hitNames(got), []string{"gmail"}) {
		t.Errorf("search after LoadCached = %v, want [gmail]", hitNames(got))
	}
	// The lexical cache is its own model key: an embedder never reads it.
	ix3 := NewIndex([]Source{{Name: "test", Path: t.TempDir()}}, embed.HybridMock{}, cache)
	if n := ix3.LoadCached(ctx); n != 0 {
		t.Errorf("local-embedder index hydrated %d vector-less entries from the lexical cache", n)
	}
}

// Session-start seeding uses the same mode's scores: pinned skills ordered by
// the fused (or BM25) score, all of them listed even when they match nothing.
func TestIndex_InitialInFusedModes(t *testing.T) {
	for name, emb := range map[string]embed.Embedder{"local": embed.HybridMock{}, "lexical": nil} {
		t.Run(name, func(t *testing.T) {
			lib, mine, cache := pinnedFixture(t)
			ctx := context.Background()
			ix := NewIndex([]Source{{Name: "lib", Path: lib}, {Name: "mine", Path: mine, Pinned: true}}, emb, cache)
			if err := ix.Build(ctx); err != nil {
				t.Fatal(err)
			}
			pinned, relevant, err := ix.Initial(ctx, "deploy to serving", 2)
			if err != nil {
				t.Fatal(err)
			}
			if got := entryNames(pinned); len(got) != 2 || got[0] != "deploy-model" {
				t.Errorf("pinned = %v, want both, deploy-model first", got)
			}
			for _, h := range relevant {
				if h.Entry.Name == "deploy-model" || h.Entry.Name == "tune-model" {
					t.Errorf("relevant lists pinned skill %s", h.Entry.Name)
				}
			}
			pinned, relevant, err = ix.Initial(ctx, "query spanner sql", 1)
			if err != nil || len(pinned) != 2 || !slices.Equal(hitNames(relevant), []string{"spanner"}) {
				t.Errorf("pinned=%v relevant=%v err=%v; want 2 pinned and [spanner]", entryNames(pinned), hitNames(relevant), err)
			}
		})
	}
}
