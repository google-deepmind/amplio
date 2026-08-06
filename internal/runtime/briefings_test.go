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

package runtime

import (
	"context"
	"strings"
	"testing"

	"amplio/internal/config"
	"amplio/internal/db/sqlite"
	"amplio/internal/llm"
	"amplio/internal/workspace/plain"
)

// StartRun is the one place a run is created, so it is where a caller's
// selection is validated and frozen onto the run. Doing it there rather than in
// each caller is what keeps the UI, the CLI and the raw API agreeing.
func TestStartRun_ResolvesBriefings(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil },
		NewRunRegistry(), plain.Factory)

	start := func(sel []string) []string {
		t.Helper()
		id, err := mgr.StartRun(context.Background(), StartRunConfig{
			RunConfig:     config.RunConfig{Task: "t", Workspace: ".", LLM: "x", AgentType: "recover_test_agent"},
			RootSessionID: "root",
			Briefings:     sel,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return run.Config.Briefings
	}

	if got := start(nil); len(got) != 0 {
		t.Errorf("no selection stored %v, want none: briefings are opt-in", got)
	}
	if got := start([]string{"second-opinion"}); strings.Join(got, ",") != "second-opinion" {
		t.Errorf("explicit selection stored %v", got)
	}
	// A typo is dropped, not fatal: the run still starts.
	if got := start([]string{"second-opinion", "nope"}); strings.Join(got, ",") != "second-opinion" {
		t.Errorf("unknown name stored %v, want it dropped", got)
	}
}
