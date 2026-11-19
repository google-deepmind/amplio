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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"amplio/internal/config"
	"amplio/internal/event"
	"amplio/internal/loop"
	"amplio/internal/verify"
)

// LoopDefaults are the [loop] settings the loop form fills in.
type LoopDefaults struct {
	Expiry time.Duration // how long a new loop runs by default
	Never  bool          // loops don't end by default
}

// SetLoops installs the loop scheduler behind the .../loops routes and the
// defaults GET /api/settings reports. Without it, those routes answer 503.
func (s *Server) SetLoops(sched *loop.Scheduler, defaults LoopDefaults) {
	s.loops, s.loopDefaults = sched, defaults
}

// loopDTO is a loop as the API shows it.
type loopDTO struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Prompt    string         `json:"prompt"`
	Schedule  event.Schedule `json:"schedule"`
	// Verify is the verification each tick starts, its defaults filled in.
	Verify    *event.LoopVerify `json:"verify,omitempty"`
	Summary   string            `json:"summary"` // a few words, or the rule itself; see loop.Rule.Describe
	CreatedAt time.Time         `json:"created_at"`
	Ticks     int               `json:"ticks"`
	LastTick  *time.Time        `json:"last_tick,omitempty"`
	NextTick  *time.Time        `json:"next_tick,omitempty"`
}

// toLoopDTO renders times in loc, the server's zone, like the summary and the
// tick headers.
func toLoopDTO(i loop.Info, loc *time.Location) loopDTO {
	d := loopDTO{
		ID: i.ID, SessionID: i.SessionID, Prompt: i.Prompt, Schedule: i.Schedule, Verify: i.Verify,
		Summary: i.Summary, CreatedAt: i.CreatedAt.In(loc), Ticks: i.Ticks,
	}
	if !i.LastTick.IsZero() {
		t := i.LastTick.In(loc)
		d.LastTick = &t
	}
	if !i.NextTick.IsZero() {
		t := i.NextTick.In(loc)
		d.NextTick = &t
	}
	return d
}

// loopsReady answers 503 when no scheduler is installed (only `serve` runs one).
func (s *Server) loopsReady(w http.ResponseWriter) bool {
	if s.loops == nil {
		writeErr(w, http.StatusServiceUnavailable, "loops are not available in this process")
	}
	return s.loops != nil
}

// writeLoopErr maps a scheduler error to its status: the request's fault
// (400), a missing session or loop (404), or ours (500).
func writeLoopErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, loop.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, loop.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeStrict decodes a JSON body, rejecting unknown fields so a misspelt one
// ("every", "rule") fails instead of being ignored.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// handleListLoops lists a session's active loops. Ended and stopped loops are
// in its stream (LoopEvent), not here.
func (s *Server) handleListLoops(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if !s.loopsReady(w) {
		return
	}
	if _, err := s.store.GetSession(r.Context(), id, sid); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no session %q in run %q", sid, id))
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []loopDTO{}
	for _, i := range s.loops.List(id, sid) {
		out = append(out, toLoopDTO(i, s.loops.Location()))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAddLoop sets a loop on a session: {prompt, schedule: {kind, spec},
// verify?}. Clients build the schedule (and fill in the [loop] expiry
// default); the server validates it and fires the first tick at once if it is
// due. With verify ({criteria?, model?, max_rounds?}, the [verify] defaults
// filled in now), each tick starts a new version of the session's
// verification.
func (s *Server) handleAddLoop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt   string          `json:"prompt"`
		Schedule event.Schedule  `json:"schedule"`
		Verify   *verify.Request `json:"verify"`
	}
	if err := decodeStrict(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.loopsReady(w) {
		return
	}
	var v *event.LoopVerify
	if req.Verify != nil {
		var err error
		if v, err = s.loopVerify(r, *req.Verify); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	info, err := s.loops.Add(r.Context(), r.PathValue("id"), r.PathValue("sid"), req.Prompt, req.Schedule, v)
	if err != nil {
		writeLoopErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLoopDTO(info, s.loops.Location()))
}

// handleEditLoop changes a loop's prompt, schedule, verification, or several:
// {prompt?, schedule?, verify?}. verify is a request, as on POST, which
// replaces the loop's; null stops verifying its ticks.
func (s *Server) handleEditLoop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt   *string         `json:"prompt"`
		Schedule *event.Schedule `json:"schedule"`
		Verify   json.RawMessage `json:"verify"` // absent: unchanged; null: off
	}
	if err := decodeStrict(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.loopsReady(w) {
		return
	}
	c := loop.Change{Prompt: req.Prompt, Schedule: req.Schedule}
	switch v := strings.TrimSpace(string(req.Verify)); v {
	case "":
	case "null":
		c.NoVerify = true
	default:
		var vr verify.Request
		dec := json.NewDecoder(strings.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&vr); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid verify: %v", err))
			return
		}
		var err error
		if c.Verify, err = s.loopVerify(r, vr); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	info, err := s.loops.Edit(r.Context(), r.PathValue("id"), r.PathValue("sid"), r.PathValue("lid"), c)
	if err != nil {
		writeLoopErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLoopDTO(info, s.loops.Location()))
}

// loopVerify resolves a loop's verification request with the [verify]
// defaults.
func (s *Server) loopVerify(r *http.Request, req verify.Request) (*event.LoopVerify, error) {
	v, err := req.ForLoop(r.Context(), s.mgr.VerifyDefaults(), s.mgr.ResolveLLM)
	if err != nil {
		return nil, fmt.Errorf("%w: verify each tick: %v", loop.ErrInvalid, err)
	}
	return v, nil
}

// handleStopLoop stops a loop.
func (s *Server) handleStopLoop(w http.ResponseWriter, r *http.Request) {
	if !s.loopsReady(w) {
		return
	}
	if err := s.loops.Stop(r.Context(), r.PathValue("id"), r.PathValue("sid"), r.PathValue("lid")); err != nil {
		writeLoopErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// loopEventText is a LoopEvent as one short note for the chat: "loop 1 set by
// the operator: every 30m, 5 times" (", verifying each tick (…)", or ", no
// longer verifying each tick") and the prompt, or why it ended.
func (s *Server) loopEventText(e *event.LoopEvent) string {
	verb, ok := map[string]string{
		event.LoopSet: "set", event.LoopEdit: "changed", event.LoopStop: "stopped", event.LoopEnded: "ended",
	}[e.Action]
	if !ok {
		verb = e.Action
	}
	head := fmt.Sprintf("loop %s %s", e.ID, verb)
	if e.By == event.ByOperator {
		head += " by the operator"
	}
	switch {
	case e.Schedule != nil:
		loc := time.Local
		if s.loops != nil {
			loc = s.loops.Location()
		}
		summary := strings.Join(strings.Fields(e.Schedule.Spec), " ")
		if rule, err := loop.Parse(*e.Schedule, loc); err == nil {
			summary = rule.Describe()
		}
		switch {
		case e.Verify != nil:
			summary += ", verifying each tick (" + e.Verify.String() + ")"
		case e.VerifyStopped:
			summary += ", no longer verifying each tick"
		}
		return head + ": " + summary + "\n" + e.Prompt
	case e.Reason != "":
		return head + ": " + e.Reason
	}
	return head
}

// loopSettings is the "loop" part of GET /api/settings: what the loop form
// fills in. Expiry is a Go duration ("168h") or "never".
type loopSettings struct {
	Expiry             string `json:"expiry"`
	MinIntervalSeconds int    `json:"min_interval_seconds"`
}

func (s *Server) loopSettings() loopSettings {
	expiry := config.LoopExpiryNever
	if !s.loopDefaults.Never {
		d := s.loopDefaults.Expiry
		if d <= 0 {
			d, _, _ = config.Config{}.LoopExpiry()
		}
		expiry = goDuration(d)
	}
	return loopSettings{Expiry: expiry, MinIntervalSeconds: int(loop.MinInterval / time.Second)}
}

// goDuration is d.String() without its zero tail: "168h", not "168h0m0s".
func goDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
