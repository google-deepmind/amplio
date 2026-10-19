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

package textrank

import (
	"math"
	"slices"
	"testing"
)

func TestTokens(t *testing.T) {
	got := Tokens("How do I run the Spanner-SQL query_tool on Café data 2024?")
	want := []string{"run", "spanner", "sql", "query", "tool", "café", "data", "2024"}
	if !slices.Equal(got, want) {
		t.Errorf("Tokens = %q, want %q", got, want)
	}
}

func TestBM25_RanksByTermEvidence(t *testing.T) {
	ix := NewBM25([]string{
		"build and test code with bazel",
		"query spanner databases with sql",
		"read and send email",
		"spanner spanner spanner schema migrations for spanner",
	})
	s := ix.Scores("spanner sql")
	if s[0] != 0 || s[2] != 0 {
		t.Errorf("docs sharing no term must score 0: %v", s)
	}
	// Doc 1 matches both terms (and the rare "sql"); doc 3 only repeats "spanner",
	// which saturates (k1) rather than growing linearly.
	if !(s[1] > s[3] && s[3] > 0) {
		t.Errorf("scores = %v, want doc1 > doc3 > 0", s)
	}
	if got := ix.Scores("the of and"); slices.Max(got) != 0 {
		t.Errorf("a stopword-only query should match nothing: %v", got)
	}
	if ix.Len() != 4 {
		t.Errorf("Len = %d, want 4", ix.Len())
	}
}

func TestBM25_Degenerate(t *testing.T) {
	if s := NewBM25(nil).Scores("x"); len(s) != 0 {
		t.Errorf("empty corpus scores = %v", s)
	}
	for _, v := range NewBM25([]string{"", "..."}).Scores("x") {
		if v != 0 || math.IsNaN(v) {
			t.Errorf("token-less corpus must score 0, got %v", v)
		}
	}
}

func TestMinMax(t *testing.T) {
	s := []float64{2, 4, 3}
	MinMax(s)
	if !slices.Equal(s, []float64{0, 1, 0.5}) {
		t.Errorf("MinMax = %v", s)
	}
	flat := []float64{7, 7}
	MinMax(flat)
	if !slices.Equal(flat, []float64{0, 0}) {
		t.Errorf("all-equal MinMax = %v, want zeros", flat)
	}
}

func TestFuse(t *testing.T) {
	cos := []float64{0.2, 0.4, 0.3}
	lex := []float64{0, 0, 9}
	got := Fuse(cos, lex, 0.7)
	want := []float64{0, 0.7, 0.7*0.5 + 0.3}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("Fuse = %v, want %v", got, want)
		}
	}
	if cos[0] != 0.2 || lex[2] != 9 {
		t.Error("Fuse must not modify its inputs")
	}
	// No cosine signal: the lexical part alone, with its full weight at alpha 0.
	if got := Fuse(nil, lex, 0); !slices.Equal(got, []float64{0, 0, 1}) {
		t.Errorf("lexical-only Fuse = %v", got)
	}
}
