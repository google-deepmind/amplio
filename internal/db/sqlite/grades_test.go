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

package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"amplio/internal/db"
)

func TestReportGrades(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	report := func(runID string, iteration, grade int) {
		t.Helper()
		if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil && iteration == 1 {
			t.Fatal(err)
		}
		if err := store.AppendObservation(ctx, db.ObservationRecord{
			RunID: runID, ObsID: fmt.Sprintf("run_report-%d", iteration), Kind: "run_report",
			Data:      map[string]any{"version": iteration, "grade": grade},
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	report("a", 1, 2)
	report("a", 3, 5) // out of order on purpose, and with a gap
	report("a", 2, 4)
	report("b", 1, 1)
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "none"}); err != nil {
		t.Fatal(err)
	}
	// A report that predates the grade field must not be dropped or crash.
	if err := store.AppendObservation(ctx, db.ObservationRecord{
		RunID: "none", ObsID: "run_report-1", Kind: "run_report",
		Data: map[string]any{"summary": "old report, no grade"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReportGrades(ctx, []string{"a", "b", "none", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["a"]) != 3 {
		t.Fatalf("run a: %d grades, want 3", len(got["a"]))
	}
	// Ordered by iteration, not by insertion or created_at.
	for i, want := range []struct{ iter, grade int }{{1, 2}, {2, 4}, {3, 5}} {
		if g := got["a"][i]; g.Iteration != want.iter || g.Grade != want.grade {
			t.Errorf("run a[%d] = iteration %d grade %d, want %d/%d", i, g.Iteration, g.Grade, want.iter, want.grade)
		}
	}
	if got["a"][0].CreatedAt.IsZero() {
		t.Error("created_at not populated")
	}
	if len(got["b"]) != 1 {
		t.Errorf("run b: %d grades, want 1", len(got["b"]))
	}
	if g := got["none"]; len(g) != 1 || g[0].Grade != 0 {
		t.Errorf("run with an ungraded report = %+v, want one entry with grade 0", g)
	}
	if _, ok := got["missing"]; ok {
		t.Error("a run with no reports should be absent from the map, not an empty slice")
	}
}

// No ids must not become "IN ()" or scan the table.
func TestReportGrades_EmptyInput(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	got, err := store.ReportGrades(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries for no ids", len(got))
	}
}
