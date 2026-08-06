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
	"os"
	"strings"
	"testing"
)

// The /api/runs response is the pagination envelope {runs, has_more,
// next_cursor}, NOT a bare array. `client list` must parse the envelope and
// feed its inner `runs` to printRunSummaries — a regression guard for the
// pagination change that briefly broke `client list` (it unmarshaled the
// object into a []struct and errored).
func TestClientList_ParsesPaginationEnvelope(t *testing.T) {
	raw := []byte(`{
		"runs": [
			{"run_id":"r1","title":"One","root_status":"idle","root_step":3,"starred":true},
			{"run_id":"r2","task":"do thing","root_status":"concluded","root_step":9}
		],
		"has_more": true,
		"next_cursor": "2026-01-01T00:00:00Z"
	}`)

	var page struct {
		Runs    json.RawMessage `json:"runs"`
		HasMore bool            `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if !page.HasMore {
		t.Error("has_more = false, want true")
	}
	// printRunSummaries consumes the inner array (not the envelope); it must not
	// error on the documented runSummary[] shape.
	var out, errOut bytes.Buffer
	if err := printRunSummaries(&out, &errOut, page.Runs, page.HasMore); err != nil {
		t.Fatalf("printRunSummaries: %v", err)
	}
	// The table is the datum (stdout); the "more runs" hint is commentary (stderr).
	if !strings.Contains(out.String(), "r1") || !strings.Contains(out.String(), "r2") {
		t.Errorf("run rows missing from stdout:\n%s", out.String())
	}
	if strings.Contains(out.String(), "more runs beyond this page") {
		t.Error("pagination hint leaked into stdout")
	}
	if !strings.Contains(errOut.String(), "more runs beyond this page") {
		t.Errorf("pagination hint missing from stderr: %q", errOut.String())
	}
}

// An empty page still renders cleanly (no crash, no spurious error).
func TestClientList_EmptyPage(t *testing.T) {
	var page struct {
		Runs    json.RawMessage `json:"runs"`
		HasMore bool            `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(`{"runs":[],"has_more":false}`), &page); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := printRunSummaries(&out, &errOut, page.Runs, page.HasMore); err != nil {
		t.Fatalf("printRunSummaries(empty): %v", err)
	}
	// Empty stdout is what lets a caller test `[ -z "$(amplio client list)" ]`.
	if out.Len() != 0 {
		t.Errorf("stdout not empty for an empty page: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "(no runs)") {
		t.Errorf("(no runs) missing from stderr: %q", errOut.String())
	}
}

// resolveTask is the whole task-source policy for `client submit`: exactly one
// source, stdin only when asked for.
func TestResolveTask(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		flag    string // "" means --task not passed
		setFlag bool
		stdin   string
		devNull bool // stdin is a char device (terminal or /dev/null), not a pipe
		want    string
		wantErr string
	}{
		{name: "positional", args: []string{"do the thing"}, want: "do the thing"},
		{name: "flag", flag: "do the thing", setFlag: true, want: "do the thing"},
		{name: "stdin", flag: "-", setFlag: true, stdin: "# Task\n\nbody\n", want: "# Task\n\nbody"},
		{
			name: "front matter survives stdin", flag: "-", setFlag: true,
			stdin: "---\nid: t1\n---\nbody\n", want: "---\nid: t1\n---\nbody",
		},
		{
			name: "both sources", args: []string{"a"}, flag: "b", setFlag: true,
			wantErr: "task given twice",
		},
		{name: "neither", wantErr: "a task is required"},
		{name: "empty flag", flag: "  ", setFlag: true, wantErr: "--task was empty"},
		{name: "empty stdin", flag: "-", setFlag: true, stdin: "  \n", wantErr: "stdin was empty"},
		{name: "stdin not redirected", flag: "-", setFlag: true, devNull: true, wantErr: "not a file or pipe"},
		{name: "blank positional", args: []string{"   "}, wantErr: "a task is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := clientSubmitCmd()
			cmd.SetIn(strings.NewReader(tc.stdin))
			if tc.devNull {
				f, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close() //nolint:errcheck
				cmd.SetIn(f)
			}
			if tc.setFlag {
				if err := cmd.Flags().Set("task", tc.flag); err != nil {
					t.Fatal(err)
				}
			}
			got, err := resolveTask(cmd, tc.args, tc.flag)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case got != tc.want:
				t.Errorf("task = %q, want %q", got, tc.want)
			}
		})
	}
}

// A task beginning with "-" cannot be positional — pflag takes it first. The
// command cannot fix that, so it must at least say what to do instead.
func TestSubmit_LeadingDashTaskExplainsStdin(t *testing.T) {
	cmd := clientSubmitCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"---\nid: t1\n---\nbody"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a flag-parse error for a task starting with ---")
	}
	if !strings.Contains(err.Error(), "--task=-") {
		t.Errorf("error does not point at stdin: %v", err)
	}
}

// Briefings are opt-in: no flag means the request carries no briefings key.
func TestSubmit_Briefings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		want   string // JSON fragment expected in the request body
		absent bool
	}{
		{name: "absent", args: []string{"--task=t"}, absent: true},
		{name: "one", args: []string{"--task=t", "--briefing=run-manager"}, want: `"briefings":["run-manager"]`},
		{
			name: "repeated", args: []string{"--task=t", "--briefing=a", "--briefing=b"},
			want: `"briefings":["a","b"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body) //nolint:errcheck
				_, _ = w.Write([]byte(`{"run_id":"r1"}`))
			})
			cmd := clientSubmitCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); err != nil {
				t.Fatalf("submit: %v", err)
			}
			switch {
			case tc.absent && strings.Contains(string(body), "briefings"):
				t.Errorf("no flag should send no briefings key: %s", body)
			case !tc.absent && !strings.Contains(string(body), tc.want):
				t.Errorf("body = %s, want it to contain %s", body, tc.want)
			}
		})
	}
}
