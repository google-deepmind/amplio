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
	"testing"

	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/embed"
)

func newStore(t *testing.T) db.Store {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedLesson inserts a lesson with an embedding computed from desc by emb, then
// applies the score counter.
func seedLesson(t *testing.T, store db.Store, emb embed.Embedder, id, desc string, score int) {
	t.Helper()
	ctx := context.Background()
	vecs, err := emb.Embed(ctx, []string{desc})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertLesson(ctx, db.LessonRecord{
		LessonID: id, Title: id, Description: desc, Body: "body of " + id,
		Embedding: vecs[0], EmbedderID: emb.ModelID(),
	}); err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		if err := store.AddToLessonScore(ctx, id, score); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndex_CosineRanking(t *testing.T) {
	store := newStore(t)
	emb := embed.Mock{Dim: 4096}
	seedLesson(t, store, emb, "apple", "apple apple apple", 0)
	seedLesson(t, store, emb, "banana", "banana banana banana", 0)

	ix := NewIndex(store, emb)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ix.Size() != 2 {
		t.Fatalf("size = %d, want 2", ix.Size())
	}

	hits, err := ix.Search(context.Background(), "apple", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Lesson.LessonID != "apple" {
		t.Fatalf("hits = %+v, want apple first", hits)
	}
	if hits[0].Cosine <= hits[1].Cosine {
		t.Fatalf("apple cosine %v should exceed banana %v", hits[0].Cosine, hits[1].Cosine)
	}
}

func TestIndex_ConfidenceRanking(t *testing.T) {
	store := newStore(t)
	emb := embed.Mock{Dim: 4096}
	// Identical descriptions → identical cosine; the higher-score lesson wins on
	// the confidence multiplier.
	seedLesson(t, store, emb, "low", "alpha beta gamma", 0)
	seedLesson(t, store, emb, "high", "alpha beta gamma", 5)

	ix := NewIndex(store, emb)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search(context.Background(), "alpha beta gamma", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Lesson.LessonID != "high" {
		t.Fatalf("hits = %+v, want high-score first", hits)
	}
	if hits[0].Cosine != hits[1].Cosine {
		t.Fatalf("identical descriptions should share cosine: %v vs %v", hits[0].Cosine, hits[1].Cosine)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("high-score lesson should rank higher: %v vs %v", hits[0].Score, hits[1].Score)
	}
}

func TestConfidenceAdjustment(t *testing.T) {
	if a := confidenceAdjustment(0, 0); a != 0 {
		t.Errorf("neutral lesson = %v, want 0 (ranked by cosine alone)", a)
	}
	// A positive score lifts; more loads damp the same score.
	few, many := confidenceAdjustment(4, 0), confidenceAdjustment(4, 96)
	if !(few > many && many > 0) {
		t.Errorf("load damping: few-loads=%v should exceed many-loads=%v > 0", few, many)
	}
	// Bounded both ways: the bonus never exceeds confidenceBonus, the penalty never
	// exceeds confidencePenalty, however extreme the score.
	if a := confidenceAdjustment(1000, 0); a <= 0 || a > confidenceBonus {
		t.Errorf("max bonus = %v, want in (0, %v]", a, confidenceBonus)
	}
	if a := confidenceAdjustment(-1000, 0); a >= 0 || a < -confidencePenalty {
		t.Errorf("max penalty = %v, want in [-%v, 0)", a, confidencePenalty)
	}
	// Asymmetric: the same evidence counts for more when it is negative.
	if up, down := confidenceAdjustment(1, 0), confidenceAdjustment(-1, 0); -down <= up {
		t.Errorf("penalty %v should outweigh bonus %v for the same |score|", down, up)
	}
}

// seedVec inserts a lesson with an explicit embedding, then applies its score.
// Lets a test place lessons at chosen cosines to a query.
func seedVec(t *testing.T, store db.Store, id string, v []float32, score int) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertLesson(ctx, db.LessonRecord{
		LessonID: id, Title: id, Description: id, Body: "body of " + id,
		Embedding: v, EmbedderID: embed.Mock{}.ModelID(),
	}); err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		if err := store.AddToLessonScore(ctx, id, score); err != nil {
			t.Fatal(err)
		}
	}
}

// atCos is a unit vector whose cosine to the query axis (1,0,0) is c.
func atCos(c float64) []float32 {
	return []float32{float32(c), float32(math.Sqrt(1 - c*c)), 0}
}

var queryAxis = []float32{1, 0, 0}

func buildIndex(t *testing.T, store db.Store) *Index {
	t.Helper()
	ix := NewIndex(store, embed.Mock{})
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ix
}

func ids(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Lesson.LessonID
	}
	return out
}

// A track record reorders only lessons that are about equally relevant: the most
// highly rated lesson cannot overtake one that is clearly more relevant. (Under the
// multiplicative rule this replaced, 0.60 x (1 + 8/2) = 3.0 beat 0.70 x 1.)
func TestIndex_ConfidenceCannotOverrideRelevance(t *testing.T) {
	store := newStore(t)
	seedVec(t, store, "relevant", atCos(0.70), 0)
	seedVec(t, store, "well-rated", atCos(0.60), 8)
	if got := ids(buildIndex(t, store).SearchVec(queryAxis, 2)); got[0] != "relevant" {
		t.Fatalf("ranking = %v, want the more relevant lesson first", got)
	}
}

// ...but among near-ties a positive track record wins.
func TestIndex_ConfidenceBreaksNearTies(t *testing.T) {
	store := newStore(t)
	seedVec(t, store, "neutral", atCos(0.700), 0)
	seedVec(t, store, "helpful-once", atCos(0.695), 1)
	if got := ids(buildIndex(t, store).SearchVec(queryAxis, 2)); got[0] != "helpful-once" {
		t.Fatalf("ranking = %v, want the rated lesson to win a near-tie", got)
	}
}

// A lesson judged harmful drops below clearly less relevant neutral ones.
func TestIndex_PenaltyDemotes(t *testing.T) {
	store := newStore(t)
	seedVec(t, store, "harmful", atCos(0.70), -3)
	seedVec(t, store, "neutral", atCos(0.62), 0)
	if got := ids(buildIndex(t, store).SearchVec(queryAxis, 2)); got[0] != "neutral" {
		t.Fatalf("ranking = %v, want the harmful lesson demoted", got)
	}
}

// Nearest answers "which lesson is closest?" by raw cosine, even when the
// confidence-adjusted ranking would put a slightly less similar, well-rated lesson
// first. Mining relies on this for its near-duplicate check.
func TestIndex_NearestIgnoresConfidence(t *testing.T) {
	store := newStore(t)
	seedVec(t, store, "closest", atCos(0.95), 0)
	seedVec(t, store, "well-rated", atCos(0.94), 8)
	ix := buildIndex(t, store)
	if top := ids(ix.SearchVec(queryAxis, 1)); top[0] != "well-rated" {
		t.Fatalf("precondition: SearchVec top = %v, want the well-rated near-tie", top)
	}
	hit, ok := ix.Nearest(queryAxis)
	if !ok || hit.Lesson.LessonID != "closest" || math.Abs(hit.Cosine-0.95) > 1e-6 {
		t.Fatalf("Nearest = %+v (ok=%v), want lesson \"closest\" at cosine 0.95", hit, ok)
	}
	if _, ok := NewIndex(newStore(t), embed.Mock{}).Nearest(queryAxis); ok {
		t.Error("Nearest on an empty index should report ok=false")
	}
}

func TestIndex_LoadAndSkipMissingEmbedding(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	emb := embed.Mock{Dim: 4096}
	seedLesson(t, store, emb, "good", "hello world", 0)
	// A lesson with no embedding is skipped from the index.
	if err := store.InsertLesson(ctx, db.LessonRecord{LessonID: "noembed", Title: "x", Description: "y"}); err != nil {
		t.Fatal(err)
	}

	ix := NewIndex(store, emb)
	if err := ix.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if ix.Size() != 1 {
		t.Fatalf("size = %d, want 1 (noembed skipped)", ix.Size())
	}
	if _, ok := ix.Load("good"); !ok {
		t.Fatal("good not loadable")
	}
	if _, ok := ix.Load("noembed"); ok {
		t.Fatal("noembed should be absent")
	}
}

// TestIndex_SkipsEmptyEmbedderID guards Build's stricter "EmbedderID must
// exactly match the current embedder's ModelID" rule. A row that lost its
// embedder_id (older builds, cross-system import) is admitted by neither
// fresh-embedded queries nor dim-mismatched matrices; skipping it keeps the
// corpus clean until re-mining repopulates the field.
func TestIndex_SkipsEmptyEmbedderID(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	emb := embed.Mock{Dim: 4096}
	seedLesson(t, store, emb, "tagged", "alpha beta", 0)
	// Same vector dimensions, but blank embedder_id.
	vecs, err := emb.Embed(ctx, []string{"alpha beta"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertLesson(ctx, db.LessonRecord{
		LessonID: "untagged", Title: "x", Description: "y",
		Embedding: vecs[0], EmbedderID: "", // intentionally blank
	}); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(store, emb)
	if err := ix.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if ix.Size() != 1 {
		t.Fatalf("size = %d, want 1 (empty-EmbedderID row skipped)", ix.Size())
	}
	if _, ok := ix.Load("untagged"); ok {
		t.Fatal("untagged row should be skipped on empty EmbedderID")
	}
}

// TestIndex_DegenerateQueryReturnsEmpty guards the search-side bug the audit
// surfaced: a query that embeds to a zero vector (here: the empty string,
// which Mock.Embed deterministically maps to all-zeros) used to score every
// row at 0 and let an unstable sort pick arbitrary winners.
func TestIndex_DegenerateQueryReturnsEmpty(t *testing.T) {
	store := newStore(t)
	emb := embed.Mock{Dim: 4096}
	seedLesson(t, store, emb, "apple", "apple apple apple", 0)
	seedLesson(t, store, emb, "banana", "banana banana banana", 0)
	ix := NewIndex(store, emb)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Whitespace-only query: Mock.Embed → all-zero vector → SearchVec must bail.
	hits, err := ix.Search(context.Background(), "   ", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("degenerate query produced %d hits, want 0", len(hits))
	}
}
