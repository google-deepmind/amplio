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

package sessionsearch

import (
	"strings"
	"unicode"
)

// simpleQuery is a query in the simple syntax, converted to FTS5.
type simpleQuery struct {
	// FTS5 is the query to MATCH; empty when no term is left to search for.
	FTS5 string
	// Terms are the FTS5 strings of the query's words and phrases.
	Terms []string
	// HasOR is whether an OR joins any two terms.
	HasOR bool
}

// AnyTerm is the FTS5 query matching any of the terms.
func (q simpleQuery) AnyTerm() string { return strings.Join(q.Terms, " OR ") }

// toFTS5 converts a simple-syntax query to FTS5. Each word, punctuation and
// all, and each "quoted phrase" becomes an FTS5 string, which the tokenizer
// splits as it splits the indexed text; a standalone OR joins the terms on
// either side; AND is accepted and changes nothing; the rest are ANDed. A
// term with no letters or digits would match nothing, so it is dropped. The
// result is never an FTS5 syntax error.
func toFTS5(query string) simpleQuery {
	var groups [][]string // ANDed groups of ORed terms
	pendingOR := false
	for _, w := range simpleWords(query) {
		if !w.quoted && (w.text == "OR" || w.text == "AND") {
			pendingOR = w.text == "OR" && len(groups) > 0
			continue
		}
		if !searchable(w.text) {
			continue
		}
		term := `"` + w.text + `"` // simpleWords leaves no quote in a word.
		if pendingOR {
			groups[len(groups)-1] = append(groups[len(groups)-1], term)
		} else {
			groups = append(groups, []string{term})
		}
		pendingOR = false
	}

	var q simpleQuery
	parts := make([]string, len(groups))
	for i, g := range groups {
		q.Terms = append(q.Terms, g...)
		parts[i] = strings.Join(g, " OR ")
		if len(g) > 1 {
			q.HasOR = true
			if len(groups) > 1 {
				parts[i] = "(" + parts[i] + ")"
			}
		}
	}
	// Explicit AND: FTS5 has no implicit AND before a parenthesised group.
	q.FTS5 = strings.Join(parts, " AND ")
	return q
}

type simpleWord struct {
	text   string
	quoted bool
}

// simpleWords splits a query into whitespace-separated words and "quoted
// phrases". A quote opens or closes a phrase wherever it is; an unclosed
// phrase runs to the end.
func simpleWords(query string) []simpleWord {
	var words []simpleWord
	var cur strings.Builder
	quoted := false
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, simpleWord{cur.String(), quoted})
			cur.Reset()
		}
	}
	for _, r := range query {
		if r == 0 {
			r = ' ' // FTS5 reads the query up to a NUL.
		}
		switch {
		case r == '"':
			flush()
			quoted = !quoted
		case !quoted && unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// searchable reports whether the default FTS5 tokenizer (unicode61) finds a
// token in s: its token characters are letters, numbers and private-use ones.
func searchable(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r)
	})
}
