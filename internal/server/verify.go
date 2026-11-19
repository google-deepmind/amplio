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
	"errors"
	"fmt"
	"io"
	"net/http"

	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/verify"
)

// A session's verification (internal/verify): read openly like the rest of a
// run; set, replace and clear with the token. The operator may change any
// session's; agents change their sub-agents' through spawn_agent and
// send_message.

func (s *Server) handleGetVerify(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := s.store.GetSession(r.Context(), id, sid); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no session %q in run %q", sid, id))
		return
	}
	s.writeVerify(w, r, id, sid)
}

// handlePutVerify sets verification, or replaces it if it is on, from a
// verify.Request body (every field optional).
func (s *Server) handlePutVerify(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	var req verify.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if code, err := s.setVerify(r, id, sid, req, nil); err != nil {
		writeErr(w, code, err.Error())
		return
	}
	s.writeVerify(w, r, id, sid)
}

func (s *Server) handleDeleteVerify(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := s.store.GetSession(r.Context(), id, sid); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no session %q in run %q", sid, id))
		return
	}
	if _, err := verify.Clear(r.Context(), s.store, s.mgr.RunRegistry().GetOrCreate(id), id, sid, event.ByOperator); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeVerify(w, r, id, sid)
}

// setVerify applies the operator's request to a session, returning the HTTP
// status for a failure.
func (s *Server) setVerify(r *http.Request, runID, sid string, req verify.Request, then func()) (int, error) {
	if _, err := s.store.GetSession(r.Context(), runID, sid); err != nil {
		return http.StatusNotFound, fmt.Errorf("no session %q in run %q", sid, runID)
	}
	e, err := req.Resolve(r.Context(), event.ByOperator, s.mgr.VerifyDefaults(), s.mgr.ResolveLLM)
	if err != nil {
		return http.StatusBadRequest, err
	}
	if _, err := verify.SetThen(r.Context(), s.store, s.mgr.RunRegistry().GetOrCreate(runID), runID, sid, e, then); err != nil {
		if errors.Is(err, db.ErrStore) {
			return http.StatusInternalServerError, err
		}
		return http.StatusBadRequest, err
	}
	return 0, nil
}

func (s *Server) writeVerify(w http.ResponseWriter, r *http.Request, runID, sid string) {
	rep, err := verify.Describe(r.Context(), s.store, runID, sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
