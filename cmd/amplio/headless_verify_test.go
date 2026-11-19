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
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/llm"
	"amplio/internal/verify"

	"github.com/spf13/cobra"
)

// --verify alone asks for the defaults; any --verify-* flag implies it.
func TestVerifyFlags(t *testing.T) {
	for _, c := range []struct {
		args      []string
		on        bool
		criteria  string
		maxRounds *int
	}{
		{nil, false, "", nil},
		{[]string{"--verify"}, true, "", nil},
		{[]string{"--verify-criteria", "tests pass"}, true, "tests pass", nil},
		{[]string{"--verify-max-rounds", "0"}, true, "", new(int)},
		{[]string{"--verify-model", "#judge"}, true, "", nil},
	} {
		var vf verifyFlags
		cmd := &cobra.Command{}
		vf.register(cmd, "")
		if err := cmd.ParseFlags(c.args); err != nil {
			t.Fatal(err)
		}
		r := vf.request(cmd)
		if (r != nil) != c.on {
			t.Errorf("%v: request = %+v, want on=%v", c.args, r, c.on)
			continue
		}
		if r != nil && (r.Criteria != c.criteria || (r.MaxRounds == nil) != (c.maxRounds == nil)) {
			t.Errorf("%v: request = %+v", c.args, r)
		}
	}
}

// resume --verify verifies the conclusion a --message asks for; without one
// it is refused.
func TestResumeVerifyNeedsMessage(t *testing.T) {
	noReport := false
	cmd := resumeCmd(&noReport)
	cmd.SetArgs([]string{"r", "--verify"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--verify goes with --message") {
		t.Errorf("resume --verify without --message = %v", err)
	}
}

// byRole is the root's model for a headless test whose verifier runs on it
// too ("the agent's own"): the root's requests get root's script and every
// other session's get verifier's, so neither depends on the order the two
// call the model in.
type byRole struct{ root, verifier *llm.MockProvider }

func (b byRole) pick(req llm.Request) *llm.MockProvider {
	if strings.HasSuffix(req.SessionID, "/"+config.RootAgentSessionID) {
		return b.root
	}
	return b.verifier
}

func (b byRole) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return b.pick(req).Call(ctx, req)
}

func (b byRole) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return b.pick(req).Stream(ctx, req)
}

func (b byRole) ModelID() string { return b.root.ModelID() }
func (b byRole) MaxTokens() int  { return b.root.MaxTokens() }

// finishWithin runs a headless execute to its end, failing the test if it is
// still running after a while: a mis-scripted run waits forever otherwise.
func finishWithin(t *testing.T, run func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run was still going after 30s")
	}
}

// concludeWith is a scripted turn that calls conclude with result.
func concludeWith(id, result string) llm.Response {
	return llm.Response{ToolCalls: []llm.ToolCall{{ID: id, Name: "conclude", Arguments: `{"result":"` + result + `"}`}}}
}

// verdictWith is a scripted verifier turn that concludes with a verdict.
func verdictWith(id, verdict, findings string) llm.Response {
	return llm.Response{ToolCalls: []llm.ToolCall{{ID: id, Name: "conclude",
		Arguments: `{"verdict":"` + verdict + `","findings":"` + findings + `"}`}}}
}

// A headless run with verification: the verifier (a sub-agent of the verifier
// type on the root's model) rejects the first answer and passes the second,
// which the root's conclude call waited for; the process logs the pass before
// the run is at rest.
func TestExecuteRun_Verified(t *testing.T) {
	models := byRole{
		root: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			concludeWith("c1", "Paris"),
			concludeWith("c2", "Paris: it is the seat of government."),
		}},
		verifier: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			verdictWith("v1", "FAIL", "no reason given"),
			verdictWith("v2", "PASS", "it is"),
		}},
	}
	cfg := restingRunConfig(t, models)
	logs := captureLogs(t, "text")
	finishWithin(t, func() error {
		return executeRun(cfg, "What is the capital of France?", t.TempDir(), "headless-test:agent", "",
			&verify.Request{}, true)
	})
	out := logs.String()
	pass, rest := strings.Index(out, `msg="verification passed" session=main-agent`), strings.Index(out, `msg="run at rest"`)
	if pass < 0 || rest < pass || !strings.Contains(out, "round=2") {
		t.Errorf("want `verification passed` (round 2) before `run at rest`:\n%s", out)
	}
	if r, v := models.root.CallCount(), models.verifier.CallCount(); r != 2 || v != 2 {
		t.Errorf("LLM calls: root %d, verifier %d; want 2 and 2", r, v)
	}

	// The verifier is of the verifier type, and its verdicts reached the root
	// as outcome records, not child_results.
	store, err := sqlite.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sessions, _ := store.ListSessions(ctx, testRunID(t, store))
	for _, s := range sessions {
		if verify.VerifierFor(&s) != "" && s.AgentType != verify.AgentType {
			t.Errorf("verifier %s is a %s, want a %s", s.SessionID, s.AgentType, verify.AgentType)
		}
	}
	st, _ := verify.Load(ctx, store, testRunID(t, store), config.RootAgentSessionID)
	var verdicts []string
	for _, e := range st.History {
		if e.Action == event.VerifyVerdict {
			verdicts = append(verdicts, e.Outcome.Verdict+": "+e.Outcome.Findings)
		}
	}
	if want := []string{"FAIL: no reason given", "PASS: it is"}; !slices.Equal(verdicts, want) {
		t.Errorf("outcome records = %q, want %q", verdicts, want)
	}
	// The root's verification summary, mirrored into its metadata for the UI
	// and written before the process exits, says it passed in round 2.
	root, err := store.GetSession(ctx, testRunID(t, store), config.RootAgentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sum := verify.SummaryFrom(root.Metadata); sum == nil || sum.Status != verify.StatusPassed || sum.Round != 2 {
		t.Errorf("root's verification summary = %+v, want passed in round 2", sum)
	}
}

// testRunID is the run a headless test started (restingRunConfig made "r").
func testRunID(t *testing.T, store db.Store) string {
	t.Helper()
	runs, _, err := store.ListRuns(context.Background(), db.ListRunsOpts{})
	for _, r := range runs {
		if r.RunID != "r" {
			return r.RunID
		}
	}
	t.Fatalf("runs = %v, %v; want one besides r", runs, err)
	return ""
}

// The same with a root that replies without tool calls: each reply is a
// candidate, the harness waits for its verdict (the verifier takes the next
// scripted turn), and the pass delivers the second.
func TestExecuteRun_VerifiedByReplies(t *testing.T) {
	models := byRole{
		root: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			{Content: "Paris"},
			{Content: "Paris: it is the seat of government."},
		}},
		verifier: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			verdictWith("v1", "FAIL", "no reason given"),
			verdictWith("v2", "PASS", "it is"),
		}},
	}
	cfg := restingRunConfig(t, models)
	logs := captureLogs(t, "text")
	finishWithin(t, func() error {
		return executeRun(cfg, "What is the capital of France?", t.TempDir(), "headless-test:agent", "",
			&verify.Request{}, true)
	})
	out := logs.String()
	pass, rest := strings.Index(out, `msg="verification passed" session=main-agent`), strings.Index(out, `msg="run at rest"`)
	if pass < 0 || rest < pass || !strings.Contains(out, "round=2") {
		t.Errorf("want `verification passed` (round 2) before `run at rest`:\n%s", out)
	}
	if r, v := models.root.CallCount(), models.verifier.CallCount(); r != 2 || v != 2 {
		t.Errorf("LLM calls: root %d, verifier %d; want 2 and 2", r, v)
	}
}

// resume --message --verify: the rounds run out, and the process says so.
func TestExecuteResume_VerifyExhausted(t *testing.T) {
	models := byRole{
		root: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			concludeWith("c1", "Lyon"),
		}},
		verifier: &llm.MockProvider{Model: "agent", Responses: []llm.Response{
			verdictWith("v1", "FAIL", "Lyon is not the capital"),
		}},
	}
	cfg := restingRunConfig(t, models)
	logs := captureLogs(t, "text")
	one := 1
	finishWithin(t, func() error {
		return executeResume(cfg, "r", "Answer again.", &verify.Request{MaxRounds: &one}, true)
	})
	if out := logs.String(); !strings.Contains(out, `msg="not verified after 1 rejection" session=main-agent`) {
		t.Errorf("no `not verified` line:\n%s", out)
	}
}

func runVerify(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := clientVerifyCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// client verify PUTs only the fields given (an unset --max-rounds takes the
// server's default, an explicit 0 means unbounded), --off DELETEs, and the
// state goes to stdout.
func TestClientVerify(t *testing.T) {
	var method, path, body string
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		state := "on"
		if r.Method == http.MethodDelete {
			state = "off"
		}
		_, _ = w.Write([]byte(`{"state":"` + state + `","criteria":"tests pass","max_rounds":3,"round":1,"version":2,"history":[]}`))
	})
	out, errOut, err := runVerify(t, "run1", "worker", "--criteria", "tests pass")
	if err != nil || method != http.MethodPut || path != "/api/runs/run1/sessions/worker/verify" || body != `{"criteria":"tests pass"}` {
		t.Errorf("set: %s %s %s (%v)", method, path, body, err)
	}
	if strings.TrimSpace(out) != "on" || !strings.Contains(errOut, "verification on for worker (v2; criteria: tests pass; round 1 of 3)") {
		t.Errorf("stdout %q, stderr %q", out, errOut)
	}
	// A server before versions reports none: no "v0".
	if got := describeVerify("worker", verify.Report{State: verify.StatusOn, Round: 1, MaxRounds: 3}); got != "verification on for worker (round 1 of 3)" {
		t.Errorf("without a version: %q", got)
	}
	if _, _, err := runVerify(t, "run1", "worker", "--max-rounds", "0"); err != nil || body != `{"max_rounds":0}` {
		t.Errorf("explicit 0: body %s (%v)", body, err)
	}
	if out, _, err := runVerify(t, "run1", "worker", "--off"); err != nil || method != http.MethodDelete || strings.TrimSpace(out) != "off" {
		t.Errorf("off: %s, stdout %q (%v)", method, out, err)
	}
	if _, _, err := runVerify(t, "run1", "worker", "--off", "--criteria", "x"); err == nil {
		t.Error("--off with --criteria accepted")
	}
}
