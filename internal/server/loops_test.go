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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
	"amplio/internal/eventstream"
	"amplio/internal/llm"
	"amplio/internal/loop"
	"amplio/internal/runtime"
	"amplio/internal/verify"
	"amplio/internal/workspace/plain"
)

// newLoopServer is newTestServer with a started loop scheduler (real clock),
// whose loops start verification versions as serve's do.
func newLoopServer(t *testing.T) (*httptest.Server, *Server, db.Store) {
	t.Helper()
	srv, _, store := newTestServer(t)
	sched := loop.New(store, loop.Options{
		Verify: func(ctx context.Context, runID, sessionID, loopID string, v *event.LoopVerify,
			write func(*event.VerifyEvent, int) error) (int, error) {
			return verify.Tick(ctx, store, srv.mgr.RunRegistry().GetOrCreate(runID), runID, sessionID, loopID, v, write)
		},
	})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Shutdown)
	srv.SetLoops(sched, LoopDefaults{Expiry: 7 * 24 * time.Hour})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, store
}

// loopBody is a POST body: every minute, now first, n ticks.
func loopBody(t *testing.T, prompt string, f loop.Form) string {
	t.Helper()
	s, err := f.Schedule(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"prompt": prompt, "schedule": s})
	return string(b)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// streamOf is main-agent's stream.
func streamOf(t *testing.T, store db.Store, runID string) []event.Event {
	t.Helper()
	recs, err := store.GetEvents(context.Background(), runID, "main-agent", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]event.Event, len(recs))
	for i, r := range recs {
		out[i] = r.Event
	}
	return out
}

func countTicks(evs []event.Event) int {
	n := 0
	for _, e := range evs {
		if u, ok := e.(*event.UserEvent); ok && u.Loop != nil {
			n++
		}
	}
	return n
}

func loopActions(evs []event.Event) []string {
	var out []string
	for _, e := range evs {
		if l, ok := e.(*event.LoopEvent); ok {
			out = append(out, l.Action)
		}
	}
	return out
}

func TestServer_LoopsAuth(t *testing.T) {
	t.Parallel()
	ts, _, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops"
	body := loopBody(t, "p", loop.Form{Every: time.Hour})
	for _, req := range []struct{ method, url, body string }{
		{http.MethodPost, base, body},
		{http.MethodPost, base + "?token=wrong", body},
		{http.MethodPatch, base + "/1", `{"prompt":"x"}`},
		{http.MethodDelete, base + "/1", ""},
	} {
		if code, _ := doReq(t, req.method, req.url, req.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", req.method, req.url, code)
		}
	}
	if evs := streamOf(t, store, testRun); len(loopActions(evs)) != 0 {
		t.Errorf("an unauthorized write changed loops: %v", loopActions(evs))
	}
	// Reads are open, like the rest of a run's reads.
	for _, path := range []string{base, ts.URL + "/api/settings"} {
		if code, b := doReq(t, http.MethodGet, path, ""); code != http.StatusOK {
			t.Errorf("GET %s without token: %d %s", path, code, b)
		}
	}
}

func TestServer_LoopsValidation(t *testing.T) {
	t.Parallel()
	ts, _, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops"
	good := `"schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=HOURLY"}`
	for _, tc := range []struct {
		name, method, url, body string
		want                    int
		wantIn                  string
	}{
		{"malformed", http.MethodPost, base, `{"prompt":`, 400, "invalid JSON"},
		{"unknown field", http.MethodPost, base, `{"prompt":"p","every":"30m",` + good + `}`, 400, "unknown field"},
		{"no prompt", http.MethodPost, base, `{` + good + `}`, 400, "prompt is required"},
		{"prompt too long", http.MethodPost, base, `{"prompt":"` + strings.Repeat("x", 70000) + `",` + good + `}`, 400, "limit is"},
		{"kind", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"cron","spec":"*/5 * * * *"}}`, 400, "only kind"},
		{"unparseable", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=OFTEN"}}`, 400, "undefined frequency"},
		{"spec too long", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"` + strings.Repeat("x", 5000) + `"}}`, 400, "limit is"},
		{"too fast", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=MINUTELY;BYSECOND=0,20,40"}}`, 400, "BYSECOND other than 0"},
		{"never lands", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=2;BYHOUR=1"}}`, 400, "never reaches"},
		{"over", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=HOURLY;UNTIL=20991110T000000Z"}}`, 400, "no tick after now"},
		{"ancient", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20201110T120000Z\nRRULE:FREQ=HOURLY"}}`, 400, "more than a year before now"},
		{"far future", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:99991231T230000Z\nRRULE:FREQ=DAILY"}}`, 400, "more than 100 years from now"},
		{"value spelt twice", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=DAILY;BYHOUR=9,09"}}`, 400, "BYHOUR lists 9 twice"},
		{"count and until", http.MethodPost, base, `{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=HOURLY;COUNT=5;UNTIL=21000101T000000Z"}}`, 400, "not both"},
		{"no session", http.MethodPost, ts.URL + "/api/runs/" + testRun + "/sessions/nobody/loops", `{"prompt":"p",` + good + `}`, 404, "no session"},
		{"edit unknown", http.MethodPatch, base + "/9", `{"prompt":"x"}`, 404, "no active loop"},
		{"edit nothing", http.MethodPatch, base + "/9", `{}`, 400, "nothing to change"},
		{"stop unknown", http.MethodDelete, base + "/9", "", 404, "no active loop"},
		{"list unknown session", http.MethodGet, ts.URL + "/api/runs/" + testRun + "/sessions/nobody/loops", "", 404, "no session"},
	} {
		sep := "?"
		if strings.Contains(tc.url, "?") {
			sep = "&"
		}
		code, body := doReq(t, tc.method, tc.url+sep+"token=secret", tc.body)
		if code != tc.want || !strings.Contains(string(body), tc.wantIn) {
			t.Errorf("%s: %d %s; want %d containing %q", tc.name, code, body, tc.want, tc.wantIn)
		}
	}
	if evs := streamOf(t, store, testRun); len(loopActions(evs)) != 0 {
		t.Errorf("a refused request wrote %v", loopActions(evs))
	}
}

func TestServer_LoopsLifecycle(t *testing.T) {
	t.Parallel()
	ts, _, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops"

	code, body := doReq(t, http.MethodPost, base+"?token=secret", loopBody(t, "check the eval", loop.Form{Every: time.Minute, SendNow: true, Times: 3}))
	if code != http.StatusCreated {
		t.Fatalf("POST: %d %s", code, body)
	}
	var created loopDTO
	if err := json.Unmarshal(body, &created); err != nil || created.ID != "1" || created.Summary != "every 1m, 3 times" || created.NextTick == nil {
		t.Fatalf("POST answer %s", body)
	}
	waitFor(t, "the send-now tick", func() bool { return countTicks(streamOf(t, store, testRun)) == 1 })

	// The scheduler records the tick in what GET lists just after the tick's
	// write, so the list can lag the stream for a moment: wait for it.
	var listed []loopDTO
	waitFor(t, "the tick in the list", func() bool {
		_, body = doReq(t, http.MethodGet, base, "")
		listed = nil
		return json.Unmarshal(body, &listed) == nil && len(listed) == 1 && listed[0].Ticks == 1 && listed[0].LastTick != nil
	})
	if code, body := doReq(t, http.MethodPatch, base+"/1?token=secret", `{"prompt":"check it again"}`); code != http.StatusOK || !strings.Contains(string(body), "check it again") {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	if code, body := doReq(t, http.MethodDelete, base+"/1?token=secret", ""); code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", code, body)
	}
	if _, body := doReq(t, http.MethodGet, base, ""); strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("GET after stop: %s", body)
	}
	if got := strings.Join(loopActions(streamOf(t, store, testRun)), ","); got != "set,edit,stop" {
		t.Errorf("loop events %s", got)
	}

	// The chat shows the tick as the operator's, labelled with its loop, and
	// the loop's changes as amplio's notes.
	_, body = doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/chat", "")
	var feed chatFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, m := range feed.Messages {
		lines = append(lines, m.Kind+"/"+m.From+": "+firstLineOf(m.Content))
	}
	want := []string{
		"environment/amplio: loop 1 set by the operator: every 1m, 3 times",
		"operator/loop 1: [loop 1 · every 1m, 3 times · tick 1] check the eval",
		"environment/amplio: loop 1 changed by the operator: every 1m, 3 times",
		"environment/amplio: loop 1 stopped by the operator",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("chat:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// A loop can verify each tick: POST and PATCH take a verification request
// (the [verify] defaults filled in when it is given), PATCH null stops it, GET
// shows it, and each tick starts a version before its message. The chat shows
// the loop's request and the tick's version, not a note per version.
func TestServer_LoopsVerify(t *testing.T) {
	t.Parallel()
	ts, srv, store := newLoopServer(t)
	srv.mgr.SetVerifyDefaults(config.VerifyDefaults{MaxRounds: 4})
	seedRun(t, store, db.SessionConcluded, 1)
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: testRun, SessionID: config.ChatbotSessionID,
		AgentType: config.ChatbotAgentType, Status: db.SessionIdle}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops"
	withVerify := func(prompt string, f loop.Form, v string) string {
		b := loopBody(t, prompt, f)
		return strings.TrimSuffix(b, "}") + `,"verify":` + v + `}`
	}

	code, body := doReq(t, http.MethodPost, base+"?token=secret", withVerify("run the tests", loop.Form{Every: time.Minute, SendNow: true, Times: 2}, `{"criteria":"they pass"}`))
	var created loopDTO
	if code != http.StatusCreated || json.Unmarshal(body, &created) != nil || created.Verify == nil ||
		*created.Verify != (event.LoopVerify{Criteria: "they pass", MaxRounds: 4}) {
		t.Fatalf("POST with verify: %d %s", code, body)
	}
	waitFor(t, "the send-now tick", func() bool { return countTicks(streamOf(t, store, testRun)) == 1 })
	var setBy string
	for _, e := range streamOf(t, store, testRun) {
		switch x := e.(type) {
		case *event.VerifyEvent:
			if x.Action == event.VerifySet {
				setBy = x.By
			}
		case *event.UserEvent:
			if x.Loop != nil && (setBy != "loop 1" || x.Loop.Version != 1) {
				t.Errorf("tick %+v after a version set by %q; want version 1, set by loop 1, before it", x.Loop, setBy)
			}
		}
	}
	_, body = doReq(t, http.MethodGet, base, "")
	if !strings.Contains(string(body), `"verify":{"criteria":"they pass","max_rounds":4}`) {
		t.Errorf("GET: %s", body)
	}

	_, body = doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/chat", "")
	var feed chatFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, m := range feed.Messages {
		line := m.Kind + ": " + firstLineOf(m.Content)
		if m.Loop != nil {
			line += fmt.Sprintf(" (v%d)", m.Loop.Version)
		}
		lines = append(lines, line)
	}
	want := []string{
		"environment: loop 1 set by the operator: every 1m, 2 times, verifying each tick (criteria: they pass; up to 4 rounds)",
		"operator: [loop 1 · every 1m, 2 times · tick 1 · verification v1] run the tests (v1)",
	}
	if len(lines) < 2 || strings.Join(lines[:2], "\n") != strings.Join(want, "\n") {
		t.Errorf("chat:\n%s\nwant it to start:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	for _, tc := range []struct {
		name, body string
		want       int
		wantIn     string
	}{
		{"change it", `{"verify":{"max_rounds":0}}`, 200, `"verify":{"max_rounds":0}`},
		{"keep it", `{"prompt":"run them again"}`, 200, `"verify":{"max_rounds":0}`},
		{"unknown field", `{"verify":{"rounds":2}}`, 400, "unknown field"},
		{"bad rounds", `{"verify":{"max_rounds":-1}}`, 400, "max_rounds"},
		{"stop it", `{"verify":null}`, 200, `"prompt":"run them again"`},
		{"stop it again", `{"verify":null}`, 400, "doesn't verify its ticks"},
	} {
		if tc.name == "stop it" {
			// While loop 1 verifies, no other loop of the session can.
			code, body := doReq(t, http.MethodPost, base+"?token=secret", withVerify("p", loop.Form{Every: time.Hour}, `{}`))
			if code != http.StatusBadRequest || !strings.Contains(string(body), "loop 1 already verifies each tick") {
				t.Errorf("a second verifying loop: %d %s", code, body)
			}
		}
		code, body := doReq(t, http.MethodPatch, base+"/1?token=secret", tc.body)
		if code != tc.want || !strings.Contains(string(body), tc.wantIn) {
			t.Errorf("PATCH %s: %d %s; want %d containing %q", tc.name, code, body, tc.want, tc.wantIn)
		}
	}
	if _, body := doReq(t, http.MethodGet, base, ""); strings.Contains(string(body), "verify") {
		t.Errorf("GET after stopping verification: %s", body)
	}
	_, body = doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/chat", "")
	if !strings.Contains(string(body), "loop 1 changed by the operator: every 1m, 2 times, no longer verifying each tick") {
		t.Errorf("the chat doesn't say the loop stopped verifying: %s", body)
	}

	chat := ts.URL + "/api/runs/" + testRun + "/sessions/" + config.ChatbotSessionID + "/loops?token=secret"
	if code, body := doReq(t, http.MethodPost, chat, withVerify("p", loop.Form{Every: time.Hour}, `{}`)); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "interactive") {
		t.Errorf("POST verify on the chatbot: %d %s", code, body)
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestServer_ArchiveEndsAndDeleteForgetsLoops(t *testing.T) {
	t.Parallel()
	ts, srv, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun
	if code, body := doReq(t, http.MethodPost, base+"/sessions/main-agent/loops?token=secret", loopBody(t, "p", loop.Form{Every: time.Hour})); code != http.StatusCreated {
		t.Fatalf("POST: %d %s", code, body)
	}
	if code, body := doReq(t, http.MethodPatch, base+"?token=secret", `{"archived":true}`); code != http.StatusOK {
		t.Fatalf("archive: %d %s", code, body)
	}
	if got := strings.Join(loopActions(streamOf(t, store, testRun)), ","); got != "set,ended" {
		t.Errorf("loop events after archiving: %s", got)
	}
	if got := srv.loops.List(testRun, ""); len(got) != 0 {
		t.Errorf("loops left after archiving: %+v", got)
	}

	if code, _ := doReq(t, http.MethodPatch, base+"?token=secret", `{"archived":false}`); code != http.StatusOK {
		t.Fatal("unarchive")
	}
	if code, body := doReq(t, http.MethodPost, base+"/sessions/main-agent/loops?token=secret", loopBody(t, "p", loop.Form{Every: time.Hour})); code != http.StatusCreated {
		t.Fatalf("POST after unarchiving: %d %s", code, body)
	}
	if code, body := doReq(t, http.MethodDelete, base+"?token=secret", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if got := srv.loops.List(testRun, ""); len(got) != 0 {
		t.Errorf("loops left after deleting the run: %+v", got)
	}
}

func TestServer_LoopSettings(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	get := func() loopSettings {
		t.Helper()
		code, body := doReq(t, http.MethodGet, ts.URL+"/api/settings", "")
		var s struct {
			Loop loopSettings `json:"loop"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &s) != nil {
			t.Fatalf("GET /api/settings: %d %s", code, body)
		}
		return s.Loop
	}
	if got := get(); got.Expiry != "168h" || got.MinIntervalSeconds != 60 {
		t.Errorf("defaults: %+v", got)
	}
	srv.SetLoops(nil, LoopDefaults{Expiry: 36 * time.Hour})
	if got := get(); got.Expiry != "36h" {
		t.Errorf("36h: %+v", got)
	}
	srv.SetLoops(nil, LoopDefaults{Expiry: 90 * time.Minute})
	if got := get(); got.Expiry != "1h30m" {
		t.Errorf("90m: %+v", got)
	}
	srv.SetLoops(nil, LoopDefaults{Never: true})
	if got := get(); got.Expiry != "never" {
		t.Errorf("never: %+v", got)
	}
	// Without a scheduler, the loop routes say so rather than failing obscurely.
	seedRun(t, srv.store, db.SessionConcluded, 0)
	if code, _ := doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/loops", ""); code != http.StatusServiceUnavailable {
		t.Errorf("loops without a scheduler: %d, want 503", code)
	}
}

// End to end through the runtime: a tick is operator input, so it revives a
// concluded agent, which answers it.
func TestServer_LoopTickRevivesConcludedAgent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(cancel)
	replies := make([]llm.Response, 10)
	for i := range replies {
		replies[i] = llm.Response{Content: "done", StopReason: "end_turn"}
	}
	provider := &llm.MockProvider{Model: "m", Responses: replies}
	mgr := runtime.NewRunManager(store, func(string) (llm.Provider, error) { return provider, nil }, runtime.NewRunRegistry(), plain.Factory)
	store.SetCommitListener(runtime.NewCommitNotifier(mgr.RunRegistry(), mgr.RespawnSession, mgr.SessionStatus, mgr.AwaitsVerdict))
	srv := New(ctx, store, mgr, eventstream.NewBus(), "secret", RunDefaults{LLM: "vertex:x"})
	sched := loop.New(store, loop.Options{})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Shutdown)
	srv.SetLoops(sched, LoopDefaults{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	code, body := doReq(t, http.MethodPost, ts.URL+"/api/runs?token=secret", `{"task":"say done"}`)
	if code != http.StatusCreated {
		t.Fatalf("start run: %d %s", code, body)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(body, &created)
	status := func() string {
		s, err := store.GetSession(ctx, created.RunID, "main-agent")
		if err != nil {
			return ""
		}
		return s.Status
	}
	// Its goroutine gone too, not just its terminal write: a tick that lands
	// before the goroutine unregisters only bumps it, and revives nothing.
	waitFor(t, "the run to conclude", func() bool {
		return status() == db.SessionConcluded && !mgr.RunRegistry().IsRunActive(created.RunID)
	})
	before := provider.CallCount()

	if code, body := doReq(t, http.MethodPost, ts.URL+"/api/runs/"+created.RunID+"/sessions/main-agent/loops?token=secret",
		loopBody(t, "say done again", loop.Form{Every: time.Minute, SendNow: true, Times: 1})); code != http.StatusCreated {
		t.Fatalf("POST loop: %d %s", code, body)
	}
	waitFor(t, "the tick to be answered", func() bool {
		answered := false
		for _, e := range streamOf(t, store, created.RunID) {
			if u, ok := e.(*event.UserEvent); ok && u.Loop != nil {
				answered = false
			} else if a, ok := e.(*event.AssistantEvent); ok && a.Content == "done" {
				answered = true
			}
		}
		return answered && provider.CallCount() > before && status() == db.SessionConcluded
	})
	// The model saw the tick as a user turn.
	calls := provider.Recorded()
	saw := false
	for _, m := range calls[len(calls)-1].Messages {
		saw = saw || (m.Role == llm.RoleUser && strings.Contains(m.Content, "tick 1] say done again"))
	}
	if !saw {
		t.Error("the revived agent's request did not carry the tick")
	}
}

// Cancelling a run ends its loops at once, including one on a session that had
// already concluded (the cancel cascade leaves those alone).
func TestServer_CancelRunEndsLoops(t *testing.T) {
	t.Parallel()
	ts, srv, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun
	if code, body := doReq(t, http.MethodPost, base+"/sessions/main-agent/loops?token=secret", loopBody(t, "p", loop.Form{Every: time.Hour})); code != http.StatusCreated {
		t.Fatalf("POST: %d %s", code, body)
	}
	if code, body := doReq(t, http.MethodPost, base+"/cancel?token=secret", ""); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", code, body)
	}
	evs := streamOf(t, store, testRun)
	if got := strings.Join(loopActions(evs), ","); got != "set,ended" {
		t.Fatalf("loop events after cancelling: %s", got)
	}
	if l := evs[len(evs)-1].(*event.LoopEvent); l.Reason != loop.EndedRunCancelled {
		t.Errorf("ended because %q", l.Reason)
	}
	if got := srv.loops.List(testRun, ""); len(got) != 0 {
		t.Errorf("loops left after cancelling: %+v", got)
	}
}

// The API shows a loop's times in the server's zone, like its summary and the
// tick headers.
func TestServer_LoopTimesInServerZone(t *testing.T) {
	t.Parallel()
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skip(err)
	}
	srv, _, store := newTestServer(t)
	sched := loop.New(store, loop.Options{Location: zurich})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Shutdown)
	srv.SetLoops(sched, LoopDefaults{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedRun(t, store, db.SessionConcluded, 1)
	code, body := doReq(t, http.MethodPost, ts.URL+"/api/runs/"+testRun+"/sessions/main-agent/loops?token=secret",
		`{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:20991110T120000Z\nRRULE:FREQ=HOURLY"}}`)
	if code != http.StatusCreated || !strings.Contains(string(body), `"next_tick":"2099-11-10T13:00:00+01:00"`) {
		t.Errorf("POST: %d %s; want next_tick in Zurich time", code, body)
	}
}

// A loop whose next tick would read as year 10000 in the server's zone is
// refused up front, so it can't turn the session's loop list into an empty
// answer.
func TestServer_LoopInYear10000Refused(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip(err)
	}
	srv, _, store := newTestServer(t)
	sched := loop.New(store, loop.Options{Location: tokyo})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Shutdown)
	srv.SetLoops(sched, LoopDefaults{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	seedRun(t, store, db.SessionConcluded, 1)
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops"
	code, body := doReq(t, http.MethodPost, base+"?token=secret",
		`{"prompt":"p","schedule":{"kind":"rrule","spec":"DTSTART:99991231T230000Z\nRRULE:FREQ=DAILY"}}`)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "100 years") {
		t.Errorf("POST: %d %s", code, body)
	}
	if code, body = doReq(t, http.MethodGet, base, ""); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("GET: %d %q", code, body)
	}
}

// writeJSON encodes before it commits to a status: a value JSON can't carry is
// a 500 with an error, not the promised status with an empty body.
func TestWriteJSON_EncodeErrorIs500(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]time.Time{"t": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusInternalServerError || err != nil ||
		!strings.Contains(got["error"], "year outside of range") {
		t.Errorf("writeJSON(year 10000) = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]int{"n": 1})
	if rec.Code != http.StatusCreated || rec.Body.String() != "{\"n\":1}\n" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("writeJSON = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

// The run detail carries each session's active loops (a count and the
// earliest next tick) and its verification summary, so the session tree and
// the status lines show them without a request per session.
func TestServer_RunDetailSummarizesLoopsAndVerification(t *testing.T) {
	t.Parallel()
	ts, _, store := newLoopServer(t)
	seedRun(t, store, db.SessionConcluded, 1)
	ctx := context.Background()
	if err := store.CreateSession(ctx, db.SessionRecord{RunID: testRun, SessionID: "worker", ParentID: "main-agent",
		AgentType: "standard_agent", Status: db.SessionOngoing, Metadata: map[string]any{
			verify.SummaryMetadataKey: verify.Summary{Status: verify.StatusVerifying, Version: 2, Round: 1, MaxRounds: 3, Verifier: "lynx"},
		}}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/runs/" + testRun + "/sessions/main-agent/loops?token=secret"
	for _, every := range []time.Duration{time.Hour, time.Minute} {
		if code, body := doReq(t, http.MethodPost, base, loopBody(t, "check", loop.Form{Every: every})); code != http.StatusCreated {
			t.Fatalf("POST loop: %d %s", code, body)
		}
	}
	_, body := doReq(t, http.MethodGet, ts.URL+"/api/runs/"+testRun, "")
	var detail runDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	by := map[string]sessionDTO{}
	for _, s := range detail.Sessions {
		by[s.SessionID] = s
	}
	main, worker := by["main-agent"], by["worker"]
	if main.Loops == nil || main.Loops.Count != 2 || time.Until(main.Loops.NextTick) > 2*time.Minute {
		t.Errorf("main-agent loops = %+v, want 2, the next within the minute's", main.Loops)
	}
	if main.Verify != nil || worker.Loops != nil {
		t.Errorf("main-agent verify = %+v, worker loops = %+v; want neither", main.Verify, worker.Loops)
	}
	if w := worker.Verify; w == nil || w.Status != verify.StatusVerifying || w.Round != 1 || w.MaxRounds != 3 || w.Verifier != "lynx" {
		t.Errorf("worker verify = %+v", w)
	}
}
