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
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"

	"amplio/internal/db"
	"amplio/internal/embed"
	"amplio/internal/textrank"
	"amplio/internal/vec"
)

// confidenceK softens the usage normalization in the confidence measure, so a
// high score backed by few loads isn't over-trusted. Module constant, not a
// search parameter.
const confidenceK = 4.0

// confidenceBonus and confidencePenalty bound how far a lesson's track record
// can move it, in cosine units (see confidenceAdjustment). The fused score of
// the local and lexical modes lies in [0, 1], where the same bounds keep their
// meaning: a near-tie break, and dropping out of view.
//
// They are additive and small on purpose. Embedding cosines sit in a narrow
// band (text-embedding-005: a real query's #1 and #5 lessons typically differ
// by ~0.035-0.06, neighbouring ranks by ~0.003), so the multiplicative factor
// this replaced (cosine x (1 + confidence), i.e. 1.5x for a single "helpful"
// verdict) swamped relevance: on real queries the 7% of lessons with a positive
// score took 94% of recall_search's lesson slots and ~100% of the session-start
// ones, whatever the query. A bonus of at most +0.02 only reorders lessons that
// are about equally relevant (it lifts a relevant, well-rated lesson ~1.5 ranks
// and leaves 95-98% of the cosine top-k unchanged). The penalty is larger: a
// lesson judged unhelpful or harmful should drop out of view, not merely slip.
const (
	confidenceBonus   = 0.02
	confidencePenalty = 0.15
)

// Hit is one search result: the lesson, its raw cosine (0 without an
// embedder), and the ranking score (cosine, or the fused cosine + BM25 score,
// plus the bounded confidence adjustment).
type Hit struct {
	Lesson db.LessonRecord
	Cosine float64
	Score  float64
}

// Index is an in-memory index over the Lesson table. Build (re)loads from the
// store; Search queries.
//
// How it ranks follows the embedder (embed.FusionWeight). With an API embedder,
// by cosine against the embeddings lessons store — so Build does no embedding,
// and only rows embedded by the same model are searchable. With the built-in
// local embedder, by cosine fused with BM25, over every lesson: Build embeds
// all descriptions in memory (microseconds each), so lessons mined under
// another model stay searchable, and stored embeddings are never rewritten.
// With no embedder (nil), by BM25 alone, over every lesson.
type Index struct {
	store    db.Store
	embedder embed.Embedder // nil: BM25 only
	alpha    float64        // cosine weight when fused
	fused    bool           // rank by textrank.Fuse rather than cosine alone

	// recallDisabled turns off the agent-facing surface only (see
	// DisableRecall). Set once at startup, before the index is shared.
	recallDisabled bool

	mu      sync.RWMutex
	lessons map[string]db.LessonRecord
	ids     []string       // row order
	matrix  [][]float32    // L2-normalized vectors, row per id (nil without an embedder)
	lexical *textrank.BM25 // over descriptions, row per id (nil unless fused)
	built   bool
}

// NewIndex constructs an index; call Build to populate. A nil embedder ranks
// by BM25 alone.
func NewIndex(store db.Store, embedder embed.Embedder) *Index {
	alpha, fused := embed.FusionWeight(embedder)
	return &Index{store: store, embedder: embedder, alpha: alpha, fused: fused}
}

// DisableRecall marks this index as unavailable to the AGENT-FACING recall
// surface: recall_search stops searching lessons, recall_load refuses a
// lesson: handle, and the run-start seed omits them. Everything else that uses
// the index — end-of-run mining, its near-duplicate check, lesson scoring, and
// the operator's /recall browse page — keeps working, because Search itself is
// untouched. That split is the point: an isolated run must not READ what past
// runs learned, while still CONTRIBUTING what it learns.
//
// Startup only, before the index is handed to the manager: read without
// synchronization on the hot path.
func (ix *Index) DisableRecall() { ix.recallDisabled = true }

// RecallDisabled reports whether the agent-facing recall surface is off.
func (ix *Index) RecallDisabled() bool { return ix.recallDisabled }

// Embedder returns the index's embedder (used by mining to embed candidate
// lessons with the same model the index searches against). nil when the index
// ranks by BM25 alone.
func (ix *Index) Embedder() embed.Embedder { return ix.embedder }

// Build loads all lessons and rebuilds the in-memory index (see Index for the
// ranking modes).
//
// With an API embedder, rows with no embedding, or whose `embedder_id` does not
// exactly match the current embedder's ModelID, are skipped (logged) — they
// can't be compared against fresh query vectors. An empty `embedder_id` (rows
// from older builds or cross-system imports) is therefore also skipped:
// re-mining or re-embedding will populate the field on the next pass.
func (ix *Index) Build(ctx context.Context) error {
	recs, err := ix.store.ListAllLessons(ctx)
	if err != nil {
		return fmt.Errorf("list lessons: %w", err)
	}
	if ix.fused {
		return ix.buildFused(ctx, recs)
	}
	model := ix.embedder.ModelID()
	lessons := make(map[string]db.LessonRecord, len(recs))
	ids := make([]string, 0, len(recs))
	matrix := make([][]float32, 0, len(recs))
	skipped := 0
	for _, r := range recs {
		if len(r.Embedding) == 0 || r.EmbedderID != model {
			skipped++
			continue
		}
		lessons[r.LessonID] = r
		ids = append(ids, r.LessonID)
		matrix = append(matrix, vec.Normalize(r.Embedding))
	}

	ix.mu.Lock()
	ix.lessons, ix.ids, ix.matrix, ix.built = lessons, ids, matrix, true
	ix.mu.Unlock()

	if skipped > 0 {
		slog.Warn("lesson index: skipped rows with missing/mismatched embeddings", "skipped", skipped, "model", model)
	}
	slog.Info("lesson index built", "lessons", len(ids))
	return nil
}

// buildFused indexes every lesson by its description: BM25, plus in-memory
// vectors from the (local) embedder when there is one.
func (ix *Index) buildFused(ctx context.Context, recs []db.LessonRecord) error {
	lessons := make(map[string]db.LessonRecord, len(recs))
	ids := make([]string, 0, len(recs))
	descs := make([]string, 0, len(recs))
	for _, r := range recs {
		lessons[r.LessonID] = r
		ids = append(ids, r.LessonID)
		descs = append(descs, r.Description)
	}
	var matrix [][]float32
	if ix.embedder != nil {
		vecs, err := ix.embedder.Embed(ctx, descs)
		if err != nil {
			return fmt.Errorf("embed lessons: %w", err)
		}
		for _, v := range vecs {
			matrix = append(matrix, vec.Normalize(v))
		}
	}
	lexical := textrank.NewBM25(descs)

	ix.mu.Lock()
	ix.lessons, ix.ids, ix.matrix, ix.lexical, ix.built = lessons, ids, matrix, lexical, true
	ix.mu.Unlock()
	slog.Info("lesson index built", "lessons", len(ids), "embedded_in_memory", len(matrix))
	return nil
}

// IsBuilt reports whether Build has completed.
func (ix *Index) IsBuilt() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.built
}

// Size is the number of indexed lessons.
func (ix *Index) Size() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.ids)
}

// Load returns the lesson for id (in-memory; no DB read).
func (ix *Index) Load(id string) (db.LessonRecord, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	l, ok := ix.lessons[id]
	return l, ok
}

// RecordLoad persists a usage (loaded_count += 1) for a lesson. Best-effort: the
// caller (recall_load) returns the body regardless of a write failure. The
// in-memory count isn't bumped — it's only used by the next Build.
func (ix *Index) RecordLoad(ctx context.Context, id string) error {
	return ix.store.IncrementLessonLoadCount(ctx, id, 1)
}

// Search returns up to k lessons most relevant to query, ranked by cosine (or
// the fused cosine + BM25 score; see Index) plus a small, bounded confidence
// adjustment (descending; see confidenceAdjustment). With an API embedder it
// embeds the query, then delegates to SearchVec.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if k <= 0 || query == "" || ix.Size() == 0 {
		return nil, nil
	}
	if ix.fused {
		return ix.searchFused(ctx, query, k)
	}
	vecs, err := ix.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return ix.SearchVec(vecs[0], k), nil
}

// searchFused is Search for the local and lexical modes. Without a cosine
// signal (no embedder, or a degenerate query vector) only lessons sharing a
// term with the query are candidates, and a query matching none returns nil.
func (ix *Index) searchFused(ctx context.Context, query string, k int) ([]Hit, error) {
	ix.mu.RLock()
	ids, matrix, lessons, lexical := ix.ids, ix.matrix, ix.lessons, ix.lexical
	ix.mu.RUnlock()

	var cos []float64
	if ix.embedder != nil {
		vecs, err := ix.embedder.Embed(ctx, []string{query})
		if err != nil {
			return nil, err
		}
		if qv := vec.NormalizeOrNil(vecs[0]); qv != nil {
			cos = make([]float64, len(matrix))
			for i, row := range matrix {
				cos[i] = vec.Dot(row, qv)
			}
		}
	}
	lex := lexical.Scores(query)
	fused := textrank.Fuse(cos, lex, ix.alpha)
	hits := make([]Hit, 0, len(ids))
	for i, id := range ids {
		if cos == nil && lex[i] <= 0 {
			continue
		}
		l := lessons[id]
		h := Hit{Lesson: l, Score: fused[i] + confidenceAdjustment(l.Score, l.LoadedCount)}
		if cos != nil {
			h.Cosine = cos[i]
		}
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	if len(hits) == 0 {
		return nil, nil
	}
	return hits[:min(k, len(hits))], nil
}

// SearchVec ranks lessons against a precomputed query vector (no embedding) by
// cosine plus confidence: Search's ranking with an API embedder. (For "what is
// the closest existing lesson", e.g. dedup, use Nearest: it ignores
// confidence.) A degenerate query
// vector (norm below vec.MinNorm — e.g. the embedder collapsed an empty or
// whitespace-only string) returns nil rather than every-row-tied-at-zero junk
// from an arbitrary unstable sort.
func (ix *Index) SearchVec(query []float32, k int) []Hit {
	ix.mu.RLock()
	ids, matrix, lessons := ix.ids, ix.matrix, ix.lessons
	ix.mu.RUnlock()

	if k <= 0 || len(ids) == 0 {
		return nil
	}
	qv := vec.NormalizeOrNil(query)
	if qv == nil {
		return nil
	}
	type scored struct {
		i      int
		cosine float64
		score  float64
	}
	scoredHits := make([]scored, len(ids))
	for i, row := range matrix {
		c := vec.Dot(row, qv)
		l := lessons[ids[i]]
		scoredHits[i] = scored{i, c, c + confidenceAdjustment(l.Score, l.LoadedCount)}
	}
	sort.Slice(scoredHits, func(a, b int) bool { return scoredHits[a].score > scoredHits[b].score })
	if k > len(scoredHits) {
		k = len(scoredHits)
	}
	out := make([]Hit, 0, k)
	for _, s := range scoredHits[:k] {
		out = append(out, Hit{Lesson: lessons[ids[s.i]], Cosine: s.cosine, Score: s.score})
	}
	return out
}

// confidenceAdjustment is a lesson's track record expressed as a bounded shift
// of its cosine. Confidence c = score / sqrt(loads + confidenceK) (a high score
// backed by few loads counts for less); tanh saturates it, so the shift stays
// within [-confidencePenalty, +confidenceBonus]. A neutral lesson (score 0) is
// ranked by cosine alone. See confidenceBonus for why it is additive and small.
func confidenceAdjustment(score, loadedCount int) float64 {
	c := float64(score) / math.Sqrt(float64(loadedCount)+confidenceK)
	if c >= 0 {
		return confidenceBonus * math.Tanh(c)
	}
	return -confidencePenalty * math.Tanh(-c)
}

// Nearest returns the lesson whose embedding is closest to query by raw cosine,
// ignoring confidence and BM25: the right question for "is this a
// near-duplicate of an existing lesson?". (Search ranks by cosine plus
// confidence, so its top hit can be a well-rated lesson that is not the closest
// one.) ok is false when the index is empty, has no embeddings (no embedder),
// or the query is degenerate.
func (ix *Index) Nearest(query []float32) (hit Hit, ok bool) {
	ix.mu.RLock()
	ids, matrix, lessons := ix.ids, ix.matrix, ix.lessons
	ix.mu.RUnlock()

	qv := vec.NormalizeOrNil(query)
	if qv == nil || len(ids) == 0 || len(matrix) != len(ids) {
		return Hit{}, false
	}
	best, bestCos := -1, math.Inf(-1)
	for i, row := range matrix {
		if c := vec.Dot(row, qv); c > bestCos {
			best, bestCos = i, c
		}
	}
	l := lessons[ids[best]]
	return Hit{Lesson: l, Cosine: bestCos, Score: bestCos + confidenceAdjustment(l.Score, l.LoadedCount)}, true
}
