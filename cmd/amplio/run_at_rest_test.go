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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"amplio/internal/runtime"
)

// A harness supervising a headless run keys on `run at rest` (msg and run_id),
// in text or JSON. waitForRun logs it once the run's last session is done.
func TestWaitForRun_LogsRunAtRest(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			logs := captureLogs(t, format)
			mgr := runtime.NewRunManager(nil, nil, runtime.NewRunRegistry(), nil)
			_, _, release, ok := mgr.RunRegistry().GetOrCreate("r").RegisterAndContext(context.Background(), "main-agent")
			if !ok {
				t.Fatal("register")
			}
			time.AfterFunc(100*time.Millisecond, release)

			waitForRun(context.Background(), mgr, "r")
			if mgr.RunRegistry().IsRunActive("r") {
				t.Fatal("waitForRun returned while the run was active")
			}
			if n := countRunAtRest(t, logs.String(), format, "r"); n != 1 {
				t.Errorf("logged `run at rest` %d times, want once:\n%s", n, logs)
			}
		})
	}
}

// An interrupted run was stopped, not finished: no `run at rest`. waitForRun
// returns once the cancelled root has exited, and nothing starts the run again
// before the process exits.
func TestWaitForRun_InterruptIsNotRest(t *testing.T) {
	logs := captureLogs(t, "text")
	mgr := runtime.NewRunManager(nil, nil, runtime.NewRunRegistry(), nil)
	runCtx, _, release, ok := mgr.RunRegistry().GetOrCreate("r").RegisterAndContext(context.Background(), "main-agent")
	if !ok {
		t.Fatal("register")
	}
	go func() { <-runCtx.Done(); release() }() // the root exits when cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	waitForRun(ctx, mgr, "r")
	if !strings.Contains(logs.String(), "interrupted, cancelling run") {
		t.Errorf("no interrupt line:\n%s", logs)
	}
	if n := countRunAtRest(t, logs.String(), "text", "r"); n != 0 {
		t.Errorf("logged `run at rest` %d times after an interrupt:\n%s", n, logs)
	}
	if mgr.RunRegistry().IsRunActive("r") {
		t.Error("waitForRun returned before the cancelled root exited")
	}
	if _, _, _, ok := mgr.RunRegistry().GetOrCreate("r").RegisterAndContext(context.Background(), "main-agent"); ok {
		t.Error("the root could be revived after the interrupt")
	}
}

// A session that doesn't exit when cancelled holds an interrupted process for
// stopGrace at most.
func TestWaitForRun_InterruptWaitIsBounded(t *testing.T) {
	captureLogs(t, "text")
	defer func(d time.Duration) { stopGrace = d }(stopGrace)
	stopGrace = 50 * time.Millisecond
	mgr := runtime.NewRunManager(nil, nil, runtime.NewRunRegistry(), nil)
	_, _, release, ok := mgr.RunRegistry().GetOrCreate("r").RegisterAndContext(context.Background(), "main-agent")
	if !ok {
		t.Fatal("register")
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	waitForRun(ctx, mgr, "r")
	if d := time.Since(start); d < stopGrace {
		t.Errorf("returned after %s, before stopGrace (%s), with the session still running", d, stopGrace)
	}
}

// Resuming a run that is already at rest says so with the same line.
func TestExecuteResume_AtRestRunLogsRunAtRest(t *testing.T) {
	cfg := restingRunConfig(t, stubHQ{})
	logs := captureLogs(t, "json")
	if err := executeResume(cfg, "r", "", nil, false); err != nil {
		t.Fatalf("executeResume: %v", err)
	}
	if n := countRunAtRest(t, logs.String(), "json", "r"); n != 1 {
		t.Errorf("logged `run at rest` %d times, want once:\n%s", n, logs)
	}
}

// countRunAtRest counts the `run at rest` records for runID in logs, parsed as
// a harness would: msg="run at rest" run_id=<id> in text, the fields in JSON.
func countRunAtRest(t *testing.T, logs, format, runID string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch format {
		case "json":
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("not a JSON log record: %q", line)
			}
			if rec["msg"] == "run at rest" && rec["run_id"] == runID {
				n++
			}
		default:
			if strings.Contains(line, `msg="run at rest" run_id=`+runID) {
				n++
			}
		}
	}
	return n
}
