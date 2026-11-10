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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/agent/critic"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	amlog "amplio/internal/log"
)

// --no-report on `headless run`: the run concludes, and no critic writes a
// report for it.
func TestExecuteRun_NoReportSkipsTheReport(t *testing.T) {
	for _, tc := range []struct {
		noReport bool
		want     int
	}{{false, 1}, {true, 0}} {
		t.Run(fmt.Sprintf("no-report=%v", tc.noReport), func(t *testing.T) {
			agent := &llm.MockProvider{Model: "agent", Responses: []llm.Response{{Content: "Paris"}}}
			cfg := restingRunConfig(t, agent)

			if err := executeRun(cfg, "What is the capital of France?", t.TempDir(), "headless-test:agent", "", tc.noReport); err != nil {
				t.Fatalf("executeRun: %v", err)
			}
			if n := agent.CallCount(); n != 1 {
				t.Fatalf("root made %d LLM calls, want 1", n)
			}
			store, err := sqlite.Open(cfg.DB)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			runs, _, err := store.ListRuns(ctx, db.ListRunsOpts{Limit: -1})
			if err != nil {
				t.Fatal(err)
			}
			reports := 0
			for _, r := range runs {
				rs, err := critic.AllReports(ctx, store, r.RunID)
				if err != nil {
					t.Fatal(err)
				}
				reports += len(rs)
			}
			if reports != tc.want {
				t.Errorf("got %d reports, want %d", reports, tc.want)
			}
		})
	}
}

// The harness's "time is up" turn: resume --message on a concluded run wakes
// the root with the message, and the root concludes again with its reply.
func TestExecuteResume_MessageRevivesConcludedRoot(t *testing.T) {
	agent := &llm.MockProvider{Model: "agent", Responses: []llm.Response{{Content: "Paris"}}}
	cfg := restingRunConfig(t, agent)
	logs := captureLogs(t, "text")

	const message = "Time is up: give your final answer now."
	if err := executeResume(cfg, "r", message, false); err != nil {
		t.Fatalf("executeResume: %v", err)
	}
	if strings.Contains(logs.String(), "did not respond") {
		t.Errorf("warned that the root did not respond, but it did:\n%s", logs)
	}

	calls := agent.Recorded()
	if len(calls) != 1 {
		t.Fatalf("root made %d LLM calls, want 1 (woken by the message)", len(calls))
	}
	msgs := calls[0].Messages
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleUser || !strings.Contains(last.Content, message) {
		t.Errorf("last message to the root = %s %q, want the user message", last.Role, last.Content)
	}

	store, err := sqlite.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	root, err := store.GetSession(ctx, "r", config.RootAgentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != db.SessionConcluded {
		t.Errorf("root status = %s, want concluded again", root.Status)
	}
	events, err := store.GetEvents(ctx, "r", config.RootAgentSessionID, db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var reply string
	for _, e := range events {
		if a, ok := e.Event.(*event.AssistantEvent); ok {
			reply = a.Content
		}
	}
	if reply != "Paris" {
		t.Errorf("root's last reply = %q, want the answer to the message", reply)
	}
}

// A message that revives nothing (here the root's workspace is gone, so it
// can't be respawned) means resume couldn't do its job: an error, at once. That
// holds for a concluded root and for an interrupted (ongoing) one, which
// RecoverRun also marks for recovery.
func TestExecuteResume_MessageThatRevivesNothingFails(t *testing.T) {
	for _, status := range []string{db.SessionConcluded, db.SessionOngoing} {
		t.Run(status, func(t *testing.T) {
			agent := &llm.MockProvider{Model: "agent"}
			cfg := restingRunConfig(t, agent)
			ctx := context.Background()
			store, err := sqlite.Open(cfg.DB)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSessionStatus(ctx, "r", config.RootAgentSessionID, status); err != nil {
				t.Fatal(err)
			}
			run, err := store.GetRun(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(run.Config.Workspace); err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			err = executeResume(cfg, "r", "hello", false)
			if err == nil || !strings.Contains(err.Error(), "did not revive") {
				t.Errorf("executeResume = %v, want an error saying the message revived nothing", err)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("executeResume took %s, want it to return once nothing is running", d)
			}
			if n := agent.CallCount(); n != 0 {
				t.Errorf("root made %d LLM calls, want none", n)
			}
		})
	}
}

// A root that is revived but crashes before replying is the run's outcome, not
// the command's: exit 0 (as for `run`), the crash recorded in the root's
// status, and a warning in the log.
func TestExecuteResume_MessageToARootThatCrashesWarns(t *testing.T) {
	cfg := restingRunConfig(t, failingLLM{})
	logs := captureLogs(t, "text")

	if err := executeResume(cfg, "r", "hello", false); err != nil {
		t.Fatalf("executeResume: %v, want nil (the crash is the run's outcome)", err)
	}
	store, err := sqlite.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root, err := store.GetSession(context.Background(), "r", config.RootAgentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != db.SessionCrashed {
		t.Errorf("root status = %s, want crashed", root.Status)
	}
	if !strings.Contains(logs.String(), `level=WARN msg="main-agent did not respond to the message"`) {
		t.Errorf("no warning that the root did not respond:\n%s", logs)
	}
}

func TestExecuteResume_MessageNeedsARoot(t *testing.T) {
	cfg := restingRunConfig(t, &llm.MockProvider{Model: "agent"})
	store, err := sqlite.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(context.Background(), db.RunRecord{RunID: "rootless"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	err = executeResume(cfg, "rootless", "hello", false)
	if err == nil || !strings.Contains(err.Error(), "has no "+config.RootAgentSessionID) {
		t.Fatalf("executeResume = %v, want an error naming the missing root", err)
	}
}

// failingLLM fails every call, as a provider outage past its retries does.
type failingLLM struct{}

func (failingLLM) Call(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("provider unavailable")
}
func (failingLLM) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("provider unavailable")
}
func (failingLLM) ModelID() string { return "failing" }
func (failingLLM) MaxTokens() int  { return 1000 }

// logSink is a log destination safe for concurrent writers.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureLogs sends the process logger's output, in --log-format=format, to a
// sink for the rest of the test.
func captureLogs(t *testing.T, format string) *logSink {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	logs := &logSink{}
	amlog.Init(amlog.Options{Level: slog.LevelInfo, Format: format, Writer: logs})
	return logs
}

// restingRunConfig makes a data dir holding run "r", whose root has
// concluded, and returns the config for the headless commands: model
// "headless-test:agent" (run r's) is agent, the system tiers are stubHQ, and
// recall is off.
func restingRunConfig(t *testing.T, agent llm.Provider) config.Config {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	config.SetDataDir(dataDir)
	t.Cleanup(func() { config.SetDataDir("") })

	const providerName = "headless-test"
	old, hadOld := providerRegistry[providerName]
	providerRegistry[providerName] = providerEntry{
		new: func(model string, _ int, _, _ url.Values) (llm.Provider, error) {
			if model == "agent" {
				return agent, nil
			}
			return stubHQ{}, nil
		},
	}
	t.Cleanup(func() {
		if hadOld {
			providerRegistry[providerName] = old
		} else {
			delete(providerRegistry, providerName)
		}
	})

	dbPath := filepath.Join(dataDir, "amplio.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "r", Config: config.RunConfig{
		Task:      "What is the capital of France?",
		Workspace: t.TempDir(),
		LLM:       providerName + ":agent",
		AgentType: config.DefaultAgentType,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, db.SessionRecord{
		RunID: "r", SessionID: config.RootAgentSessionID, AgentType: config.DefaultAgentType,
		Task: "What is the capital of France?", Status: db.SessionConcluded,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceStep(ctx, "r", config.RootAgentSessionID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		DB:            dbPath,
		SystemLLMHQ:   providerName + ":hq",
		SystemLLMFast: providerName + ":fast",
		EmbedModel:    config.EmbedModelNone, // no recall: no skill indexing, no embedding calls
	}
}
