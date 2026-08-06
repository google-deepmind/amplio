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
	"os"
	"testing"

	"amplio/internal/config"
	"amplio/internal/db/sqlite"
	"amplio/internal/llm"
	"amplio/internal/workspace/plain"
)

// A run owns its artifact directory from creation, so no later caller has to
// create it — that is what lets config.ArtifactDir stay a pure path helper.
func TestStartRun_CreatesArtifactDir(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil },
		NewRunRegistry(), plain.Factory)

	id, err := mgr.StartRun(context.Background(), StartRunConfig{
		RunConfig:     config.RunConfig{Task: "t", Workspace: ".", LLM: "x", AgentType: "recover_test_agent"},
		RootSessionID: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(config.ArtifactDir(id))
	if err != nil {
		t.Fatalf("artifact dir missing after StartRun: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%s is not a directory", config.ArtifactDir(id))
	}
}

// The mkdir failing is a real failure, not something to discover later when the
// agent's first write hits ENOENT: a file where the artifacts root should be
// makes every run creation fail loudly.
func TestStartRun_ArtifactDirFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	if err := os.WriteFile(dir+"/artifacts", []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewRunManager(store, func(string) (llm.Provider, error) { return &llm.MockProvider{Model: "m"}, nil },
		NewRunRegistry(), plain.Factory)

	_, err = mgr.StartRun(context.Background(), StartRunConfig{
		RunConfig:     config.RunConfig{Task: "t", Workspace: ".", LLM: "x", AgentType: "recover_test_agent"},
		RootSessionID: "root",
	})
	if err == nil {
		t.Fatal("StartRun succeeded with an unusable artifacts root")
	}
}
