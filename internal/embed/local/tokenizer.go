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
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// tokenizer reproduces what model2vec feeds its lookup table for a BERT
// WordPiece tokenizer.json: the HF tokenizers pipeline (special-token split,
// BertNormalizer, BertPreTokenizer, WordPiece) with no [CLS]/[SEP], then
// model2vec's post-processing (drop [UNK], cap the length). Every step mirrors
// the reference implementation, including its quirks, so that the vectors
// match the ones the recipe was evaluated with.
type tokenizer struct {
	vocab        map[string]int32 // token -> id
	cont         map[string]int32 // continuation pieces with the prefix stripped -> id
	unk          int32
	maxWordChars int
	special      []string // matched verbatim in the raw text
	maxTokens    int
	maxTextChars int
}

func newTokenizer(vocab []string, cfg TokenizerConfig) (*tokenizer, error) {
	t := &tokenizer{
		vocab:        make(map[string]int32, len(vocab)),
		cont:         make(map[string]int32),
		maxWordChars: cfg.MaxWordChars,
		maxTokens:    maxTokens,
	}
	lens := make([]int, len(vocab))
	for id, tok := range vocab {
		t.vocab[tok] = int32(id) //nolint:gosec // vocab size is validated far below 2^31
		if rest, ok := strings.CutPrefix(tok, cfg.Prefix); ok && cfg.Prefix != "" {
			t.cont[rest] = int32(id) //nolint:gosec // ditto
		}
		lens[id] = utf8.RuneCountInString(tok)
	}
	unk, ok := t.vocab[cfg.Unk]
	if !ok {
		return nil, fmt.Errorf("unknown token %q is not in the vocabulary", cfg.Unk)
	}
	t.unk = unk
	for _, s := range cfg.Special {
		if _, ok := t.vocab[s]; !ok {
			return nil, fmt.Errorf("special token %q is not in the vocabulary", s)
		}
		if s != "" {
			t.special = append(t.special, s)
		}
	}
	// model2vec pre-clips the text to maxTokens x the median token length (in
	// characters, truncated to an int) before tokenizing.
	slices.Sort(lens)
	n := len(lens)
	if n == 0 {
		return nil, fmt.Errorf("empty vocabulary")
	}
	median := float64(lens[n/2])
	if n%2 == 0 {
		median = float64(lens[n/2-1]+lens[n/2]) / 2
	}
	t.maxTextChars = maxTokens * int(median)
	return t, nil
}

// tokenize returns the token ids model2vec pools for text.
func (t *tokenizer) tokenize(text string) []int32 {
	text = clipRunes(text, t.maxTextChars)
	var ids []int32
	t.splitSpecial(text, func(seg string, special bool) {
		if special {
			ids = append(ids, t.vocab[seg])
			return
		}
		for _, w := range preTokenize(normalize(seg)) {
			ids = t.wordPiece(w, ids)
		}
	})
	kept := ids[:0]
	for _, id := range ids {
		if id != t.unk {
			kept = append(kept, id)
		}
	}
	if t.maxTokens > 0 && len(kept) > t.maxTokens {
		kept = kept[:t.maxTokens]
	}
	return kept
}

// clipRunes returns the first n characters of s (all of s when n <= 0).
func clipRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// splitSpecial cuts text at verbatim occurrences of the special tokens
// (leftmost first, longest on a tie), emitting the pieces in order.
func (t *tokenizer) splitSpecial(text string, emit func(seg string, special bool)) {
	for text != "" {
		at, tok := -1, ""
		for _, s := range t.special {
			if i := strings.Index(text, s); i >= 0 && (at < 0 || i < at || (i == at && len(s) > len(tok))) {
				at, tok = i, s
			}
		}
		if at < 0 {
			emit(text, false)
			return
		}
		if at > 0 {
			emit(text[:at], false)
		}
		emit(tok, true)
		text = text[at+len(tok):]
	}
}

// wordPiece appends w's greedy longest-match-first pieces to out; a word that
// is too long or has an unmatchable remainder becomes a single unknown token.
func (t *tokenizer) wordPiece(w string, out []int32) []int32 {
	if utf8.RuneCountInString(w) > t.maxWordChars {
		return append(out, t.unk)
	}
	mark := len(out)
	for start := 0; start < len(w); {
		dict := t.vocab
		if start > 0 {
			dict = t.cont
		}
		end := len(w)
		for ; end > start; end -= lastRuneLen(w[start:end]) {
			if id, ok := dict[w[start:end]]; ok {
				out = append(out, id)
				break
			}
		}
		if end == start {
			return append(out[:mark], t.unk)
		}
		start = end
	}
	return out
}

func lastRuneLen(s string) int {
	_, n := utf8.DecodeLastRuneInString(s)
	return n
}

// normalize is HF's BertNormalizer with clean_text, handle_chinese_chars,
// strip_accents and lowercase all on.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0 || r == utf8.RuneError || (r != '\t' && r != '\n' && r != '\r' && isOther(r)):
			continue
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case isCJK(r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	var out strings.Builder
	out.Grow(b.Len())
	for _, r := range norm.NFD.String(b.String()) {
		if !unicode.Is(unicode.Mn, r) {
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

// preTokenize is HF's BertPreTokenizer: split on whitespace (dropped) and on
// punctuation (each punctuation character becomes its own word).
func preTokenize(s string) []string {
	var words []string
	start := -1
	for i, r := range s {
		space := unicode.IsSpace(r)
		if !space && !isPunct(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			words = append(words, s[start:i])
			start = -1
		}
		if !space {
			words = append(words, string(r))
		}
	}
	if start >= 0 {
		words = append(words, s[start:])
	}
	return words
}

// cjkRanges are the CJK ideograph blocks BertNormalizer pads with spaces.
var cjkRanges = [][2]rune{
	{0x4E00, 0x9FFF}, {0x3400, 0x4DBF}, {0x20000, 0x2A6DF}, {0x2A700, 0x2B73F},
	{0x2B740, 0x2B81F}, {0x2B920, 0x2CEAF}, {0xF900, 0xFAFF}, {0x2F800, 0x2FA1F},
}

func isCJK(r rune) bool {
	for _, c := range cjkRanges {
		if r >= c[0] && r <= c[1] {
			return true
		}
	}
	return false
}

// isOther is general category C*, including unassigned code points (which
// Go's unicode.C table leaves out).
func isOther(r rune) bool {
	return unicode.In(r, unicode.C) || !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

// isPunct is BERT's punctuation: all ASCII non-alphanumeric symbols plus the
// Unicode P* categories.
func isPunct(r rune) bool {
	return (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) ||
		unicode.IsPunct(r)
}
