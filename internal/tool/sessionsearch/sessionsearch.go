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

// Package sessionsearch provides full-text search over session events.
package sessionsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"amplio/internal/db"
	"amplio/internal/tool"
	"amplio/internal/util"
)

type Params struct {
	Query     string `json:"query" jsonschema:"required" jsonschema_description:"Words to find, all of which must match; each matches as written, punctuation and all (final_result.json, sub-agent). \"Quoted phrases\" match in sequence; a OR b matches either."`
	Syntax    string `json:"syntax,omitempty" jsonschema:"enum=simple,enum=fts5" jsonschema_description:"simple (default), or fts5 to pass the query to SQLite FTS5 as written, for NOT, NEAR, prefix* and grouping."`
	SessionID string `json:"session_id,omitempty" jsonschema_description:"Limit search to a specific session"`
	StepMin   *int   `json:"step_min,omitempty" jsonschema_description:"Inclusive lower bound on step number"`
	StepMax   *int   `json:"step_max,omitempty" jsonschema_description:"Inclusive upper bound on step number"`
	Limit     int    `json:"limit,omitempty" jsonschema_description:"Max results (default 20, max 100)"`
	RunID     string `json:"run_id,omitempty" jsonschema_description:"Optional: a prior run's id to search instead of the current run."`
}

func New(store db.Store, runID string) *tool.Tool {
	return &tool.Tool{
		Name:        "session_search",
		Description: "Full-text search over events in a run (the current run, or a prior run via run_id). Returns matching events ranked by relevance.",
		ParamType:   &Params{},
		Execute: func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
			params, errResult := tool.ParseArgs[Params](args)
			if errResult != nil {
				return errResult, nil
			}
			rid := runID
			if params.RunID != "" {
				rid = params.RunID
			}
			limit := params.Limit
			if limit <= 0 {
				limit = 20
			}
			if limit > 100 {
				limit = 100
			}
			query := params.Query
			var simple *simpleQuery
			switch params.Syntax {
			case "", "simple":
				q := toFTS5(params.Query)
				if q.FTS5 == "" {
					return &tool.Result{Content: fmt.Sprintf("Nothing to search for in %q: no word or phrase in it has a letter or digit.", params.Query)}, nil
				}
				query, simple = q.FTS5, &q
			case "fts5":
			default:
				return &tool.Result{Content: fmt.Sprintf("Error: syntax %q: want simple or fts5.", params.Syntax), IsError: true}, nil
			}
			fail := func(err error) (*tool.Result, error) {
				msg := fmt.Sprintf("Error: %s", err)
				if simple == nil {
					msg += "\nsyntax=fts5 passes the query to SQLite FTS5 as written; the default simple syntax takes plain words."
				}
				return &tool.Result{Content: msg, IsError: true}, nil
			}

			results, err := search(ctx, store, rid, query, params, limit)
			if err != nil {
				return fail(err)
			}
			header := fmt.Sprintf("%d result(s) for %q:", len(results), params.Query)
			// Every term must match, which a long list of words often defeats.
			if len(results) == 0 && simple != nil && len(simple.Terms) > 1 && !simple.HasOR {
				if results, err = search(ctx, store, rid, simple.AnyTerm(), params, limit); err != nil {
					return fail(err)
				}
				header = fmt.Sprintf("No event matches all %d terms of %q; closest matches (any of the terms, best first):",
					len(simple.Terms), params.Query)
			}
			if len(results) == 0 {
				return &tool.Result{Content: fmt.Sprintf("No results for query %q.", params.Query)}, nil
			}

			var b strings.Builder
			b.WriteString(header + "\n")
			for _, e := range results {
				// Rune-safe truncation: event content is frequently non-ASCII
				// (agent prose, file contents), so a byte slice could split a rune.
				content := util.TruncateRunes(e.Event.ToText(), 150)
				content = strings.ReplaceAll(content, "\n", " ")
				fmt.Fprintf(&b, "  session=%s step=%d type=%-12s %s\n",
					e.SessionID, e.Step, e.Event.EventType(), content)
			}
			return &tool.Result{Content: b.String()}, nil
		},
	}
}

// maxSearchRows bounds how many matches search reads to fill limit.
const maxSearchRows = 5000

// search returns the best limit matches of the FTS5 query that this binary
// knows. The full-text index covers a newer binary's events too (it indexes
// the raw JSON), so when some of the matches are those, it reads further until
// limit known ones are found or the matches run out.
func search(ctx context.Context, store db.Store, runID, query string, p *Params, limit int) ([]db.EventRecord, error) {
	for fetch := limit; ; fetch *= 2 {
		recs, err := store.SearchEvents(ctx, runID, query, db.SearchOpts{
			SessionID: p.SessionID,
			StepMin:   p.StepMin,
			StepMax:   p.StepMax,
			Limit:     fetch,
		})
		if err != nil {
			return nil, err
		}
		known := db.KnownEvents(recs)
		if len(known) >= limit || len(recs) < fetch || fetch >= maxSearchRows {
			return known[:min(limit, len(known))], nil
		}
	}
}
