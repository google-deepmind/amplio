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

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/verify"
)

// What the verification and loop forms read beyond their own APIs: which
// sessions can be verified, and how the chat labels ticks, notes and changes.

func TestServer_RunDetail_MarksInteractiveSessions(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	if err := store.CreateRun(ctx, db.RunRecord{RunID: testRun}); err != nil {
		t.Fatal(err)
	}
	for sid, typ := range map[string]string{"chatty-bot": "chatbot", "main-agent": "standard_agent"} {
		if err := store.CreateSession(ctx, db.SessionRecord{RunID: testRun, SessionID: sid, AgentType: typ, Status: db.SessionIdle}); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun, "")
	var detail runDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range detail.Sessions {
		got[s.SessionID] = s.Interactive
	}
	if !got["chatty-bot"] || got["main-agent"] || len(got) != 2 {
		t.Errorf("interactive = %v, want only chatty-bot", got)
	}
}

func TestServer_SettingsReportsNow(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	before := time.Now()
	_, body := doReq(t, http.MethodGet, ts.URL+"/api/settings", "")
	var got struct {
		Now time.Time `json:"now"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Now.Before(before.Add(-time.Second)) || got.Now.After(time.Now().Add(time.Second)) {
		t.Errorf("now = %v, want about %v (%s)", got.Now, before, body)
	}
}

func TestServer_RunDetail_NamesVerifiers(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	seedRun(t, store, db.SessionAwaiting, 1)
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: testRun, SessionID: "quick-rabbit", ParentID: "main-agent",
		AgentType: "standard_agent", Status: db.SessionOngoing, Metadata: map[string]any{verify.MetadataVerifierFor: "main-agent"}}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun, "")
	var detail runDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range detail.Sessions {
		got[s.SessionID] = s.VerifierFor
	}
	if got["quick-rabbit"] != "main-agent" || got["main-agent"] != "" {
		t.Errorf("verifier_for = %v, want quick-rabbit for main-agent only", got)
	}
}

func TestServer_ChatProjection_VerifyAndLoops(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	seedRun(t, store, db.SessionOngoing, 1)
	if err := store.FinalizeStep(ctx, testRun, "main-agent", 1, []event.Event{
		&event.VerifyEvent{Action: event.VerifySet, Criteria: "tests pass", MaxRounds: 3, By: event.ByOperator},
		&event.UserEvent{Content: "fix the build"},
		&event.VerifyEvent{Action: event.VerifyVerifying, By: event.ByHarness, Verifier: "quick-rabbit", Round: 1},
		&event.VerifyEvent{Action: event.VerifyReplace, Model: "vertex:y", MaxRounds: 1, By: "parent-7"},
		&event.VerifyEvent{Action: event.VerifyClear, By: event.ByOperator},
		&event.VerifyEvent{Action: event.VerifySet, Criteria: "it builds"},
		&event.UserEvent{Content: "[loop 2 · every 30m · tick 4] check the eval", Loop: &event.LoopTick{ID: "2", N: 4}},
		&event.ChildResultEvent{ChildSessionID: "worker-3", Verdict: db.SessionConcluded, Content: "done", Notes: []event.ResultNote{
			{Kind: event.NoteVerified, Text: "quick-rabbit (round 2)"},
			{Kind: event.NoteNotVerified, Text: "after 1 rejection. Latest findings: no test output"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/chat", "")
	var feed chatFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, m := range feed.Messages {
		lines = append(lines, m.Kind+"/"+m.From+": "+strings.ReplaceAll(m.Content, "\n", " | "))
	}
	want := []string{
		"environment/amplio: verification set by the operator | against: tests pass | verifier model: the agent's own · up to 3 rounds",
		"operator/user: fix the build",
		"environment/amplio: verification replaced by parent-7 | against: the agent's current request | verifier model: vertex:y · 1 round",
		"environment/amplio: verification cleared by the operator",
		"environment/amplio: verification set | against: it builds | verifier model: the agent's own · unbounded rounds",
		"operator/loop 2: [loop 2 · every 30m · tick 4] check the eval",
		"child_result/worker-3: done",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chat:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if tick := feed.Messages[5]; tick.Loop == nil || *tick.Loop != (event.LoopTick{ID: "2", N: 4}) {
		t.Errorf("tick bubble loop = %+v, want loop 2 tick 4", tick.Loop)
	}
	if plain := feed.Messages[1]; plain.Loop != nil {
		t.Errorf("a typed message carries loop %+v", plain.Loop)
	}
	wantNotes := []chatNote{
		{Kind: event.NoteVerified, Label: "verified by", Text: "quick-rabbit (round 2)"},
		{Kind: event.NoteNotVerified, Label: "not verified", Text: "after 1 rejection. Latest findings: no test output"},
	}
	if got := feed.Messages[6].Notes; len(got) != 2 || got[0] != wantNotes[0] || got[1] != wantNotes[1] {
		t.Errorf("notes = %+v, want %+v", got, wantNotes)
	}
}
