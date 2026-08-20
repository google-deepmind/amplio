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
	"encoding/json"
	"net/http"

	"amplio/internal/agent"
	"amplio/internal/config"
	"amplio/internal/workspace"
)

type toolDefDTO struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type sessionToolsDTO struct {
	AgentType string       `json:"agent_type"`
	Cwd       string       `json:"cwd"`
	Known     bool         `json:"known"` // false: this agent type does not describe its tools
	Tools     []toolDefDTO `json:"tools"`
}

// handleSessionTools describes the tools an agent of this session's type would
// be given RIGHT NOW.
//
// Reconstructed per request rather than recorded per session: the defs are a
// pure function of the agent type plus this session's workspace path, so there
// is nothing to persist and nothing to migrate. The cost is that it is a LIVE
// view — the recall tools appear only while this server has the corpora, so an
// old session shows what it would get if revived today, not necessarily what it
// had. Documented in agent.ToolLister and surfaced in the UI's wording.
func (s *Server) handleSessionTools(w http.ResponseWriter, r *http.Request) {
	runID, sid := r.PathValue("id"), r.PathValue("sid")
	sess, err := s.store.GetSession(r.Context(), runID, sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	cwd := sessionCwd(s, r, sess.Metadata)
	defs, known := agent.ToolsFor(sess.AgentType, agent.ToolContext{
		RunID:       runID,
		SessionID:   sid,
		Cwd:         cwd,
		ArtifactDir: config.ArtifactDir(runID),
		SkillIndex:  s.skillIndex,
		LessonIndex: s.lessonIndex,
	})
	out := sessionToolsDTO{AgentType: sess.AgentType, Cwd: cwd, Known: known, Tools: []toolDefDTO{}}
	for _, d := range defs {
		out.Tools = append(out.Tools, toolDefDTO{Name: d.Name, Description: d.Description, Schema: d.Schema})
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionCwd is the workspace root this session ran in. A session records its
// own workspace (sub-agents can diverge from the run's), so prefer that and
// fall back to the run's configured path — a description with the wrong path
// would be worse than one with the general one.
func sessionCwd(s *Server, r *http.Request, meta map[string]any) string {
	if raw, ok := meta[workspace.SessionMetadataKey].(string); ok && raw != "" {
		if ws, err := workspace.Unmarshal([]byte(raw)); err == nil {
			return ws.Root()
		}
	}
	if run, err := s.store.GetRun(r.Context(), r.PathValue("id")); err == nil {
		return run.Config.Workspace
	}
	return ""
}
