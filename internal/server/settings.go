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
	"net/http"
	"time"

	"amplio/internal/util"
)

// settingsDTO is the server's settings that the UI needs before it renders.
type settingsDTO struct {
	// Timezone is the IANA name of the server's zone, which every time an agent
	// sees is in; the UI renders timestamps in it, not in the browser's zone.
	// "" if the server can't name it (the UI keeps the browser's).
	Timezone string `json:"timezone"`
	// Verify is the [verify] defaults, which the verification form shows as
	// placeholders.
	Verify verifySettings `json:"verify"`
	// Loop is what the loop form fills in (see loopSettings).
	Loop loopSettings `json:"loop"`
	// Now is the server's clock: a loop form builds "send now" from it, since
	// the browser's clock may be off by more than the scheduler's minute of
	// grace.
	Now time.Time `json:"now"`
}

type verifySettings struct {
	MaxRounds int    `json:"max_rounds"`
	Model     string `json:"model"` // "" = the verified agent's own
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	d := s.mgr.VerifyDefaults()
	writeJSON(w, http.StatusOK, settingsDTO{Timezone: util.LocalZoneName(),
		Verify: verifySettings{MaxRounds: d.MaxRounds, Model: d.Model}, Loop: s.loopSettings(), Now: time.Now()})
}
