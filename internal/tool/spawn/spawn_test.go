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

package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/llm"
	"amplio/internal/session"
	"amplio/internal/tool"
	"amplio/internal/workspace"
	"amplio/internal/workspace/git"
)

type noopAgent struct{ id string }

func (a *noopAgent) Run(context.Context) error { return nil }
func (a *noopAgent) SessionID() string         { return a.id }

// linkCaptured records the workspace handed to the spawned child (set
// synchronously by the factory, before the child goroutine starts).
var linkCaptured workspace.Workspace

func init() {
	agent.Register("spawn_link_test_agent", func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		linkCaptured = env.Workspace
		return &noopAgent{id: cfg.SessionID}, nil
	}, agent.Traits{})
}

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "tester")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return repo
}

func TestSpawnLinkMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repo := gitRepo(t)

	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := db.NewRunID()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}

	env := &agent.Env{
		Store:     store,
		RunID:     runID,
		Names:     session.NewNameAllocator(store, runID),
		Registry:  session.NewRegistry(),
		Workspace: git.New(repo),
	}
	ex := makeExecutor(env, "parent")
	args, _ := json.Marshal(Params{Task: "t", AgentType: "spawn_link_test_agent", WorkspaceMode: "link"})
	res, err := ex(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("spawn errored: %s", res.Content)
	}

	if linkCaptured == nil {
		t.Fatal("child workspace not captured")
	}
	if linkCaptured.LinkedFrom() != repo {
		t.Errorf("child LinkedFrom = %q, want %q", linkCaptured.LinkedFrom(), repo)
	}
	if !strings.Contains(linkCaptured.Root(), "myrepo-amplio/worktrees/") {
		t.Errorf("child root = %q, want under myrepo-amplio/worktrees/", linkCaptured.Root())
	}
	// Named "<runID>-<session>", not the bare session id: session ids repeat
	// across runs, and the worktrees dir (like a CitC alias namespace) is shared
	// by every run on the repo.
	m := regexp.MustCompile(`Spawned sub-agent "([^"]+)"`).FindStringSubmatch(res.Content)
	if m == nil {
		t.Fatalf("no session id in spawn result: %s", res.Content)
	}
	if got, want := filepath.Base(linkCaptured.Root()), workspace.LinkName(runID, m[1]); got != want {
		t.Errorf("linked worktree name = %q, want %q", got, want)
	}
	if !workspace.PathExists(linkCaptured.Root()) {
		t.Errorf("linked worktree dir missing: %s", linkCaptured.Root())
	}
}

// modelCaptured records the Env handed to spawn_model_test_agent.
var modelCaptured *agent.Env

func init() {
	agent.Register("spawn_model_test_agent", func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		modelCaptured = env
		return &noopAgent{id: cfg.SessionID}, nil
	}, agent.Traits{})
}

func modelEnv(t *testing.T) *agent.Env {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := db.NewRunID()
	if err := store.CreateRun(context.Background(), db.RunRecord{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	modelCaptured = nil
	return &agent.Env{
		Store:    store,
		RunID:    runID,
		Names:    session.NewNameAllocator(store, runID),
		Registry: session.NewRegistry(),
		LLM:      &llm.MockProvider{Model: "parent"},
		LLMSpec:  "vertex-claude:parent",
	}
}

func spawnWith(t *testing.T, env *agent.Env, p Params) *tool.Result {
	t.Helper()
	p.Task, p.AgentType = "t", "spawn_model_test_agent"
	args, _ := json.Marshal(p)
	res, err := makeExecutor(env, "parent")(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Without a model the child runs on its parent's, as before the argument
// existed, and the resolver is not consulted.
func TestSpawnModel_InheritsByDefault(t *testing.T) {
	env := modelEnv(t)
	env.ResolveLLM = func(context.Context, string) (string, llm.Provider, error) {
		t.Error("ResolveLLM called for a spawn that named no model")
		return "", nil, errors.New("unexpected")
	}
	res := spawnWith(t, env, Params{Model: "  "})
	if res.IsError {
		t.Fatalf("spawn errored: %s", res.Content)
	}
	if modelCaptured == nil || modelCaptured.LLM != env.LLM || modelCaptured.LLMSpec != env.LLMSpec {
		t.Fatalf("child env = %+v, want the parent's LLM and LLMSpec", modelCaptured)
	}
	if strings.Contains(res.Content, "model=") {
		t.Errorf("result mentions a model nobody asked for: %s", res.Content)
	}
}

// A named model is resolved once, becomes the child's LLM and LLMSpec, and the
// result says which spec it runs on.
func TestSpawnModel_Resolved(t *testing.T) {
	env := modelEnv(t)
	gemini := &llm.MockProvider{Model: "gemini"}
	var asked []string
	env.ResolveLLM = func(_ context.Context, model string) (string, llm.Provider, error) {
		asked = append(asked, model)
		return "vertex-gemini:gemini-3.6-flash#fast", gemini, nil
	}
	res := spawnWith(t, env, Params{Model: " #fast "})
	if res.IsError {
		t.Fatalf("spawn errored: %s", res.Content)
	}
	if len(asked) != 1 || asked[0] != "#fast" {
		t.Errorf("ResolveLLM asked for %q, want [\"#fast\"]", asked)
	}
	if modelCaptured == nil || modelCaptured.LLM != gemini || modelCaptured.LLMSpec != "vertex-gemini:gemini-3.6-flash#fast" {
		t.Fatalf("child env = %+v, want the resolved provider and spec", modelCaptured)
	}
	if env.LLM.ModelID() != "parent" || env.LLMSpec != "vertex-claude:parent" {
		t.Errorf("parent env changed to %q / %q; the child must get a copy", env.LLM.ModelID(), env.LLMSpec)
	}
	if !strings.Contains(res.Content, "(type=spawn_model_test_agent, model=vertex-gemini:gemini-3.6-flash#fast)") {
		t.Errorf("result does not name the model: %s", res.Content)
	}
}

// A model that can't be resolved refuses the spawn before any child exists,
// and so does naming one where the server offers no resolver.
func TestSpawnModel_Refused(t *testing.T) {
	env := modelEnv(t)
	env.ResolveLLM = func(context.Context, string) (string, llm.Provider, error) {
		return "", nil, errors.New(`"sonnet" is not a model spec`)
	}
	res := spawnWith(t, env, Params{Model: "sonnet"})
	if !res.IsError || !strings.Contains(res.Content, `"sonnet" is not a model spec`) {
		t.Errorf("result = %+v, want an error carrying the resolver's reason", res)
	}
	env.ResolveLLM = nil
	res = spawnWith(t, env, Params{Model: "anything"})
	if !res.IsError || !strings.Contains(res.Content, "omit model") {
		t.Errorf("result = %+v, want an error saying to omit model", res)
	}
	if modelCaptured != nil {
		t.Error("a child was created for a refused spawn")
	}
	sessions, err := env.Store.ListSessions(context.Background(), env.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("refused spawns left %d sessions", len(sessions))
	}
}

// A model and a linked workspace together: the child gets both.
func TestSpawnModel_WithLinkedWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	env := modelEnv(t)
	repo := gitRepo(t)
	env.Workspace = git.New(repo)
	gemini := &llm.MockProvider{Model: "gemini"}
	env.ResolveLLM = func(context.Context, string) (string, llm.Provider, error) {
		return "vertex-gemini:gemini-3.6-flash", gemini, nil
	}
	res := spawnWith(t, env, Params{Model: "gemini-3.6-flash", WorkspaceMode: "link"})
	if res.IsError {
		t.Fatalf("spawn errored: %s", res.Content)
	}
	if modelCaptured == nil || modelCaptured.LLM != gemini || modelCaptured.LLMSpec != "vertex-gemini:gemini-3.6-flash" {
		t.Fatalf("child env LLM/LLMSpec = %v / %q, want the resolved model", modelCaptured.LLM, modelCaptured.LLMSpec)
	}
	if modelCaptured.Workspace.LinkedFrom() != repo {
		t.Errorf("child workspace LinkedFrom = %q, want %q", modelCaptured.Workspace.LinkedFrom(), repo)
	}
}
