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
	"amplio/internal/db"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amplio/internal/config"
)

// Not parallel: config.SetDataDir pins the data dir for the whole process.
func TestServer_Artifacts(t *testing.T) {
	config.SetDataDir(t.TempDir())
	t.Cleanup(func() { config.SetDataDir("") })
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Populate the run's artifact dir: a file + a subdir with a file.
	base := config.ArtifactDir(testRun)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "plan.md"), []byte("# Plan\nstep 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sub", "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(path string) (int, string, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Security-Policy"), b
	}

	// Root listing: dirs first (sub), then files (plan.md).
	status, _, body := get("/api/runs/" + testRun + "/artifacts?token=secret")
	if status != 200 {
		t.Fatalf("list status = %d", status)
	}
	var listing artifactListing
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 || !listing.Entries[0].IsDir || listing.Entries[0].Name != "sub" || listing.Entries[1].Name != "plan.md" {
		t.Fatalf("entries = %+v", listing.Entries)
	}

	// Subdir listing.
	_, _, body = get("/api/runs/" + testRun + "/artifacts?path=sub&token=secret")
	_ = json.Unmarshal(body, &listing)
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "note.txt" {
		t.Errorf("sub entries = %+v", listing.Entries)
	}

	// File serve: content + CSP sandbox.
	status, csp, body := get("/api/runs/" + testRun + "/artifacts/raw?path=plan.md&token=secret")
	if status != 200 || string(body) != "# Plan\nstep 1" {
		t.Errorf("raw status=%d body=%q", status, body)
	}
	if !strings.Contains(csp, "sandbox") {
		t.Errorf("missing CSP sandbox header: %q", csp)
	}

	// Traversal attempt is confined by os.Root → not found.
	if status, _, _ := get("/api/runs/" + testRun + "/artifacts/raw?path=../../config.toml&token=secret"); status != http.StatusNotFound {
		t.Errorf("traversal status = %d, want 404", status)
	}

	// Listing a file (not a dir) → 400.
	if status, _, _ := get("/api/runs/" + testRun + "/artifacts?path=plan.md&token=secret"); status != http.StatusBadRequest {
		t.Errorf("list-a-file status = %d, want 400", status)
	}

	// Recursive listing: every FILE (not dir), forward-slashed subpaths, sorted.
	status, _, body = get("/api/runs/" + testRun + "/artifacts/all?token=secret")
	if status != 200 {
		t.Fatalf("all status = %d", status)
	}
	var all artifactAllListing
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatal(err)
	}
	gotPaths := make([]string, len(all.Files))
	for i, f := range all.Files {
		gotPaths[i] = f.Path
	}
	want := []string{"plan.md", "sub/note.txt"}
	if len(gotPaths) != len(want) || gotPaths[0] != want[0] || gotPaths[1] != want[1] {
		t.Errorf("recursive files = %v, want %v", gotPaths, want)
	}
}

// A run that has not written any artifact yet has no artifact dir. Both listings
// still answer 200 with their usual object shape and an empty (not null) list:
// /artifacts/all used to answer a bare [], which the browser's file search read
// as {files: undefined} and left on "Loading…".
//
// Not parallel: config.SetDataDir pins the data dir for the whole process.
func TestServer_Artifacts_NoArtifactDir(t *testing.T) {
	config.SetDataDir(t.TempDir())
	t.Cleanup(func() { config.SetDataDir("") })
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if _, err := os.Stat(config.ArtifactDir(testRun)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("artifact dir exists before the test (stat err = %v)", err)
	}

	get := func(path string) []byte {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, resp.StatusCode, b)
		}
		return b
	}
	// Decode into a generic map first: a bare [] (the old answer) must fail here,
	// and a null list must be told apart from an empty one.
	object := func(body []byte) map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("want a JSON object, got %s (%v)", body, err)
		}
		return m
	}

	all := object(get("/api/runs/" + testRun + "/artifacts/all?token=secret"))
	if got := string(all["files"]); got != "[]" {
		t.Errorf("/artifacts/all files = %s, want []", got)
	}
	var root string
	if err := json.Unmarshal(all["root"], &root); err != nil || root != config.ArtifactDir(testRun) {
		t.Errorf("/artifacts/all root = %s, want %q", all["root"], config.ArtifactDir(testRun))
	}

	listing := object(get("/api/runs/" + testRun + "/artifacts?token=secret"))
	if got := string(listing["entries"]); got != "[]" {
		t.Errorf("/artifacts entries = %s, want []", got)
	}

	// Reads never create the dir.
	if _, err := os.Stat(config.ArtifactDir(testRun)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("listing created the artifact dir (stat err = %v)", err)
	}
}

// A PDF must reach the browser's built-in viewer, which the artifact browser
// embeds in a frame: served inline (an attachment would download it instead of
// rendering it) under a sandbox that still lets the viewer script itself.
//
// Not parallel: config.SetDataDir pins the data dir for the whole process.
func TestServer_ArtifactRaw_Disposition(t *testing.T) {
	config.SetDataDir(t.TempDir())
	t.Cleanup(func() { config.SetDataDir("") })
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	base := config.ArtifactDir(testRun)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	const pdfName = "paper (v2).pdf" // spaces: the filename param must survive quoting
	files := map[string]string{
		pdfName:    "%PDF-1.4\n%\u00e2\u00e3\u00cf\u00d3\n",
		"blob.bin": "\x00\x01\x02\x03", // unmapped extension, sniffs as binary
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(name string) (http.Header, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		path := "/api/runs/" + testRun + "/artifacts/raw?path=" + url.QueryEscape(name)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", name, rec.Code, rec.Body.String())
		}
		return rec.Header(), rec.Body.String()
	}

	hdr, body := fetch(pdfName)
	if body != files[pdfName] {
		t.Errorf("pdf body = %q", body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("pdf Content-Type = %q, want application/pdf", ct)
	}
	if disp, params, err := mime.ParseMediaType(hdr.Get("Content-Disposition")); err != nil ||
		disp != "inline" || params["filename"] != pdfName {
		t.Errorf("pdf Content-Disposition = %q (err %v), want inline + the real filename", hdr.Get("Content-Disposition"), err)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "allow-scripts") {
		t.Errorf("pdf CSP = %q, want a sandbox the viewer can still script in", csp)
	}
	if hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("pdf served without nosniff")
	}

	// Everything else keeps downloading, under the bare sandbox.
	hdr, _ = fetch("blob.bin")
	if disp, params, err := mime.ParseMediaType(hdr.Get("Content-Disposition")); err != nil ||
		disp != "attachment" || params["filename"] != "blob.bin" {
		t.Errorf("binary Content-Disposition = %q (err %v), want attachment + the real filename", hdr.Get("Content-Disposition"), err)
	}
	if csp := hdr.Get("Content-Security-Policy"); strings.Contains(csp, "allow-scripts") {
		t.Errorf("binary CSP = %q, want the bare sandbox", csp)
	}
}

// Naming a run's artifact dir must never create it: deleting a run that wrote
// nothing used to leave an empty directory behind, made by the very call that
// asked where to delete.
func TestArtifactDir_IsPureAndReadsDoNotCreate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	path := config.ArtifactDir("never-wrote-anything")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ArtifactDir created %s (stat err = %v)", path, err)
	}

	srv, _, store := newTestServer(t)
	if err := store.CreateRun(context.Background(), db.RunRecord{RunID: "never-wrote-anything"}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// Listing is an empty result, not a 500 and not a mkdir.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/never-wrote-anything/artifacts", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("list = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// A missing file under a missing dir is 404, not an internal error.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/never-wrote-anything/artifacts/raw?path=x.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("raw = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a GET created %s", path)
	}
}
