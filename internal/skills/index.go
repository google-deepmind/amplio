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
	"log/slog"
	"sort"
	"sync"
	"time"

	"amplio/internal/config"
	"amplio/internal/embed"
	"amplio/internal/textrank"
	"amplio/internal/vec"
)

// Hit is one search result.
type Hit struct {
	Entry Entry
	// Score ranks the hit: cosine similarity in [-1, 1] with an API embedder,
	// the fused cosine + BM25 score in [0, 1] otherwise (see embed.FusionWeight).
	Score float64
}

// lexicalCacheKey is the cache's model key when there is no embedder: entries
// are cached for their parsed fields only, with no vector.
const lexicalCacheKey = "lexical"

// Index is an in-memory similarity index over skill descriptions. Build once
// at startup (disk-cached embeddings); query repeatedly. Search is brute force
// over a few hundred rows — fast enough without a vector index.
//
// How it ranks follows the embedder (embed.FusionWeight): cosine alone for an
// API embedder; cosine fused with BM25 for the built-in local one; BM25 alone
// with no embedder (nil).
type Index struct {
	sources  []Source
	embedder embed.Embedder // nil: BM25 only
	cache    Cache
	alpha    float64 // cosine weight when fused
	fused    bool    // rank by textrank.Fuse rather than cosine alone

	mu      sync.RWMutex
	entries map[string]Entry
	names   []string        // row order
	matrix  [][]float32     // L2-normalized vectors, row per name (nil without an embedder)
	lexical *textrank.BM25  // over embedText, row per name (nil unless fused)
	pinned  map[string]bool // names of entries from a pinned source
	built   bool

	// initial is how many relevance-ranked skills the session-start seed lists
	// (see Initial). An instance-wide recall policy carried on the index — like
	// lessons.Index's recall switch — because the index is the one per-instance
	// recall object every agent already receives. Set once at startup.
	initial int
}

// NewIndex constructs an index; call Build to populate (does I/O + embeds). A
// nil embedder ranks by BM25 alone.
func NewIndex(sources []Source, embedder embed.Embedder, cache Cache) *Index {
	alpha, fused := embed.FusionWeight(embedder)
	return &Index{sources: sources, embedder: embedder, cache: cache, alpha: alpha, fused: fused,
		initial: config.DefaultSkillsInitial}
}

// cacheKey is the model key the cache stores this index's entries under.
func (ix *Index) cacheKey() string {
	if ix.embedder == nil {
		return lexicalCacheKey
	}
	return ix.embedder.ModelID()
}

// lexicalIndex builds the BM25 index over the entries' embedding texts, or nil
// when the index ranks by cosine alone.
func (ix *Index) lexicalIndex(names []string, entries map[string]Entry) *textrank.BM25 {
	if !ix.fused {
		return nil
	}
	docs := make([]string, len(names))
	for i, n := range names {
		docs[i] = embedText(entries[n])
	}
	return textrank.NewBM25(docs)
}

// SetInitialLimit sets how many relevance-ranked skills the session-start seed
// lists, in addition to pinned ones ([skills].initial). Call once at startup.
func (ix *Index) SetInitialLimit(n int) { ix.initial = max(n, 0) }

// InitialLimit is how many relevance-ranked skills the session-start seed lists.
func (ix *Index) InitialLimit() int { return ix.initial }

func embedText(e Entry) string { return e.Name + ": " + e.Description }

// LoadCached hydrates the index purely from the persisted cache — no file scan,
// no embedding API. Fast: just a single DB read + matrix construction. After
// this returns successfully with cached entries, IsBuilt() reports true and
// Search/Load serve from the cached snapshot.
//
// Intended as Stage 1 of a two-stage startup: call this synchronously so the
// agent has recall immediately, then call Build in a goroutine to reconcile
// the in-memory state against the current on-disk corpus. The atomic swap at
// Build's end replaces this snapshot with the up-to-date one.
//
// Returns the number of entries hydrated (0 means the cache was empty — the
// caller should treat this as cold-start and Build synchronously). A failure
// to load the cache is logged-and-ignored; the index stays empty and Build
// will recover from disk.
func (ix *Index) LoadCached(ctx context.Context) int {
	cached, err := ix.cache.Load(ctx, ix.cacheKey())
	if err != nil {
		slog.Warn("skill cache hydrate failed; starting empty", "error", err)
		return 0
	}
	if len(cached) == 0 {
		return 0
	}
	// Sort by name for stable ordering (the file-scan path uses a sort, too —
	// keeping both deterministic means Search results don't churn between
	// stages 1 and 2 just because of insertion order).
	names := make([]string, 0, len(cached))
	for n := range cached {
		names = append(names, n)
	}
	sort.Strings(names)
	entryMap := make(map[string]Entry, len(cached))
	var matrix [][]float32
	for _, n := range names {
		c := cached[n]
		entryMap[n] = Entry{
			Name:        n,
			Description: c.Description,
			Body:        c.Body,
			Path:        c.Path,
			ContentHash: c.Hash,
		}
		if ix.embedder != nil {
			matrix = append(matrix, vec.Normalize(c.Vector))
		}
	}
	pinned := pinnedNames(ix.sources, entryMap)
	lexical := ix.lexicalIndex(names, entryMap)
	ix.mu.Lock()
	ix.entries, ix.names, ix.matrix, ix.lexical, ix.pinned, ix.built = entryMap, names, matrix, lexical, pinned, true
	ix.mu.Unlock()
	slog.Info("skill index hydrated from cache (background reconcile to follow)", "skills", len(names), "pinned", len(pinned))
	return len(names)
}

// Build scans the sources, embeds new/changed skills (reusing cached vectors for
// unchanged ones), persists the cache, and swaps in the in-memory state. It
// retries a transient embedding failure (skills matter) up to maxAttempts before
// returning the error; the caller decides whether to fail or degrade to no recall.
func (ix *Index) Build(ctx context.Context) error {
	const maxAttempts = 3
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = ix.buildOnce(ctx); err == nil {
			return nil
		}
		slog.Warn("skill index build attempt failed", "attempt", attempt, "max", maxAttempts, "error", err)
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}
	return err
}

func (ix *Index) buildOnce(ctx context.Context) error {
	entries := scanSources(ix.sources)
	model := ix.cacheKey()

	cached, err := ix.cache.Load(ctx, model)
	if err != nil {
		slog.Warn("skill cache load failed; re-embedding all", "error", err)
		cached = nil
	}

	var toEmbedNames, toEmbedTexts []string
	reuse := make(map[string][]float32)
	for _, e := range entries {
		if ix.embedder == nil {
			reuse[e.Name] = nil // nothing to embed: BM25 reads the entry itself
			continue
		}
		if c, ok := cached[e.Name]; ok && c.Hash == e.ContentHash {
			reuse[e.Name] = c.Vector
			continue
		}
		toEmbedNames = append(toEmbedNames, e.Name)
		toEmbedTexts = append(toEmbedTexts, embedText(e))
	}

	fresh := make(map[string][]float32)
	if len(toEmbedTexts) > 0 {
		vecs, err := ix.embedder.Embed(ctx, toEmbedTexts)
		if err != nil {
			return err
		}
		for i, n := range toEmbedNames {
			fresh[n] = vecs[i]
		}
	}

	entryMap := make(map[string]Entry, len(entries))
	names := make([]string, 0, len(entries))
	var matrix [][]float32
	save := make(map[string]CacheEntry, len(entries))
	for _, e := range entries {
		embVec, ok := reuse[e.Name]
		if !ok {
			embVec, ok = fresh[e.Name]
		}
		if !ok {
			continue // unreachable: every entry was reused or embedded
		}
		entryMap[e.Name] = e
		names = append(names, e.Name)
		if ix.embedder != nil {
			matrix = append(matrix, vec.Normalize(embVec))
		}
		save[e.Name] = CacheEntry{
			Hash:        e.ContentHash,
			Vector:      embVec,
			Description: e.Description,
			Path:        e.Path,
			Body:        e.Body,
		}
	}

	if err := ix.cache.Save(ctx, model, save); err != nil {
		slog.Warn("skill cache save failed", "error", err)
	}

	pinned := pinnedNames(ix.sources, entryMap)
	lexical := ix.lexicalIndex(names, entryMap)
	ix.mu.Lock()
	ix.entries, ix.names, ix.matrix, ix.lexical, ix.pinned, ix.built = entryMap, names, matrix, lexical, pinned, true
	ix.mu.Unlock()
	slog.Info("skill index built", "skills", len(names), "pinned", len(pinned), "embedded", len(fresh), "reused", len(reuse))
	return nil
}

// IsBuilt reports whether Build has completed.
func (ix *Index) IsBuilt() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.built
}

// Size is the number of indexed skills.
func (ix *Index) Size() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.names)
}

// PinnedCount is the number of indexed skills that come from a pinned source.
func (ix *Index) PinnedCount() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.pinned)
}

// Load returns the parsed entry for name (in-memory; no file read).
func (ix *Index) Load(name string) (Entry, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.entries[name]
	return e, ok
}

// snapshot is one consistent view of the index for ranking a query.
type snapshot struct {
	names   []string
	entries map[string]Entry
	matrix  [][]float32
	lexical *textrank.BM25
	pinned  map[string]bool
}

func (ix *Index) snapshot() snapshot {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return snapshot{ix.names, ix.entries, ix.matrix, ix.lexical, ix.pinned}
}

// Search returns up to k skills most relevant to query, by score descending.
// A query with no usable signal (e.g. one the embedder collapses to a
// near-zero vector, with no lexical match either) returns nil rather than
// every-row-tied-at-zero junk from an arbitrary unstable sort.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	s := ix.snapshot()
	if k <= 0 || len(s.names) == 0 || query == "" {
		return nil, nil
	}
	scores, eligible, err := ix.score(ctx, query, s)
	if err != nil || scores == nil {
		return nil, err
	}
	return topK(s, scores, k, func(i int) bool { return eligible == nil || eligible[i] }), nil
}

// Initial selects the skills listed at session start: EVERY skill from a pinned
// source, plus the k most relevant of the rest (pinned ones never take one of
// the k slots, and never appear twice). Pinned skills are ordered by relevance
// to query, or by name when there is no query (a chat session) or ranking
// failed. The query is scored once for both.
//
// On an embedding error it still returns the pinned skills (by name) along
// with the error: they don't depend on the ranking, and the caller can log the
// failure and list them anyway.
func (ix *Index) Initial(ctx context.Context, query string, k int) (pinned []Entry, relevant []Hit, err error) {
	s := ix.snapshot()

	var scores []float64
	var eligible []bool
	if query != "" && len(s.names) > 0 && (k > 0 || len(s.pinned) > 0) {
		scores, eligible, err = ix.score(ctx, query, s)
	}

	if len(s.pinned) > 0 {
		if scores != nil {
			for _, h := range topK(s, scores, len(s.names), func(i int) bool { return s.pinned[s.names[i]] }) {
				pinned = append(pinned, h.Entry)
			}
		} else {
			byName := make([]string, 0, len(s.pinned))
			for n := range s.pinned {
				byName = append(byName, n)
			}
			sort.Strings(byName)
			for _, n := range byName {
				pinned = append(pinned, s.entries[n])
			}
		}
	}
	if scores != nil && k > 0 {
		relevant = topK(s, scores, k, func(i int) bool {
			return !s.pinned[s.names[i]] && (eligible == nil || eligible[i])
		})
	}
	return pinned, relevant, err
}

// score ranks every row for query (see Index for the three ranking modes).
// nil scores mean the query carries no usable signal. eligible, when non-nil,
// marks the rows that may be returned as relevant: without a cosine signal (no
// embedder, or a degenerate query vector) a row sharing no term with the query
// is no match at all.
func (ix *Index) score(ctx context.Context, query string, s snapshot) (scores []float64, eligible []bool, err error) {
	cos, err := ix.cosine(ctx, query, s.matrix)
	if err != nil || !ix.fused {
		return cos, nil, err
	}
	lex := s.lexical.Scores(query)
	fused := textrank.Fuse(cos, lex, ix.alpha)
	if cos != nil {
		return fused, nil, nil
	}
	eligible = make([]bool, len(lex))
	matched := false
	for i, v := range lex {
		eligible[i] = v > 0
		matched = matched || v > 0
	}
	if !matched {
		return nil, nil, nil
	}
	return fused, eligible, nil
}

// cosine embeds query and returns its cosine similarity to every row of
// matrix, or nil without an embedder or for a degenerate query vector (norm
// below vec.MinNorm — e.g. the embedder collapsed a whitespace-only string):
// every row tied at zero would be arbitrary-order junk.
func (ix *Index) cosine(ctx context.Context, query string, matrix [][]float32) ([]float64, error) {
	if ix.embedder == nil {
		return nil, nil
	}
	vecs, err := ix.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	qv := vec.NormalizeOrNil(vecs[0])
	if qv == nil {
		return nil, nil
	}
	scores := make([]float64, len(matrix))
	for i, row := range matrix {
		scores[i] = vec.Dot(row, qv)
	}
	return scores, nil
}

// topK returns up to k entries by descending score, considering only the rows
// for which keep reports true.
func topK(s snapshot, scores []float64, k int, keep func(i int) bool) []Hit {
	idx := make([]int, 0, len(s.names))
	for i := range s.names {
		if keep(i) {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	if k > len(idx) {
		k = len(idx)
	}
	out := make([]Hit, 0, k)
	for _, i := range idx[:k] {
		out = append(out, Hit{Entry: s.entries[s.names[i]], Score: scores[i]})
	}
	return out
}
