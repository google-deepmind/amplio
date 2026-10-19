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

// Package local is amplio's built-in embedder: a model2vec static-embedding
// table compiled into the binary, so recall works with no embedding API. A
// text's vector is the mean of its WordPiece tokens' rows, L2-normalized —
// microseconds per text, no network, no cgo.
//
// The table is not in the source tree. `make embed-table` (gentable) derives it
// from a pinned upstream model and installs it into data/, where go:embed picks
// it up. A binary built without it still compiles (data/README.md keeps the
// embed pattern valid); Available then reports false and recall falls back to
// BM25 alone.
package local

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sync"

	amplioembed "amplio/internal/embed"
)

//go:embed data
var dataFS embed.FS

const tablePath = "data/" + TableFile

// maxTokens is model2vec's default encode(max_length=512).
const maxTokens = 512

// hybridAlpha is the cosine weight when fusing with BM25 (textrank.Fuse). A
// static embedding misses literal matches that BM25 catches, and vice versa;
// 0.7 was the best of the weights evaluated against API-model rankings.
const hybridAlpha = 0.7

// ErrNoTable means the binary was built without the embedding table.
var ErrNoTable = errors.New("binary built without the embedding table")

// Available reports whether this binary has the embedding table compiled in.
func Available() bool {
	_, err := fs.Stat(dataFS, tablePath)
	return err == nil
}

// loadEmbedded parses the compiled-in table once, on first use (~50 ms).
var loadEmbedded = sync.OnceValues(func() (*model, error) {
	b, err := dataFS.ReadFile(tablePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoTable
	}
	if err != nil {
		return nil, err
	}
	return newModel(b)
})

// Embedder is the built-in static embedder. It implements embed.Hybrid:
// indexes fuse its cosine scores with BM25, and may re-embed freely since it
// is in-process.
type Embedder struct{ m *model }

var _ amplioembed.Hybrid = (*Embedder)(nil)

// New returns the built-in embedder, or ErrNoTable when the binary was built
// without the table.
func New() (*Embedder, error) {
	m, err := loadEmbedded()
	if err != nil {
		return nil, err
	}
	return &Embedder{m: m}, nil
}

// Name is the source model's short name, for operator-facing status.
func (e *Embedder) Name() string { return e.m.name }

// ModelID names the model, its reduced width and a hash of the exact table,
// so cached vectors are invalidated whenever the table changes.
func (e *Embedder) ModelID() string { return e.m.id }

// HybridAlpha implements embed.Hybrid.
func (e *Embedder) HybridAlpha() float64 { return hybridAlpha }

// Embed implements embed.Embedder. A text with no known token embeds to the
// zero vector.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.m.encode(t)
	}
	return out, nil
}

// model is a parsed table ready to encode.
type model struct {
	tok    *tokenizer
	dims   int
	rows   []int8
	scales []float32
	name   string
	id     string
}

func newModel(file []byte) (*model, error) {
	t, err := ParseTable(file)
	if err != nil {
		return nil, fmt.Errorf("embedding table: %w", err)
	}
	tok, err := newTokenizer(t.Vocab, t.Header.Tokenizer)
	if err != nil {
		return nil, fmt.Errorf("embedding table: %w", err)
	}
	sum := sha256.Sum256(file)
	return &model{
		tok:    tok,
		dims:   t.Header.Dims,
		rows:   t.Rows,
		scales: t.Scales,
		name:   t.Header.Name,
		id:     fmt.Sprintf("local_%s-pca%d-%x", t.Header.Name, t.Header.Dims, sum[:4]),
	}, nil
}

// encode is model2vec's encode: the mean of the dequantized token rows,
// L2-normalized (float64 accumulation, as in the reference).
func (m *model) encode(text string) []float32 {
	ids := m.tok.tokenize(text)
	out := make([]float32, m.dims)
	if len(ids) == 0 {
		return out
	}
	acc := make([]float64, m.dims)
	for _, id := range ids {
		row := m.rows[int(id)*m.dims : (int(id)+1)*m.dims]
		s := m.scales[id]
		for j, v := range row {
			acc[j] += float64(float32(v) * s)
		}
	}
	var n2 float64
	for j := range acc {
		acc[j] /= float64(len(ids))
		n2 += acc[j] * acc[j]
	}
	if n2 == 0 {
		return out
	}
	norm := math.Sqrt(n2)
	for j, v := range acc {
		out[j] = float32(v / norm)
	}
	return out
}
