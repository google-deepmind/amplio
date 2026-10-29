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
	"errors"
	"strings"
	"testing"
	"time"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/llm"
	"amplio/internal/workspace/plain"
)

// llmSeen is what llm_capture_test_agent was handed: the model in its Env.
type llmSeen struct {
	sid, model, spec string
	canResolve       bool
}

var llmCaptured = make(chan llmSeen, 16)

func init() {
	agent.Register("llm_capture_test_agent", func(env *agent.Env, cfg *agent.Config) (agent.Agent, error) {
		llmCaptured <- llmSeen{sid: cfg.SessionID, model: env.LLM.ModelID(), spec: env.LLMSpec, canResolve: env.ResolveLLM != nil}
		return &noopRunAgent{sid: cfg.SessionID}, nil
	}, agent.Traits{})
}

type noopRunAgent struct{ sid string }

func (a *noopRunAgent) Run(context.Context) error { return nil }
func (a *noopRunAgent) SessionID() string         { return a.sid }

// specFactory builds a mock provider whose ModelID is the spec it was built
// from, so a test can tell which spec a session ended up on.
func specFactory(spec string) (llm.Provider, error) {
	if strings.Contains(spec, "unbuildable") {
		return nil, errors.New("no credentials for this provider")
	}
	return &llm.MockProvider{Model: spec}, nil
}

func awaitLLMSeen(t *testing.T, sid string) llmSeen {
	t.Helper()
	select {
	case got := <-llmCaptured:
		if got.sid != sid {
			t.Fatalf("captured session %q, want %q", got.sid, sid)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("session %q was not launched", sid)
	}
	return llmSeen{}
}

// A revived session runs on the model its metadata records as its own, keeps
// it in its Env for its own sub-agents to inherit, and a session with none
// gets the run's model, as every session did before per-session models.
func TestRespawnSession_RestoresSessionModel(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const runID = "run-llm"
	if err := store.CreateRun(ctx, db.RunRecord{RunID: runID, Config: config.RunConfig{
		Task: "t", Workspace: ".", LLM: "vertex-claude:run-model",
	}}); err != nil {
		t.Fatal(err)
	}
	for sid, meta := range map[string]map[string]any{
		"own":     {llm.SessionMetadataKey: "vertex-gemini:own-model#fast"},
		"legacy":  nil,
		"emptied": {llm.SessionMetadataKey: ""},
	} {
		if err := store.CreateSession(ctx, db.SessionRecord{
			RunID: runID, SessionID: sid, AgentType: "llm_capture_test_agent",
			Status: db.SessionIdle, ParentID: "root", Metadata: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}

	mgr := NewRunManager(store, specFactory, NewRunRegistry(), plain.Factory)
	for _, c := range []struct{ sid, model, spec string }{
		{"own", "vertex-gemini:own-model#fast", "vertex-gemini:own-model#fast"},
		{"legacy", "vertex-claude:run-model", ""},
		{"emptied", "vertex-claude:run-model", ""},
	} {
		mgr.RespawnSession(runID, c.sid)
		got := awaitLLMSeen(t, c.sid)
		if got.model != c.model || got.spec != c.spec {
			t.Errorf("%s: revived on model %q with LLMSpec %q, want %q and %q", c.sid, got.model, got.spec, c.model, c.spec)
		}
		if !got.canResolve {
			t.Errorf("%s: revived without ResolveLLM, so its spawns could not pick a model", c.sid)
		}
	}
}

// A run's root starts on the run's model with no model of its own, and can
// resolve one for its sub-agents.
func TestStartRun_RootHasNoOwnModel(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewRunManager(store, specFactory, NewRunRegistry(), plain.Factory)
	if _, err := mgr.StartRun(context.Background(), StartRunConfig{
		RunConfig:     config.RunConfig{Task: "t", Workspace: ".", LLM: "vertex-claude:run-model", AgentType: "llm_capture_test_agent"},
		RootSessionID: "root",
	}); err != nil {
		t.Fatal(err)
	}
	got := awaitLLMSeen(t, "root")
	if got.model != "vertex-claude:run-model" || got.spec != "" || !got.canResolve {
		t.Errorf("root got model %q, LLMSpec %q, ResolveLLM set=%v; want the run's model, no own spec, a resolver",
			got.model, got.spec, got.canResolve)
	}
}

func TestResolveLLM(t *testing.T) {
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.AddCustomModel(ctx, "openai{base_url=http://localhost:4000/v1}:gpt-x#gpt"); err != nil {
		t.Fatal(err)
	}
	builds := map[string]int{}
	mgr := NewRunManager(store, func(spec string) (llm.Provider, error) {
		builds[spec]++
		return specFactory(spec)
	}, NewRunRegistry(), plain.Factory)
	mgr.SetModelMenu([]string{
		"vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive#opus",
		"vertex-gemini:gemini-3.6-flash",
	})

	for _, c := range []struct{ model, want string }{
		// Menu names give the entry verbatim: its args, endpoint and nickname.
		{"#opus", "vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive#opus"},
		{" opus ", "vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive#opus"},
		{"gemini-3.6-flash", "vertex-gemini:gemini-3.6-flash"},
		{"gpt", "openai{base_url=http://localhost:4000/v1}:gpt-x#gpt"}, // the UI-added half
		// Anything else runs exactly as written, on the menu or not.
		{"vertex-claude:claude-opus-5", "vertex-claude:claude-opus-5"},
		{"openai{base_url=http://127.0.0.1:9000/v1}:local-model", "openai{base_url=http://127.0.0.1:9000/v1}:local-model"},
	} {
		spec, p, err := mgr.resolveLLM(ctx, c.model)
		if err != nil {
			t.Errorf("resolveLLM(%q): %v", c.model, err)
			continue
		}
		if spec != c.want || p == nil || p.ModelID() != c.want {
			t.Errorf("resolveLLM(%q) = %q (provider for %v), want %q", c.model, spec, p, c.want)
		}
	}
	if n := builds["vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive#opus"]; n != 1 {
		t.Errorf("provider built %d times for a model asked for twice, want 1 (cached)", n)
	}

	// Neither a name nor a spec: refused, with what the menu offers.
	_, _, err = mgr.resolveLLM(ctx, "sonnet")
	if err == nil || !strings.Contains(err.Error(), "not a model spec") || !strings.Contains(err.Error(), "opus") {
		t.Errorf("resolveLLM(unknown name) error = %v, want a refusal listing the menu", err)
	}
	// A spec whose provider can't be built: refused, saying which.
	_, _, err = mgr.resolveLLM(ctx, "vertex-gemini:unbuildable")
	if err == nil || !strings.Contains(err.Error(), "build vertex-gemini:unbuildable") {
		t.Errorf("resolveLLM(unbuildable) error = %v, want the build failure", err)
	}
}
