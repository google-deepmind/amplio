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

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/verify"
)

func verifyReport(t *testing.T, method, url, reqBody string) verify.Report {
	t.Helper()
	code, body := doReq(t, method, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var r verify.Report
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// GET reads a session's verification openly; PUT sets and then replaces it,
// DELETE clears it, both with the token. Setting it on a concluded session
// records it without reviving the session.
func TestServer_Verify(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	srv.mgr.SetVerifyDefaults(config.VerifyDefaults{MaxRounds: 3})
	seedRun(t, store, db.SessionConcluded, 1)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	path := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/verify"

	if r := verifyReport(t, http.MethodGet, path, ""); r.State != verify.StatusOff || len(r.History) != 0 {
		t.Errorf("before = %+v", r)
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if code, _ := doReq(t, m, path, `{}`); code != http.StatusUnauthorized {
			t.Errorf("%s without the token: %d, want 401", m, code)
		}
	}
	r := verifyReport(t, http.MethodPut, path+"?token=secret", `{"criteria":"tests pass"}`)
	if r.State != verify.StatusOn || r.Criteria != "tests pass" || r.MaxRounds != 3 || r.Round != 1 ||
		len(r.History) != 1 || r.History[0].By != event.ByOperator {
		t.Errorf("after PUT = %+v", r)
	}
	if s, _ := store.GetSession(context.Background(), testRun, "main-agent"); s.Status != db.SessionConcluded {
		t.Errorf("setting verification revived the session: %s", s.Status)
	}
	r = verifyReport(t, http.MethodPut, path+"?token=secret", `{"max_rounds":0}`)
	if r.State != verify.StatusOn || r.Criteria != "" || r.MaxRounds != 0 || len(r.History) != 2 || r.History[1].Action != event.VerifyReplace ||
		r.Version != 2 || r.History[0].Version != 1 || r.History[1].Version != 2 {
		t.Errorf("after the second PUT = %+v, want v2", r)
	}
	if _, body := doReq(t, http.MethodGet, path, ""); !strings.Contains(string(body), `"version":2,"candidate":0`) {
		t.Errorf("the report's JSON = %s, want the version and candidate", body)
	}
	for _, bad := range []string{`{"max_rounds":-1}`, `{"model":"not a spec"}`, `{`} {
		if code, body := doReq(t, http.MethodPut, path+"?token=secret", bad); code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d %s, want 400", bad, code, body)
		}
	}
	if code, _ := doReq(t, http.MethodPut, ts.URL+"/api/runs/"+testRun+"/sessions/nobody/verify?token=secret", `{}`); code != http.StatusNotFound {
		t.Errorf("PUT on a missing session: %d, want 404", code)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if code, _ := doReq(t, m, ts.URL+"/api/runs/"+testRun+"/sessions/nobody/verify?token=secret", ""); code != http.StatusNotFound {
			t.Errorf("%s on a missing session: %d, want 404", m, code)
		}
	}
	// A verifier and an interactive agent can't be verified.
	ctx := context.Background()
	for _, sr := range []db.SessionRecord{
		{SessionID: "quick-rabbit", ParentID: "main-agent", AgentType: "standard_agent", Status: db.SessionOngoing,
			Metadata: map[string]any{verify.MetadataVerifierFor: "main-agent"}},
		{SessionID: config.ChatbotSessionID, AgentType: config.ChatbotAgentType, Status: db.SessionIdle},
	} {
		sr.RunID = testRun
		if err := store.CreateSession(ctx, sr); err != nil {
			t.Fatal(err)
		}
		if code, body := doReq(t, http.MethodPut, ts.URL+"/api/runs/"+testRun+"/sessions/"+sr.SessionID+"/verify?token=secret", `{}`); code != http.StatusBadRequest {
			t.Errorf("PUT on %s: %d %s, want 400", sr.SessionID, code, body)
		}
	}
	if r := verifyReport(t, http.MethodDelete, path+"?token=secret", ""); r.State != verify.StatusOff ||
		len(r.History) != 3 || r.History[2].Action != event.VerifyClear {
		t.Errorf("after DELETE = %+v", r)
	}
}

// A message can carry verification: it is recorded before the message, so
// the session wakes with it in place; a bad request sends nothing.
func TestServer_SendMessageWithVerify(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	path := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/message?token=secret"

	if code, _ := doReq(t, http.MethodPost, path, `{"content":"x","verify":{"max_rounds":-2}}`); code != http.StatusBadRequest {
		t.Errorf("bad verify: %d, want 400", code)
	}
	if code, body := doReq(t, http.MethodPost, path, `{"content":"now with tests","verify":{"criteria":"go test passes"}}`); code != http.StatusAccepted {
		t.Fatalf("send: %d %s", code, body)
	}
	evs, _ := store.GetEvents(context.Background(), testRun, "main-agent", db.EventFilter{})
	var kinds []string
	for _, e := range evs[:2] {
		kinds = append(kinds, e.Event.EventType())
	}
	if len(evs) < 2 || strings.Join(kinds, ",") != "verify,user" || evs[0].Event.(*event.VerifyEvent).Criteria != "go test passes" {
		t.Errorf("stream starts %v, want the verify event, then the message", kinds)
	}
}

// A new run can start with verification on its root; an interactive run
// can't, and a bad request creates nothing.
func TestServer_StartRunWithVerify(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := ts.URL + "/api/runs?token=secret"
	for _, bad := range []string{
		`{"interactive":true,"message":"hi","verify":{}}`,
		`{"task":"t","verify":{"max_rounds":-1}}`,
	} {
		if code, _ := doReq(t, http.MethodPost, url, bad); code != http.StatusBadRequest {
			t.Errorf("start %s: %d, want 400", bad, code)
		}
	}
	if runs, _, _ := store.ListRuns(context.Background(), db.ListRunsOpts{Limit: -1}); len(runs) != 0 {
		t.Errorf("bad requests created %d runs", len(runs))
	}
	code, body := doReq(t, http.MethodPost, url, `{"task":"t","verify":{"criteria":"is correct","max_rounds":2}}`)
	if code != http.StatusCreated {
		t.Fatalf("start: %d %s", code, body)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(body, &created)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := verify.Load(context.Background(), store, created.RunID, config.RootAgentSessionID)
		if len(st.History) > 0 {
			if h := st.History[0]; h.Action != event.VerifySet || h.Criteria != "is correct" || h.MaxRounds != 2 || h.By != event.ByOperator {
				t.Errorf("root's verification = %+v", h)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the root never got its verification")
}

// A misspelt field in a verify request is refused, not dropped: dropping it
// would turn verification on with the defaults in place of what was asked.
// Nothing is recorded, sent or started.
func TestServer_VerifyBodiesRefuseUnknownFields(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	srv.mgr.SetVerifyDefaults(config.VerifyDefaults{MaxRounds: 3})
	seedRun(t, store, db.SessionConcluded, 1)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/"
	before, _ := store.GetEvents(context.Background(), testRun, "main-agent", db.EventFilter{})
	for _, c := range []struct{ method, url, body, field string }{
		{http.MethodPut, base + "verify?token=secret", `{"criterion":"tests pass","max_round":1}`, "criterion"},
		{http.MethodPost, base + "message?token=secret", `{"content":"x","verify":{"critera":"y"}}`, "critera"},
		{http.MethodPost, ts.URL + "/api/runs?token=secret", `{"task":"t","verify":{"max_round":1}}`, "max_round"},
	} {
		code, body := doReq(t, c.method, c.url, c.body)
		if code != http.StatusBadRequest || !strings.Contains(string(body), `unknown field \"`+c.field+`\"`) {
			t.Errorf("%s %s %s: %d %s, want 400 naming the field", c.method, c.url, c.body, code, body)
		}
	}
	if after, _ := store.GetEvents(context.Background(), testRun, "main-agent", db.EventFilter{}); len(after) != len(before) {
		t.Errorf("refused requests wrote %d events", len(after)-len(before))
	}
	if runs, _, _ := store.ListRuns(context.Background(), db.ListRunsOpts{Limit: -1}); len(runs) != 1 {
		t.Errorf("runs = %d, want only the seeded one", len(runs))
	}
}

// A verifier can't be a run's root. It is the client's error, a 400, and is
// refused before anything is done for the run: before its workspace is
// resolved (a bad one isn't what is reported) and before a run is created.
func TestServer_StartRunRefusesVerifierRoot(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, body := range []string{
		`{"task":"t","agent":"verifier"}`,
		`{"task":"t","agent":"verifier","workspace":"/no/such/workspace/dir"}`,
	} {
		code, resp := doReq(t, http.MethodPost, ts.URL+"/api/runs?token=secret", body)
		if code != http.StatusBadRequest || !strings.Contains(string(resp), "can't run on its own") {
			t.Errorf("start %s: %d %s, want 400 refusing the verifier", body, code, resp)
		}
	}
	if runs, _, _ := store.ListRuns(context.Background(), db.ListRunsOpts{Limit: -1}); len(runs) != 0 {
		t.Errorf("refused requests created %d runs", len(runs))
	}
}

// /api/settings reports the [verify] defaults, openly.
func TestServer_Settings(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.mgr.SetVerifyDefaults(config.VerifyDefaults{Model: "vertex-claude:judge", MaxRounds: 5})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	code, body := doReq(t, http.MethodGet, ts.URL+"/api/settings", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"verify":{"max_rounds":5,"model":"vertex-claude:judge"}`) {
		t.Errorf("settings = %d %s", code, body)
	}
}
