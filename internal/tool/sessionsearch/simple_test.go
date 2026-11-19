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
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"amplio/internal/db"
)

func TestToFTS5(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
		terms int
		hasOR bool
	}{
		{`flux capacitor`, `"flux" AND "capacitor"`, 2, false},
		// Punctuation is matched as written, never parsed.
		{`final_result.json sub-agent ttl=90d 79% 1,1,4 pull/279`,
			`"final_result.json" AND "sub-agent" AND "ttl=90d" AND "79%" AND "1,1,4" AND "pull/279"`, 6, false},
		{`"$C" -readonly fin* data:x (alpha) ^beta`,
			`"$C" AND "-readonly" AND "fin*" AND "data:x" AND "(alpha)" AND "^beta"`, 6, false},
		// Phrases stay phrases; an unclosed one runs to the end.
		{`"seeded chat" sidebar`, `"seeded chat" AND "sidebar"`, 2, false},
		{`sidebar "seeded chat`, `"sidebar" AND "seeded chat"`, 2, false},
		{`say"hi there"now`, `"say" AND "hi there" AND "now"`, 3, false},
		{`ttl="90d" x`, `"ttl=" AND "90d" AND "x"`, 3, false},
		// OR joins the terms on either side.
		{`leak OR leaks OR leaking`, `"leak" OR "leaks" OR "leaking"`, 3, true},
		{`review post OR reply`, `"review" AND ("post" OR "reply")`, 3, true},
		{`a OR b c OR d e`, `("a" OR "b") AND ("c" OR "d") AND "e"`, 5, true},
		{`"jj git" OR "hg mail"`, `"jj git" OR "hg mail"`, 2, true},
		// AND changes nothing; stray operators are dropped; the last of two wins.
		{`alpha AND beta`, `"alpha" AND "beta"`, 2, false},
		{`OR alpha OR OR beta OR`, `"alpha" OR "beta"`, 2, true},
		{`AND alpha AND`, `"alpha"`, 1, false},
		{`alpha OR AND beta`, `"alpha" AND "beta"`, 2, false},
		{`alpha AND OR beta`, `"alpha" OR "beta"`, 2, true},
		// Only standalone, unquoted, uppercase OR and AND are operators.
		{`this or that`, `"this" AND "or" AND "that"`, 3, false},
		{`alpha "OR" beta`, `"alpha" AND "OR" AND "beta"`, 3, false},
		{`alpha NOT beta NEAR gamma`, `"alpha" AND "NOT" AND "beta" AND "NEAR" AND "gamma"`, 5, false},
		// A term without letters or digits would match nothing.
		{`alpha - ... % "" "--" beta`, `"alpha" AND "beta"`, 2, false},
		{`alpha OR - beta`, `"alpha" OR "beta"`, 2, true},
		{`- OR alpha`, `"alpha"`, 1, false},
		{`W6 → Lead naïve 日本語`, `"W6" AND "Lead" AND "naïve" AND "日本語"`, 4, false},
		// FTS5 reads the query up to a NUL.
		{"alpha\x00beta \"a\x00b\"", `"alpha" AND "beta" AND "a b"`, 3, false},
		// Nothing searchable.
		{``, ``, 0, false},
		{" \t\n", ``, 0, false},
		{`- ... % "" OR AND`, ``, 0, false},
	} {
		got := toFTS5(tc.query)
		if got.FTS5 != tc.want || len(got.Terms) != tc.terms || got.HasOR != tc.hasOR {
			t.Errorf("toFTS5(%q) = %q, %d terms, OR %v; want %q, %d, %v",
				tc.query, got.FTS5, len(got.Terms), got.HasOR, tc.want, tc.terms, tc.hasOR)
		}
	}
	if got := toFTS5(`final_result.json "seeded chat" x`).AnyTerm(); got != `"final_result.json" OR "seeded chat" OR "x"` {
		t.Errorf("AnyTerm = %q", got)
	}
}

// awkwardQueries are edge cases for the conversion; as written, most are FTS5
// syntax errors.
var awkwardQueries = []string{
	``, `"`, `""`, `"""`, `a"`, `"a`, `a"b"c"d`, `-`, `--x`, `-x -y`, `OR`, `AND`, `NOT`, `NEAR`,
	`a OR`, `OR a`, `a OR OR b`, `a AND AND b`, `a NOT b`, `NEAR(a b)`, `a*`, `*`, `^a`, `a:b`, `{a b}:c`,
	`(a`, `a)`, `()`, `a + b`, `a, b`, `x=1`, `50%`, `pr/1 a/b/c`, "a\x00b", "\"a\x00", "\x1a", "\xff\xfe a",
	"a\u0301", "\u0301", `''`, `'a b'`, "`a`", `a OR "" b`, `a OR - b`, `"" OR ""`,
}

// TestToFTS5_NeverAnFTS5Error runs every query agents sent session_search in
// real runs (testdata/queries.txt: one per line, internal names and IDs
// replaced), and the awkward ones, through the simple syntax against a real
// FTS5 index.
func TestToFTS5_NeverAnFTS5Error(t *testing.T) {
	body, err := os.ReadFile("testdata/queries.txt")
	if err != nil {
		t.Fatal(err)
	}
	real := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(real) < 300 {
		t.Fatalf("read %d queries; want the 309 in testdata", len(real))
	}
	store, runID := newStore(t)
	for _, q := range real {
		if toFTS5(q).FTS5 == "" {
			t.Errorf("toFTS5(%q) left nothing to search for", q)
		}
		if err := verifyNoFTS5Error(t.Context(), store, runID, q); err != nil {
			t.Error(err)
		}
	}
	for _, q := range awkwardQueries {
		if err := verifyNoFTS5Error(t.Context(), store, runID, q); err != nil {
			t.Error(err)
		}
	}
}

func FuzzToFTS5(f *testing.F) {
	for _, q := range awkwardQueries {
		f.Add(q)
	}
	store, runID := newStore(f)
	f.Fuzz(func(t *testing.T, q string) {
		if err := verifyNoFTS5Error(t.Context(), store, runID, q); err != nil {
			t.Error(err)
		}
	})
}

// verifyNoFTS5Error reports the first query toFTS5 makes of q, or its
// any-term form, that fails to run.
func verifyNoFTS5Error(ctx context.Context, store db.Store, runID, q string) error {
	sq := toFTS5(q)
	if sq.FTS5 == "" {
		return nil
	}
	for _, fq := range []string{sq.FTS5, sq.AnyTerm()} {
		if _, err := store.SearchEvents(ctx, runID, fq, db.SearchOpts{}); err != nil {
			return fmt.Errorf("query %q, as FTS5 %q: %v", q, fq, err)
		}
	}
	return nil
}
