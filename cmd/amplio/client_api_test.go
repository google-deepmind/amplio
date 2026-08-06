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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amplio/internal/config"
)

// fakeServer stands in for a running amplio: an httptest server plus the
// server.json that makes the CLI discover it, so these tests exercise the real
// discovery + auth path without a real server.
func fakeServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	info := serverInfo{PID: os.Getpid(), URL: srv.URL, Addr: srv.URL, Token: "test-token"}
	blob, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvDataDir, dir)
}

func runAPI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := clientAPICmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestClientAPI_GET(t *testing.T) {
	var seen *http.Request
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	out, errOut, err := runAPI(t, "/api/runs")
	if err != nil {
		t.Fatalf("api: %v (stderr %q)", err, errOut)
	}
	if strings.TrimSpace(out) != `{"runs":[]}` {
		t.Errorf("stdout = %q, want the raw body", out)
	}
	if errOut != "" {
		t.Errorf("stderr should be silent on success, got %q", errOut)
	}
	if seen.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", seen.Method)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the token from server.json", got)
	}
}

func TestClientAPI_PathIsNormalized(t *testing.T) {
	var seen *http.Request
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) { seen = r })
	if _, _, err := runAPI(t, "api/runs"); err != nil {
		t.Fatal(err)
	}
	if seen.URL.Path != "/api/runs" {
		t.Errorf("path = %q, want a leading slash added", seen.URL.Path)
	}
}

func TestClientAPI_DataImpliesPOST(t *testing.T) {
	var seen *http.Request
	var body []byte
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		body, _ = io.ReadAll(r.Body) //nolint:errcheck
	})
	if _, _, err := runAPI(t, "/api/runs", "--data", `{"task":"t"}`); err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPost {
		t.Errorf("method = %s, want POST implied by --data", seen.Method)
	}
	if string(body) != `{"task":"t"}` {
		t.Errorf("body = %q, want it forwarded verbatim", body)
	}
	if ct := seen.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestClientAPI_ExplicitMethodWins(t *testing.T) {
	var seen *http.Request
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) { seen = r })
	if _, _, err := runAPI(t, "/api/runs/x", "-X", "patch", "--data", `{"starred":true}`); err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH (lowercase input upcased)", seen.Method)
	}
}

// A failed call must not pollute stdout: a script doing `api … | jq` should see
// nothing to parse, and get a non-zero exit.
func TestClientAPI_ErrorBodyGoesToStderr(t *testing.T) {
	fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"run not found"}`))
	})
	out, errOut, err := runAPI(t, "/api/runs/nope")
	if err == nil {
		t.Fatal("want an error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should name the status: %v", err)
	}
	if out != "" {
		t.Errorf("stdout must stay empty on failure, got %q", out)
	}
	if !strings.Contains(errOut, "run not found") {
		t.Errorf("server's error body missing from stderr: %q", errOut)
	}
}

func TestRequestBody_Sources(t *testing.T) {
	file := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(file, []byte(`{"from":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, data, stdin, want, wantErr string
		unset                            bool
	}{
		{name: "unset", unset: true},
		{name: "literal", data: `{"a":1}`, want: `{"a":1}`},
		{name: "file", data: "@" + file, want: `{"from":"file"}`},
		{name: "stdin", data: "-", stdin: `{"from":"stdin"}`, want: `{"from":"stdin"}`},
		{name: "invalid", data: `{a:1}`, wantErr: "not valid JSON"},
		{name: "missing file", data: "@/nonexistent/b.json", wantErr: "read body file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := clientAPICmd()
			cmd.SetIn(strings.NewReader(tc.stdin))
			if !tc.unset {
				if err := cmd.Flags().Set("data", tc.data); err != nil {
					t.Fatal(err)
				}
			}
			r, err := requestBody(cmd, tc.data)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case tc.unset:
				if r != nil {
					t.Error("no --data should mean no body (a GET must not carry one)")
				}
			default:
				b, _ := io.ReadAll(r) //nolint:errcheck
				if string(b) != tc.want {
					t.Errorf("body = %q, want %q", b, tc.want)
				}
			}
		})
	}
}
