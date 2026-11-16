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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListBriefings(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	var got []struct{ Name, Description, Scope, Source string }
	getJSON(t, srv.Handler(), "/api/briefings", &got)
	if len(got) == 0 {
		t.Fatal("no briefings listed; the shipped library should be non-empty")
	}
	for _, b := range got {
		if b.Name == "" || b.Description == "" || b.Scope == "" {
			t.Errorf("incomplete entry: %+v", b)
		}
	}
	// The picker needs identity, not several KB of prompt per row.
	raw := getRaw(t, srv.Handler(), "/api/briefings")
	if strings.Contains(raw, "\"body\"") {
		t.Error("briefing bodies must not be served in the library listing")
	}
}

// A run carries exactly the briefings it asked for: opt-in, no server-side
// default set to reconcile with.
func TestStartRun_Briefings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		briefings string // the request's "briefings" member; "" leaves it out
		want      []string
	}{
		{"absent → none", "", nil},
		{"empty → none", `[]`, nil},
		{"explicit", `["second-opinion"]`, []string{"second-opinion"}},
		{"unknown dropped", `["second-opinion","nope"]`, []string{"second-opinion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A temp workspace: "." would be this checkout, which the run's
			// workspace resolution inspects with jj and git. Made before the
			// server, so it is removed after the server's cleanup cancels the run.
			ws := t.TempDir()
			srv, _, _ := newTestServer(t)
			h := srv.Handler()
			body := map[string]any{"task": "t", "workspace": ws}
			if tc.briefings != "" {
				body["briefings"] = json.RawMessage(tc.briefings)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer secret")
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated && rec.Code != http.StatusAccepted {
				t.Fatalf("start run = %d: %s", rec.Code, rec.Body.String())
			}
			var created struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Briefings []string `json:"briefings"`
			}
			getJSON(t, h, "/api/runs/"+created.RunID, &detail)
			if strings.Join(detail.Briefings, ",") != strings.Join(tc.want, ",") {
				t.Errorf("briefings = %v, want %v", detail.Briefings, tc.want)
			}
		})
	}
}

// Never null: Overview iterates it.
func TestRunDetail_BriefingsAlwaysArray(t *testing.T) {
	t.Parallel()
	srv, _, store := newTestServer(t)
	seedRun(t, store, "concluded", 1)
	raw := getRaw(t, srv.Handler(), "/api/runs/"+testRun)
	if !strings.Contains(raw, `"briefings":[]`) {
		t.Errorf("want an empty array for a run with none: %s", firstN(raw, 300))
	}
}
