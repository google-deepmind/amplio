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

package local

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"
)

// tinyVocab is a hand-made WordPiece vocabulary with the BERT special tokens
// at their usual ids.
var tinyVocab = []string{
	"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]", // 0-4
	"hello", "world", "un", "##aff", "##able", // 5-9
	",", "!", "cafe", "unicode", "中", // 10-14
	"国", "a", "##a", "run", "##s", // 15-19
}

var tinyConfig = TokenizerConfig{
	Unk: "[UNK]", Prefix: "##", MaxWordChars: 100,
	Special: []string{"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]"},
}

func tinyTokenizer(t *testing.T) *tokenizer {
	t.Helper()
	tok, err := newTokenizer(tinyVocab, tinyConfig)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Héllo\tWORLD":             "hello world",
		"Café Ünïcödé":             "cafe unicode", // NFD, drop nonspacing marks, lowercase
		"中国x":                      " 中  国 x",      // CJK ideographs are padded with spaces
		"hel\u200blo\x00\ufffd!":   "hello!",       // format/control chars, NUL and U+FFFD dropped
		"a\u00a0b\u3000c\nd\re\vf": "a b c d ef",   // every Unicode space becomes ' '; \v is a control char
		"ΣΣ":                       "σσ",           // per-character lowercase (no final-sigma rule)
		"😀 emoji":                  "😀 emoji",      // symbols survive
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreTokenize(t *testing.T) {
	got := preTokenize("hello,world!  run's «中» $5")
	want := []string{"hello", ",", "world", "!", "run", "'", "s", "«", "中", "»", "$", "5"}
	if !slices.Equal(got, want) {
		t.Errorf("preTokenize = %q, want %q", got, want)
	}
}

func TestTokenize(t *testing.T) {
	tok := tinyTokenizer(t)
	for in, want := range map[string][]int32{
		"Hello, WORLD!":        {5, 10, 6, 11},
		"unaffable":            {7, 8, 9},    // greedy longest-match WordPiece
		"runs":                 {18, 19},     // continuation piece
		"Café 中国":              {12, 14, 15}, // accents stripped, CJK split per ideograph
		"hello zzz world":      {5, 6},       // an unmatchable word is [UNK], which is dropped
		"unaffablez":           nil,          // ...even when a prefix of it matched
		"hello [MASK] world":   {5, 4, 6},    // special tokens match verbatim...
		"hello [mask]":         {5},          // ...and only verbatim
		"[UNK] hello":          {5},          // a literal [UNK] is dropped like any other
		"hello[SEP][SEP]world": {5, 3, 3, 6}, // and split adjacent text
		"":                     nil,
	} {
		if got := tok.tokenize(in); !slices.Equal(got, want) {
			t.Errorf("tokenize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTokenize_Limits(t *testing.T) {
	tok := tinyTokenizer(t)
	// A word longer than MaxWordChars is [UNK] even if its pieces exist.
	if got := tok.tokenize(strings.Repeat("a", 100)); len(got) != 100 {
		t.Errorf("100-char word: %d tokens, want 100 (a + 99 x ##a)", len(got))
	}
	if got := tok.tokenize(strings.Repeat("a", 101)); len(got) != 0 {
		t.Errorf("101-char word: %v, want dropped as [UNK]", got)
	}
	// The token cap applies after [UNK]s are dropped.
	if got := tok.tokenize(strings.Repeat("a z ", 511)); len(got) != 511 {
		t.Errorf("511 known + 511 unknown words: %d tokens, want 511", len(got))
	}
	if got := tok.tokenize(strings.Repeat("a ", 600)); len(got) != maxTokens {
		t.Errorf("600 words: %d tokens, want the %d cap", len(got), maxTokens)
	}
	// The text is pre-clipped to maxTokens x the median token length (in
	// characters): the tiny vocabulary's median is 4.5, truncated to 4.
	if tok.maxTextChars != maxTokens*4 {
		t.Errorf("maxTextChars = %d, want %d", tok.maxTextChars, maxTokens*4)
	}
	if got := tok.tokenize(strings.Repeat(" ", maxTokens*4-1) + "hello"); len(got) != 0 {
		t.Errorf("text past the character clip was tokenized: %v", got)
	}
	if got := tok.tokenize(strings.Repeat(" ", maxTokens*4-5) + "hello"); len(got) != 1 {
		t.Errorf("text within the character clip was lost: %v", got)
	}
	if got := clipRunes("héllo", 2); got != "hé" {
		t.Errorf("clipRunes counts characters, not bytes: %q", got)
	}
}

func TestNewTokenizer_RejectsMissingTokens(t *testing.T) {
	bad := tinyConfig
	bad.Unk = "<unk>"
	if _, err := newTokenizer(tinyVocab, bad); err == nil {
		t.Error("an unknown token missing from the vocabulary must be rejected")
	}
	bad = tinyConfig
	bad.Special = []string{"<s>"}
	if _, err := newTokenizer(tinyVocab, bad); err == nil {
		t.Error("a special token missing from the vocabulary must be rejected")
	}
}

// tinyTable builds a 2-dim table over tinyVocab where token i's row is
// (i, 1) quantized with scale 0.5.
func tinyTable() *Table {
	t := &Table{
		Header: Header{Format: TableFormat, Model: "test/tiny", Revision: "r1", Name: "tiny", Dims: 2,
			Quant: QuantInt8Row, Generator: 1, Vocab: len(tinyVocab), Tokenizer: tinyConfig},
		Vocab: tinyVocab,
	}
	for i := range tinyVocab {
		t.Rows = append(t.Rows, int8(2*i), 2) //nolint:gosec // small
		t.Scales = append(t.Scales, 0.5)
	}
	return t
}

func TestTable_RoundTrip(t *testing.T) {
	want := tinyTable()
	b, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseTable(b)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Vocab, want.Vocab) || !slices.Equal(got.Rows, want.Rows) ||
		!slices.Equal(got.Scales, want.Scales) || got.Header.Name != "tiny" ||
		!slices.Equal(got.Header.Tokenizer.Special, tinyConfig.Special) {
		t.Errorf("round trip mismatch: %+v", got)
	}
	for name, corrupt := range map[string][]byte{
		"truncated":   b[:len(b)-1],
		"extra byte":  append(append([]byte(nil), b...), 0),
		"bad magic":   append([]byte("XXXXXXXX"), b[8:]...),
		"header only": b[:20],
		"empty":       nil,
	} {
		if _, err := ParseTable(corrupt); err == nil {
			t.Errorf("%s table parsed without error", name)
		}
	}
	bad := tinyTable()
	bad.Scales = bad.Scales[1:]
	if _, err := bad.Marshal(); err == nil {
		t.Error("Marshal must refuse an inconsistent table")
	}
}

func tinyEmbedder(t *testing.T) *Embedder {
	t.Helper()
	b, err := tinyTable().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m, err := newModel(b)
	if err != nil {
		t.Fatal(err)
	}
	return &Embedder{m: m}
}

func TestEmbed(t *testing.T) {
	e := tinyEmbedder(t)
	vecs, err := e.Embed(context.Background(), []string{"hello world", "zzz", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// Mean of the dequantized rows (5,1) and (6,1) is (5.5, 1), then L2-normalized.
	n := math.Hypot(5.5, 1)
	if got := vecs[0]; math.Abs(float64(got[0])-5.5/n) > 1e-6 || math.Abs(float64(got[1])-1/n) > 1e-6 {
		t.Errorf("Embed(hello world) = %v, want (%v, %v)", got, 5.5/n, 1/n)
	}
	if !slices.Equal(vecs[1], []float32{0, 0}) {
		t.Errorf("a text with no known token = %v, want the zero vector", vecs[1])
	}
	if e.ModelID() == "" || !strings.HasPrefix(e.ModelID(), "local_tiny-pca2-") || len(e.ModelID()) != len("local_tiny-pca2-")+8 {
		t.Errorf("ModelID = %q, want local_tiny-pca2-<8 hex>", e.ModelID())
	}
	if e.Name() != "tiny" || e.HybridAlpha() != hybridAlpha {
		t.Errorf("Name/HybridAlpha = %q/%v", e.Name(), e.HybridAlpha())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"hello"}); err == nil {
		t.Error("Embed must honor a cancelled context")
	}
}

// The ModelID changes with the table content, so cached vectors from another
// table are never reused.
func TestModelID_TracksTableContent(t *testing.T) {
	a, err := tinyTable().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	changed := tinyTable()
	changed.Rows[10] = 7
	b, err := changed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	ma, err := newModel(a)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := newModel(b)
	if err != nil {
		t.Fatal(err)
	}
	if ma.id == mb.id {
		t.Errorf("different tables share ModelID %q", ma.id)
	}
}

// Available and New agree, whichever way this binary was built.
func TestAvailable(t *testing.T) {
	e, err := New()
	if Available() {
		if err != nil || e == nil {
			t.Fatalf("table compiled in, but New() = %v, %v", e, err)
		}
		return
	}
	if err != ErrNoTable {
		t.Fatalf("no table compiled in: New() error = %v, want ErrNoTable", err)
	}
}
