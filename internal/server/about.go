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
	"amplio/internal/llm"
	"encoding/json"
	"net/http"
	goruntime "runtime"
	"strings"
	"time"

	"amplio/internal/config"
	"amplio/internal/version"
)

// aboutInfo is the read-only server-introspection payload for the About page:
// build identity + the on-disk layout + the configured model/tier specs. All of
// it is already known to the server; this just surfaces it in one place (some
// of it, like the data dir, was previously only visible in the serve banner /
// logs).
// modelRef is one configured model: what is written in the config, and the
// short label derived from it (internal/llm.ShortLabel) — the same pair the run
// DTOs carry, so a model reads the same wherever it appears.
type modelRef struct {
	Spec  string `json:"spec"`
	Label string `json:"label"`
}

func toModelRef(spec string) modelRef {
	if spec == "" {
		return modelRef{}
	}
	return modelRef{Spec: spec, Label: llm.ShortLabel(spec)}
}

type aboutInfo struct {
	// Build identity (from version.Build()).
	Channel   string `json:"channel"`              // dev / nightly / vX.Y.Z
	Commit    string `json:"commit,omitempty"`     // VCS revision, "" when unavailable
	Modified  bool   `json:"modified"`             // built from a dirty worktree
	BuildTime string `json:"build_time,omitempty"` // commit time, RFC3339, "" when unavailable
	GoVersion string `json:"go_version"`
	// Platform is the SERVER's GOOS. The browser's OS is irrelevant: features
	// that read /proc depend on where the agents run, not where they're watched.
	Platform string `json:"platform"`

	// On-disk layout (the data dir powering THIS server, + its children).
	DataDir    string `json:"data_dir"`
	ConfigPath string `json:"config_path"`
	LogsDir    string `json:"logs_dir"`

	// Configured LLM tiers (what powers agents / observer / compaction / recall).
	// Each carries the full spec plus its short display label, so the page can
	// lead with the name a human recognises without hiding what is configured.
	DefaultLLM    modelRef   `json:"default_llm"`
	Models        []modelRef `json:"models"` // the configured agent-model menu
	SystemLLMHQ   modelRef   `json:"system_llm_hq"`
	SystemLLMFast modelRef   `json:"system_llm_fast"`

	// Server identity / access.
	Owner  string `json:"owner"`         // username running the server
	AuthOn bool   `json:"auth_on"`       // a token is configured (writes gated)
	Caller bool   `json:"caller_authed"` // whether THIS request may write
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	b := version.Build()
	buildTime := ""
	if !b.Time.IsZero() {
		buildTime = b.Time.UTC().Format(time.RFC3339)
	}
	models := make([]modelRef, 0, len(s.defaults.LLMs))
	for _, spec := range s.defaults.LLMs {
		models = append(models, toModelRef(spec))
	}
	writeJSON(w, http.StatusOK, aboutInfo{
		Channel:       b.Channel,
		Commit:        b.Commit,
		Modified:      b.Modified,
		BuildTime:     buildTime,
		GoVersion:     b.GoVersion,
		Platform:      goruntime.GOOS,
		DataDir:       config.DataDir(),
		ConfigPath:    config.ConfigPath(config.DataDir()),
		LogsDir:       config.LogsDir(),
		DefaultLLM:    toModelRef(s.defaults.LLM),
		Models:        models,
		SystemLLMHQ:   toModelRef(s.defaults.SystemLLMHQ),
		SystemLLMFast: toModelRef(s.defaults.SystemLLMFast),
		Owner:         s.owner,
		AuthOn:        s.token != "",
		Caller:        s.authed(r),
	})
}

// testLLMResult is the outcome of a one-shot LLM smoke test.
type testLLMResult struct {
	OK        bool   `json:"ok"`
	ModelID   string `json:"model_id,omitempty"` // the provider's resolved model id
	Reply     string `json:"reply,omitempty"`    // the (truncated) probe reply
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"` // parse/registry or call/auth failure
}

// handleTestLLM validates an agent-LLM spec WITHOUT starting a run: it builds the
// provider (catching spec-parse / unknown-provider errors instantly and for
// free) and issues one trivial Call (catching auth / scope / availability
// errors cheaply). This is the pre-flight check for the otherwise-expensive
// mistake of starting a long run on a misconfigured model. Behind requireAuth:
// it makes a real (billable) LLM call.
func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	if s.testLLM == nil {
		writeErr(w, http.StatusNotImplemented, "LLM testing is not configured")
		return
	}
	var req struct {
		Spec string `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	spec := strings.TrimSpace(req.Spec)
	if spec == "" {
		writeErr(w, http.StatusBadRequest, "spec is required")
		return
	}
	modelID, reply, latency, err := s.testLLM(r.Context(), spec)
	res := testLLMResult{}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
		res.ModelID = modelID
		res.Reply = reply
		res.LatencyMs = latency.Milliseconds()
	}
	// Always 200: the test RAN; ok=false carries the diagnostic. (A non-2xx
	// would conflate "the test infra failed" with "the LLM is bad".)
	writeJSON(w, http.StatusOK, res)
}
