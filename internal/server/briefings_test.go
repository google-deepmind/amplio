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
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"absent → none", `{"task":"t","workspace":"."}`, nil},
		{"empty → none", `{"task":"t","workspace":".","briefings":[]}`, nil},
		{"explicit", `{"task":"t","workspace":".","briefings":["second-opinion"]}`, []string{"second-opinion"}},
		{"unknown dropped", `{"task":"t","workspace":".","briefings":["second-opinion","nope"]}`, []string{"second-opinion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			h := srv.Handler()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewBufferString(tc.body))
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
	srv, _, store := newTestServer(t)
	seedRun(t, store, "concluded", 1)
	raw := getRaw(t, srv.Handler(), "/api/runs/"+testRun)
	if !strings.Contains(raw, `"briefings":[]`) {
		t.Errorf("want an empty array for a run with none: %s", firstN(raw, 300))
	}
}
