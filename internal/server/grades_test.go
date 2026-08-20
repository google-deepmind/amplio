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

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"amplio/internal/config"
	"amplio/internal/db"
)

// writeReport stores one run_report iteration with the given grade.
func writeReport(t *testing.T, store db.Store, runID string, iteration, grade int) {
	t.Helper()
	err := store.AppendObservation(context.Background(), db.ObservationRecord{
		RunID:     runID,
		ObsID:     "run_report-" + strings.Repeat("i", iteration), // unique per iteration
		Kind:      "run_report",
		Data:      map[string]any{"version": iteration, "grade": grade, "summary": "s"},
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The grade series and the artifact dir must reach both DTOs: they are what lets
// a manager on another instance find a run's output and judge its iterations.
func TestRunDTOs_GradesAndArtifactDir(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "graded"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "ungraded"}); err != nil {
		t.Fatal(err)
	}
	writeReport(t, store, "graded", 2, 5)
	writeReport(t, store, "graded", 1, 3)

	t.Run("detail", func(t *testing.T) {
		var d struct {
			Grades      []struct{ Grade *string } `json:"grades"`
			ArtifactDir string                    `json:"artifact_dir"`
		}
		getJSON(t, h, "/api/runs/graded", &d)
		if len(d.Grades) != 2 {
			t.Fatalf("got %d grades, want 2 (one per iteration)", len(d.Grades))
		}
		// Oldest first, so a reader sees the trajectory in reading order.
		if got := deref(d.Grades[0].Grade); got != gradeNames[3] {
			t.Errorf("first grade = %q, want %q", got, gradeNames[3])
		}
		if got := deref(d.Grades[1].Grade); got != gradeNames[5] {
			t.Errorf("second grade = %q, want %q", got, gradeNames[5])
		}
		if d.ArtifactDir != config.ArtifactDir("graded") {
			t.Errorf("artifact_dir = %q, want %q", d.ArtifactDir, config.ArtifactDir("graded"))
		}
	})

	t.Run("summary", func(t *testing.T) {
		var page struct {
			Runs []struct {
				RunID       string                    `json:"run_id"`
				Grades      []struct{ Iteration int } `json:"grades"`
				ArtifactDir string                    `json:"artifact_dir"`
			} `json:"runs"`
		}
		getJSON(t, h, "/api/runs", &page)
		byID := map[string]int{}
		for _, r := range page.Runs {
			byID[r.RunID] = len(r.Grades)
			if r.ArtifactDir == "" {
				t.Errorf("%s: artifact_dir empty", r.RunID)
			}
		}
		if byID["graded"] != 2 {
			t.Errorf("graded run has %d grades in the list DTO, want 2", byID["graded"])
		}
		if byID["ungraded"] != 0 {
			t.Errorf("ungraded run has %d grades, want 0", byID["ungraded"])
		}
	})

	// An ungraded run must serialize as [] rather than null: callers iterate it.
	t.Run("empty is an array", func(t *testing.T) {
		raw := getRaw(t, h, "/api/runs/ungraded")
		if !strings.Contains(raw, `"grades":[]`) {
			t.Errorf("want an empty array for grades, got: %s", firstN(raw, 200))
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func getRaw(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func getJSON(t *testing.T, h http.Handler, path string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(getRaw(t, h, path)), into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
