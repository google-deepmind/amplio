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

	"amplio/internal/util"
)

// settingsDTO is the server's settings that the UI needs before it renders.
type settingsDTO struct {
	// Timezone is the IANA name of the server's zone, which every time an agent
	// sees is in; the UI renders timestamps in it, not in the browser's zone.
	// "" if the server can't name it (the UI keeps the browser's).
	Timezone string `json:"timezone"`
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, settingsDTO{Timezone: util.LocalZoneName()})
}
