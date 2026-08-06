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

package observer

import (
	"context"
	"strings"
	"sync"
	"testing"

	"amplio/internal/agent"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
)

func TestPhaseSystemPrompt_BoundaryClauseByKind(t *testing.T) {
	auto, inter := phaseSystemPrompt(false), phaseSystemPrompt(true)
	for name, got := range map[string]string{"autonomous": auto, "interactive": inter} {
		if strings.Contains(got, boundaryClausePlaceholder) {
			t.Errorf("%s prompt still carries %s", name, boundaryClausePlaceholder)
		}
	}
	if !strings.Contains(auto, autonomousBoundaryClause) || strings.Contains(auto, interactiveBoundaryClause) {
		t.Error("autonomous prompt does not carry exactly the autonomous clause")
	}
	if !strings.Contains(inter, interactiveBoundaryClause) || strings.Contains(inter, autonomousBoundaryClause) {
		t.Error("interactive prompt does not carry exactly the interactive clause")
	}
	// Everything except the clause is shared: swapping one clause for the other
	// must reproduce the counterpart exactly, or the two prompts have drifted.
	if strings.Replace(auto, autonomousBoundaryClause, interactiveBoundaryClause, 1) != inter {
		t.Error("the two prompts differ somewhere other than the boundary clause")
	}
}

// capturingHQ records the system prompt of the last phase call.
type capturingHQ struct {
	mu      sync.Mutex
	prompts []string
}

func (c *capturingHQ) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, req.SystemPrompt)
	c.mu.Unlock()
	if strings.Contains(req.SystemPrompt, "phase-level evaluation") {
		return &llm.Response{Content: `{"title":"p","summary":"s","end_step":999}`}, nil
	}
	return &llm.Response{Content: `{"summary":"did the thing","status_tag":"progressing"}`}, nil
}
func (c *capturingHQ) Stream(context.Context, llm.Request) (llm.Stream, error) { return nil, nil }
func (c *capturingHQ) ModelID() string                                         { return "capturing" }
func (c *capturingHQ) MaxTokens() int                                          { return 1000 }

func (c *capturingHQ) phasePrompt(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.prompts {
		if strings.Contains(p, "phase-level evaluation") {
			return p
		}
	}
	t.Fatal("no phase-summary call was made")
	return ""
}

// TestObserver_PhasePromptFollowsAgentTraits pins the wiring end to end: the
// clause the summarizer actually receives is chosen by the agent type's
// registered Interactive trait, not by anything about the session itself.
func TestObserver_PhasePromptFollowsAgentTraits(t *testing.T) {
	agent.Register("phase_kind_interactive", nil, agent.Traits{Interactive: true})
	agent.Register("phase_kind_autonomous", nil, agent.Traits{})

	for _, tc := range []struct {
		agentType string
		want      string
		notWant   string
	}{
		{"phase_kind_interactive", interactiveBoundaryClause, autonomousBoundaryClause},
		{"phase_kind_autonomous", autonomousBoundaryClause, interactiveBoundaryClause},
	} {
		t.Run(tc.agentType, func(t *testing.T) {
			store := newStore(t)
			ctx := context.Background()
			must(t, store.CreateRun(ctx, db.RunRecord{RunID: testRunID}))
			must(t, store.CreateSession(ctx, db.SessionRecord{
				RunID: testRunID, SessionID: "s", AgentType: tc.agentType, Status: db.SessionOngoing,
			}))
			for step := 1; step <= 3; step++ {
				must(t, store.FinalizeStep(ctx, testRunID, "s", step,
					[]event.Event{&event.AssistantEvent{Content: strings.Repeat("x", 100_000)}}))
			}
			hq := &capturingHQ{}
			obs := New(store, fakeFast{}, hq, 1)
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			obs.Start(cctx)
			obs.Stop(cctx)

			got := hq.phasePrompt(t)
			if !strings.Contains(got, tc.want) {
				t.Errorf("phase prompt is missing the expected boundary clause for %s", tc.agentType)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("phase prompt carries the wrong boundary clause for %s", tc.agentType)
			}
		})
	}
}
