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

package lessons

import (
	"context"
	"math"
	"slices"
	"testing"

	"amplio/internal/db"
	"amplio/internal/embed"
	"amplio/internal/textrank"
	"amplio/internal/vec"
)

// insertRaw stores a lesson exactly as given (any embedding, any embedder id).
func insertRaw(t *testing.T, store db.Store, id, desc string, emb []float32, embedderID string, score int) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertLesson(ctx, db.LessonRecord{
		LessonID: id, Title: id, Description: desc, Body: "body of " + id, Embedding: emb, EmbedderID: embedderID,
	}); err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		if err := store.AddToLessonScore(ctx, id, score); err != nil {
			t.Fatal(err)
		}
	}
}

// With the local embedder every lesson is searchable — including ones mined
// under another model or never embedded — because Build embeds descriptions in
// memory; the stored embeddings are left exactly as they were.
func TestIndex_LocalModeEmbedsEveryLesson(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	insertRaw(t, store, "from-api", "retry flaky integration tests", []float32{0.1, 0.2, 0.3}, "vertex_text-embedding-005", 0)
	insertRaw(t, store, "unembedded", "profile memory of a binary", nil, "", 0)
	insertRaw(t, store, "other", "send email notifications", nil, "", 0)

	ix := NewIndex(store, embed.HybridMock{})
	if err := ix.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if ix.Size() != 3 {
		t.Fatalf("size = %d, want 3 (every lesson)", ix.Size())
	}
	hits, err := ix.Search(ctx, "flaky tests", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Lesson.LessonID != "from-api" {
		t.Fatalf("hits = %v, want the lesson mined under the API model", ids(hits))
	}
	l, err := store.GetLesson(ctx, "from-api")
	if err != nil {
		t.Fatal(err)
	}
	if l.EmbedderID != "vertex_text-embedding-005" || !slices.Equal(l.Embedding, []float32{0.1, 0.2, 0.3}) {
		t.Errorf("stored embedding rewritten: %v %q", l.Embedding, l.EmbedderID)
	}
	if _, ok := ix.Nearest(mustEmbed(t, "retry flaky integration tests")); !ok {
		t.Error("Nearest must work on the in-memory local vectors")
	}
}

func mustEmbed(t *testing.T, s string) []float32 {
	t.Helper()
	v, err := embed.HybridMock{}.Embed(context.Background(), []string{s})
	if err != nil {
		t.Fatal(err)
	}
	return v[0]
}

// A local-mode hit scores the fused cosine + BM25 value plus the confidence
// adjustment; Cosine stays the raw cosine.
func TestIndex_LocalModeScore(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	descs := []string{"retry flaky integration tests", "profile memory of a binary", "flaky network retries"}
	scores := []int{0, 0, 3}
	for i, d := range descs {
		insertRaw(t, store, d, d, nil, "", scores[i])
	}
	ix := NewIndex(store, embed.HybridMock{})
	if err := ix.Build(ctx); err != nil {
		t.Fatal(err)
	}
	const query = "flaky retries"
	cos := make([]float64, len(descs))
	for i, d := range descs {
		cos[i] = vec.Dot(vec.Normalize(mustEmbed(t, d)), vec.Normalize(mustEmbed(t, query)))
	}
	fused := textrank.Fuse(cos, textrank.NewBM25(descs).Scores(query), embed.HybridMockAlpha)

	hits, err := ix.Search(ctx, query, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	for _, h := range hits {
		i := slices.Index(descs, h.Lesson.LessonID)
		want := fused[i] + confidenceAdjustment(scores[i], 0)
		if math.Abs(h.Score-want) > 1e-12 || math.Abs(h.Cosine-cos[i]) > 1e-12 {
			t.Errorf("%s: score %v cosine %v, want %v and %v", h.Lesson.LessonID, h.Score, h.Cosine, want, cos[i])
		}
	}
}

// The confidence bounds keep their meaning on the fused [0, 1] scale: a bonus
// breaks a tie, and a harmful verdict drops a lesson below a slightly less
// relevant neutral one.
func TestIndex_ConfidenceOnFusedScore(t *testing.T) {
	for name, emb := range map[string]embed.Embedder{"local": embed.HybridMock{}, "lexical": nil} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			insertRaw(t, store, "neutral", "rotate service credentials", nil, "", 0)
			insertRaw(t, store, "helpful", "rotate service credentials", nil, "", 1)
			insertRaw(t, store, "harmful", "rotate service credentials", nil, "", -3)
			insertRaw(t, store, "related", "rotate the service credentials", nil, "", 0)
			insertRaw(t, store, "unrelated", "send email notifications", nil, "", 0)
			ix := NewIndex(store, emb)
			if err := ix.Build(context.Background()); err != nil {
				t.Fatal(err)
			}
			hits, err := ix.Search(context.Background(), "rotate service credentials", 5)
			if err != nil {
				t.Fatal(err)
			}
			got := ids(hits)
			if len(got) < 4 || got[0] != "helpful" {
				t.Fatalf("ranking = %v, want helpful first", got)
			}
			if slices.Index(got, "harmful") < slices.Index(got, "related") {
				t.Errorf("ranking = %v, want the harmful lesson below the slightly less relevant one", got)
			}
		})
	}
}

// Without an embedder: BM25 over every lesson, only matching lessons returned,
// no cosine, and no near-duplicate lookup.
func TestIndex_LexicalMode(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	insertRaw(t, store, "a", "retry flaky integration tests", []float32{1, 0}, "vertex_x", 0)
	insertRaw(t, store, "b", "profile memory of a binary", nil, "", 0)
	ix := NewIndex(store, nil)
	if err := ix.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if ix.Size() != 2 || ix.Embedder() != nil {
		t.Fatalf("size = %d embedder = %v, want 2 and nil", ix.Size(), ix.Embedder())
	}
	hits, err := ix.Search(ctx, "memory profile", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Lesson.LessonID != "b" || hits[0].Cosine != 0 || hits[0].Score != 1 {
		t.Errorf("hits = %+v, want only b, score 1, cosine 0", hits)
	}
	if got, _ := ix.Search(ctx, "kubernetes", 5); got != nil {
		t.Errorf("a query matching nothing returned %v", ids(got))
	}
	if _, ok := ix.Nearest([]float32{1, 0}); ok {
		t.Error("Nearest must report ok=false without embeddings")
	}
}
