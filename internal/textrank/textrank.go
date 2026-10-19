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

// Package textrank holds the lexical side of recall: an Okapi BM25 index over a
// fixed corpus, and the per-query score fusion that combines it with cosine
// similarity. Shared by the skill and lesson indexes, which use it when recall
// runs on the built-in local embedder (hybrid) or on no embedder at all
// (lexical).
package textrank

import (
	"math"
	"strings"
	"unicode"
)

// BM25 parameters (the common Lucene defaults).
const (
	k1 = 1.2
	b  = 0.75
)

// stopwords is a small English list (Lucene's classic set plus a few pronouns
// and auxiliaries). Dropping them matters for long task texts, whose
// boilerplate would otherwise dominate the lexical score.
var stopwords = func() map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(`a about above after again all also am an and any are as at be been
	before being below between both but by can could did do does don done down during each few for from
	further had has have having he her here him how i if in into is it its just may me might more most must
	my no not now of on once or other our out over own s same shall she should so some such t than that the
	their them then there these they this through to too under up very was we what when where which who whom
	why will with would you your`) {
		m[w] = true
	}
	return m
}()

// Tokens is the BM25 tokenizer: lowercased runs of Unicode letters and digits,
// minus stopwords.
func Tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := f[:0]
	for _, t := range f {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

type posting struct {
	doc int
	tf  float64
}

// BM25 is an immutable Okapi BM25 index over a fixed list of documents.
type BM25 struct {
	postings map[string][]posting
	idf      map[string]float64
	norm     []float64 // per-doc k1 * (1 - b + b*len/avglen)
}

// NewBM25 indexes docs; Scores reports one score per doc, in this order.
func NewBM25(docs []string) *BM25 {
	ix := &BM25{postings: map[string][]posting{}, idf: map[string]float64{}, norm: make([]float64, len(docs))}
	lens := make([]float64, len(docs))
	var total float64
	for i, d := range docs {
		toks := Tokens(d)
		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			if tf[t] == 0 {
				ix.postings[t] = append(ix.postings[t], posting{doc: i})
			}
			tf[t]++
		}
		for t, c := range tf {
			p := ix.postings[t]
			p[len(p)-1].tf = float64(c)
		}
		lens[i] = float64(len(toks))
		total += lens[i]
	}
	if len(docs) == 0 {
		return ix
	}
	avg := total / float64(len(docs))
	for i := range docs {
		ix.norm[i] = k1 * (1 - b + b*lens[i]/max(avg, 1e-9))
	}
	n := float64(len(docs))
	for t, p := range ix.postings {
		df := float64(len(p))
		ix.idf[t] = math.Log(1 + (n-df+0.5)/(df+0.5)) // Lucene's non-negative idf
	}
	return ix
}

// Len is the number of indexed documents.
func (ix *BM25) Len() int { return len(ix.norm) }

// Scores returns the BM25 score of every document for query (0 for a document
// sharing no term with it).
func (ix *BM25) Scores(query string) []float64 {
	s := make([]float64, len(ix.norm))
	// Query terms in first-occurrence order, so float summation (and therefore
	// tie-breaking) is deterministic run to run.
	qtf := map[string]int{}
	var order []string
	for _, t := range Tokens(query) {
		if qtf[t] == 0 {
			order = append(order, t)
		}
		qtf[t]++
	}
	for _, t := range order {
		w := float64(qtf[t]) * ix.idf[t]
		for _, p := range ix.postings[t] {
			s[p.doc] += w * p.tf * (k1 + 1) / (p.tf + ix.norm[p.doc])
		}
	}
	return s
}

// MinMax rescales s in place to [0, 1] over its own range; an all-equal s
// (including all zeros) becomes all zeros, so it contributes nothing to a fusion.
func MinMax(s []float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range s {
		lo, hi = min(lo, v), max(hi, v)
	}
	for i, v := range s {
		if hi > lo {
			s[i] = (v - lo) / (hi - lo)
		} else {
			s[i] = 0
		}
	}
}

// Fuse combines a cosine and a lexical score per document: alpha*cos' +
// (1-alpha)*lex', where ' is MinMax over all documents for this query. Each
// signal's scale differs per query, which is why both are normalized first.
// The result is in [0, 1]. cos may be nil (no embedder: lexical only). Neither
// input is modified.
func Fuse(cos, lex []float64, alpha float64) []float64 {
	out := make([]float64, len(lex))
	l := append([]float64(nil), lex...)
	MinMax(l)
	var c []float64
	if cos != nil {
		c = append([]float64(nil), cos...)
		MinMax(c)
	}
	for i := range out {
		if c != nil {
			out[i] = alpha * c[i]
		}
		out[i] += (1 - alpha) * l[i]
	}
	return out
}
