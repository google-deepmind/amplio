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

package briefing

import (
	"context"
	"log/slog"

	"amplio/internal/db"
)

// ForRun composes the briefing text an agent in this run should carry. The
// selection was resolved and frozen when the run was created, so this reads
// names from the run record and turns them into text — the same shape as the
// AGENTS.md bootstrap, which also reads the run's persisted snapshot rather
// than carrying a copy on Env.
//
// Never fatal: a run whose briefing file has since been deleted starts without
// that section, loudly, rather than failing to start.
func ForRun(ctx context.Context, store db.Store, runID string, isRoot bool) string {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		slog.Warn("briefings: run config unreadable; continuing without", "run_id", runID, "error", err)
		return ""
	}
	if len(run.Config.Briefings) == 0 {
		return ""
	}
	text, missing := Compose(run.Config.Briefings, isRoot)
	if len(missing) > 0 {
		slog.Warn("briefings: named by the run but not found; skipped",
			"run_id", runID, "missing", missing)
	}
	return text
}
