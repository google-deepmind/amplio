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
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"amplio/internal/event"
)

// loopServer fakes the server's loop routes and records what the CLI sent.
type loopServer struct {
	mu       sync.Mutex
	expiry   string          // GET /api/settings loop.expiry
	posted   *event.Schedule // the last POSTed schedule
	prompt   string
	verify   json.RawMessage // the last POSTed verify
	patched  string          // the last PATCH body
	deleted  string
	settings int // GET /api/settings calls
}

func (ls *loopServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/settings":
			ls.settings++
			_, _ = io.WriteString(w, `{"loop":{"expiry":"`+ls.expiry+`","min_interval_seconds":60}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/r1/sessions/main-agent/loops":
			var body struct {
				Prompt   string          `json:"prompt"`
				Schedule event.Schedule  `json:"schedule"`
				Verify   json.RawMessage `json:"verify"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode POST: %v", err)
			}
			ls.posted, ls.prompt, ls.verify = &body.Schedule, body.Prompt, body.Verify
			w.WriteHeader(http.StatusCreated)
			verify := ""
			if body.Verify != nil {
				verify = `,"verify":{"criteria":"they pass","max_rounds":3}`
			}
			_, _ = io.WriteString(w, `{"id":"4","session_id":"main-agent","summary":"every 30m","next_tick":"2026-11-10T12:00:00+01:00"`+verify+`}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/runs/r1/sessions/main-agent/loops/"):
			b, _ := io.ReadAll(r.Body)
			ls.patched = string(b)
			_, _ = io.WriteString(w, `{"id":"1","session_id":"main-agent","summary":"every 30m","next_tick":"2026-11-10T12:30:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/r1":
			_, _ = io.WriteString(w, `{"run_id":"r1","sessions":[{"session_id":"main-agent"},{"session_id":"helper"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/r1/sessions/main-agent/loops":
			_, _ = io.WriteString(w, `[{"id":"1","session_id":"main-agent","prompt":"check the eval\nand more","summary":"every 30m","ticks":2,"next_tick":"2026-11-10T12:30:00Z"},`+
				`{"id":"2","session_id":"main-agent","prompt":"run the tests","summary":"every 1d","verify":{"criteria":"they pass","model":"vertex:judge","max_rounds":3},"ticks":0}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/r1/sessions/helper/loops":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete:
			ls.deleted = r.URL.Path
			_, _ = io.WriteString(w, `{"status":"stopped"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func runLoop(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := clientLoopCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// parseSpec splits a posted spec into its DTSTART time and its other lines.
func parseSpec(t *testing.T, s *event.Schedule) (time.Time, []string) {
	t.Helper()
	if s == nil || s.Kind != event.ScheduleRRule {
		t.Fatalf("posted schedule = %+v", s)
	}
	lines := strings.Split(s.Spec, "\n")
	start, err := time.Parse("20060102T150405Z", strings.TrimPrefix(lines[0], "DTSTART:"))
	if err != nil {
		t.Fatalf("DTSTART in %q: %v", s.Spec, err)
	}
	return start, lines[1:]
}

func TestLoopAdd_BuildsTheRule(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiry    string
		args      []string
		startIn   time.Duration // DTSTART - now
		rule      string        // the RRULE line, with UNTIL as an offset from now below
		untilIn   time.Duration
		settingsQ int // calls to /api/settings
	}{
		{"defaults: send now, server expiry", "168h", []string{"--every", "30m"}, 0, "RRULE:FREQ=MINUTELY;INTERVAL=30", 168 * time.Hour, 1},
		{"server expiry in days", "7d", []string{"--every", "30m"}, 0, "RRULE:FREQ=MINUTELY;INTERVAL=30", 168 * time.Hour, 1},
		{"server expiry never", "never", []string{"--every", "30m"}, 0, "RRULE:FREQ=MINUTELY;INTERVAL=30", 0, 1},
		{"no send now, times", "168h", []string{"--every", "2h", "--no-send-now", "--times", "5"}, 2 * time.Hour, "RRULE:FREQ=HOURLY;INTERVAL=2;COUNT=5", 0, 0},
		{"never", "168h", []string{"--every", "1d", "--never"}, 0, "RRULE:FREQ=DAILY;INTERVAL=1", 0, 0},
		{"--never=false is no end flag", "168h", []string{"--every", "30m", "--never=false"}, 0, "RRULE:FREQ=MINUTELY;INTERVAL=30", 168 * time.Hour, 1},
		{"ends", "168h", []string{"--every", "1m", "--ends", "3m"}, 0, "RRULE:FREQ=MINUTELY;INTERVAL=1", 3 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := &loopServer{expiry: tc.expiry}
			fakeServer(t, ls.handler(t))
			now := time.Now().UTC().Truncate(time.Second)
			args := append(append([]string{"add", "r1", "main-agent"}, tc.args...), "check the eval")
			stdout, stderr, err := runLoop(t, args...)
			if err != nil {
				t.Fatalf("loop add: %v (%s)", err, stderr)
			}
			// The next tick prints as the server sent it, in the server's zone.
			if stdout != "4\n" || !strings.Contains(stderr, "loop 4 set on main-agent: every 30m; next tick 2026-11-10T12:00:00+01:00") {
				t.Errorf("stdout %q, stderr %q", stdout, stderr)
			}
			if ls.prompt != "check the eval" || ls.settings != tc.settingsQ {
				t.Errorf("prompt %q, settings calls %d; want %d", ls.prompt, ls.settings, tc.settingsQ)
			}
			start, rest := parseSpec(t, ls.posted)
			if d := start.Sub(now.Add(tc.startIn)); d < 0 || d > 2*time.Second {
				t.Errorf("DTSTART %v; want now+%s", start, tc.startIn)
			}
			want := tc.rule
			if tc.untilIn > 0 {
				want += ";UNTIL=" + start.Add(-tc.startIn).Add(tc.untilIn).Format("20060102T150405Z")
			}
			if len(rest) != 1 || rest[0] != want {
				t.Errorf("rule %q; want %q", rest, want)
			}
		})
	}
}

func TestLoopAdd_RRulePassesThrough(t *testing.T) {
	ls := &loopServer{expiry: "168h"}
	fakeServer(t, ls.handler(t))
	if _, stderr, err := runLoop(t, "add", "r1", "main-agent", "--rrule",
		`DTSTART:20261116T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0`, "weekly"); err != nil {
		t.Fatalf("%v (%s)", err, stderr)
	}
	if want := "DTSTART:20261116T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0"; ls.posted.Spec != want || ls.settings != 0 {
		t.Errorf("posted %q (settings calls %d); want %q", ls.posted.Spec, ls.settings, want)
	}
}

func TestLoopAdd_RejectsBadFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--rrule", "DTSTART:20261116T090000Z RRULE:FREQ=DAILY", "--every", "1h"}, "drop --every"},
		{[]string{"--rrule", "DTSTART:20261116T090000Z RRULE:FREQ=DAILY", "--never"}, "drop --never"},
		{[]string{"--every", "1h", "--times", "2", "--never"}, "exclude each other"},
		{[]string{"--every", "1h", "--ends", "1d", "--times", "2"}, "exclude each other"},
		{[]string{"--times", "2"}, "--every is required"},
		{[]string{"--every", "30s"}, "at most every 1m"},
		{[]string{"--every", "1h", "--times", "0"}, "at least 1"},
		{[]string{"--every", "1h", "--ends", "0"}, "--ends must be positive; for a loop that doesn't end, use --never"},
		{[]string{"--every", "1h", "--ends", "0d"}, "--ends must be positive"},
		{[]string{"--every", "1h", "--ends", "0m"}, "--ends must be positive"},
		{[]string{"--every", "1h", "--ends", "0s"}, "--ends must be positive"},
		{[]string{"--every", "often"}, "--every"},
	} {
		ls := &loopServer{expiry: "168h"}
		fakeServer(t, ls.handler(t))
		args := append(append([]string{"add", "r1", "main-agent"}, tc.args...), "p")
		if _, _, err := runLoop(t, args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v; want %q", tc.args, err, tc.want)
		}
		if ls.posted != nil {
			t.Errorf("%v: posted anyway", tc.args)
		}
	}
}

func TestLoopListAndStop(t *testing.T) {
	ls := &loopServer{}
	fakeServer(t, ls.handler(t))
	stdout, _, err := runLoop(t, "list", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "SESSION") || !strings.Contains(stdout, "main-agent  1     every 30m  -          2") ||
		!strings.Contains(stdout, "main-agent  2     every 1d   each tick  0") ||
		strings.Contains(stdout, "and more") || strings.Contains(stdout, "helper") {
		t.Errorf("list:\n%s", stdout)
	}
	stdout, _, err = runLoop(t, "list", "r1", "helper", "--json")
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("list --json of a session without loops = %q, %v", stdout, err)
	}
	if _, stderr, err := runLoop(t, "stop", "r1", "main-agent", "1"); err != nil || !strings.Contains(stderr, "loop 1 on main-agent stopped") {
		t.Errorf("stop: %v, %q", err, stderr)
	}
	if ls.deleted != "/api/runs/r1/sessions/main-agent/loops/1" {
		t.Errorf("DELETE %q", ls.deleted)
	}
}

// --verify (or any --verify-* flag) sends a verification request with the
// loop; without one, none is sent.
func TestLoopAdd_Verify(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // the posted verify, "" for none
	}{
		{nil, ""},
		{[]string{"--verify"}, `{}`},
		{[]string{"--verify-criteria", "they pass", "--verify-max-rounds", "0"}, `{"criteria":"they pass","max_rounds":0}`},
		{[]string{"--verify-model", "#judge"}, `{"model":"#judge"}`},
	} {
		ls := &loopServer{expiry: "168h"}
		fakeServer(t, ls.handler(t))
		args := append(append([]string{"add", "r1", "main-agent", "--every", "1d", "--times", "2"}, tc.args...), "run the tests")
		_, stderr, err := runLoop(t, args...)
		if err != nil {
			t.Fatalf("%v: %v (%s)", tc.args, err, stderr)
		}
		if string(ls.verify) != tc.want {
			t.Errorf("%v: posted verify %s; want %s", tc.args, ls.verify, tc.want)
		}
		if tc.want != "" && !strings.Contains(stderr, "every 30m, verifying each tick (criteria: they pass; up to 3 rounds); next tick") {
			t.Errorf("%v: stderr %q", tc.args, stderr)
		}
	}
}

// loop edit sends only what it was given; its verify flags change only their
// parts of the loop's current request; --no-verify sends null.
func TestLoopEdit(t *testing.T) {
	for _, tc := range []struct {
		loop string
		args []string
		want string // the PATCH body
	}{
		{"1", []string{"--prompt", "run them again"}, `{"prompt":"run them again"}`},
		{"1", []string{"--verify-max-rounds", "2"}, `{"verify":{"max_rounds":2}}`},
		{"2", []string{"--verify-max-rounds", "1"}, `{"verify":{"criteria":"they pass","model":"vertex:judge","max_rounds":1}}`},
		{"2", []string{"--verify-criteria", ""}, `{"verify":{"model":"vertex:judge","max_rounds":3}}`},
		{"2", []string{"--verify"}, `{"verify":{"criteria":"they pass","model":"vertex:judge","max_rounds":3}}`},
		{"1", []string{"--no-verify"}, `{"verify":null}`},
		{"1", []string{"--rrule", `DTSTART:20261116T090000Z\nRRULE:FREQ=DAILY`}, `{"schedule":{"kind":"rrule","spec":"DTSTART:20261116T090000Z\nRRULE:FREQ=DAILY"}}`},
	} {
		ls := &loopServer{}
		fakeServer(t, ls.handler(t))
		_, stderr, err := runLoop(t, append([]string{"edit", "r1", "main-agent", tc.loop}, tc.args...)...)
		if err != nil || !strings.Contains(stderr, "on main-agent changed: every 30m; next tick 2026-11-10T12:30:00Z") {
			t.Fatalf("%v: %v (%s)", tc.args, err, stderr)
		}
		if strings.TrimSpace(ls.patched) != tc.want {
			t.Errorf("%v: PATCH %s; want %s", tc.args, ls.patched, tc.want)
		}
	}
	for _, args := range [][]string{nil, {"--verify", "--no-verify"}} {
		ls := &loopServer{}
		fakeServer(t, ls.handler(t))
		if _, _, err := runLoop(t, append([]string{"edit", "r1", "main-agent", "1"}, args...)...); err == nil || ls.patched != "" {
			t.Errorf("%v: err %v, patched %q; want refused", args, err, ls.patched)
		}
	}
}
