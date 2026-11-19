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

package verify

import (
	"context"
	"slices"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
)

// Report is a session's verification as the API and the CLI show it.
type Report struct {
	State     string `json:"state"` // a Status* value
	Criteria  string `json:"criteria"`
	Model     string `json:"model"`
	MaxRounds int    `json:"max_rounds"`
	Round     int    `json:"round"`
	Verifier  string `json:"verifier"`
	// Version is the current version (v1, v2, …; every set or replace starts
	// one), or the last one once verification is off; 0 if it was never on.
	Version int `json:"version"`
	// Candidate is the number of the version's latest candidate given to the
	// verifier (0: none yet).
	Candidate int `json:"candidate"`
	// History is every change, oldest first.
	History []Change `json:"history"`
}

// Change is one VerifyEvent in a Report's history.
type Change struct {
	Action    string    `json:"action"`
	Criteria  string    `json:"criteria,omitempty"`
	Model     string    `json:"model,omitempty"`
	MaxRounds int       `json:"max_rounds"`
	By        string    `json:"by,omitempty"`
	Verifier  string    `json:"verifier,omitempty"`
	Round     int       `json:"round,omitempty"`
	Reason    string    `json:"reason,omitempty"` // exhausted: why it ended; withdrawn, stale: which candidate
	Step      int       `json:"step"`
	CreatedAt time.Time `json:"created_at"`

	// Outcome is set by verdict: what the verifier said, or how it stopped;
	// and by stale: the verdict that doesn't count.
	Outcome *event.VerifyOutcome `json:"outcome,omitempty"`
	// Version and Candidate say what the change is about (Label): the version
	// a set, replace or clear started or ended, the candidate of a harness
	// step, and for a verdict the candidate given to the verifier last before
	// it. Stale marks a verdict that didn't count.
	Version   int  `json:"version,omitempty"`
	Candidate int  `json:"candidate,omitempty"`
	Stale     bool `json:"stale,omitempty"`
}

// Describe reports a session's verification. It counts a verdict the verifier
// has already given but the agent hasn't acted on yet (the agent settles it
// when it wakes or next concludes), so the state doesn't lag the verifier.
func Describe(ctx context.Context, store db.Store, runID, sessionID string) (Report, error) {
	st, err := Load(ctx, store, runID, sessionID)
	if err != nil {
		return Report{}, err
	}
	r := Report{State: st.Status(), Round: st.Round(), Verifier: st.Verifier, Version: st.Version,
		Candidate: st.Candidates, History: []Change{}}
	if v, err := Evaluate(ctx, store, runID, st); err == nil && v.Given {
		if v.Pass {
			r.State = StatusPassed
		} else {
			st.Rejections++
			r.Round, r.State = st.Rejections+1, StatusOn
			if st.Exhausted() {
				r.Round, r.State = st.Rejections, StatusNotVerified
			}
		}
	}
	var latest *event.VerifyEvent // the request the report describes
	for i, e := range st.History {
		// A late verdict is stale even before the agent's next read records it.
		stale := e.Stale || slices.Contains(st.Late, i)
		r.History = append(r.History, Change{Action: e.Action, Criteria: e.Criteria, Model: e.Model, MaxRounds: e.MaxRounds,
			By: e.By, Verifier: e.Verifier, Round: e.Round, Reason: e.Reason, Outcome: e.Outcome, Step: e.Step,
			CreatedAt: e.CreatedAt, Version: e.Label.Version, Candidate: e.Label.Candidate, Stale: stale})
		switch e.Action {
		case event.VerifySet, event.VerifyReplace:
			latest = e.VerifyEvent
		case event.VerifyClear:
			latest = nil
		}
	}
	if latest != nil {
		r.Criteria, r.Model, r.MaxRounds = latest.Criteria, latest.Model, latest.MaxRounds
	}
	if r.State == StatusOff {
		r.Verifier = ""
	}
	return r, nil
}
