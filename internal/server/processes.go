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
	"sync"
	"time"

	"amplio/internal/proctree"
)

// procCacheTTL coalesces concurrent viewers onto one scan. A scan walks /proc,
// so N open tabs would otherwise mean N walks; within the TTL they all get the
// same sample. Short enough that the page still feels live.
const procCacheTTL = time.Second

type procCache struct {
	mu    sync.Mutex
	byRun map[string]proctree.Snapshot
}

func (c *procCache) get(runID string) (proctree.Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, ok := c.byRun[runID]
	if !ok || time.Since(snap.TakenAt) > procCacheTTL {
		return proctree.Snapshot{}, false
	}
	return snap, true
}

func (c *procCache) put(runID string, snap proctree.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRun == nil {
		c.byRun = map[string]proctree.Snapshot{}
	}
	c.byRun[runID] = snap
}

// handleProcesses lists the OS processes still running from a run's bash calls.
//
// Behind requireAuth, unlike every other GET: a command line routinely carries
// tokens, hostnames and paths that the rest of the read-only API never exposes.
//
// Sampled on request rather than pushed: the kernel has no event to subscribe
// to for an arbitrary descendant's fork or exit, and scanning on a timer would
// burn ~5% of a core at a 2s cadence with nobody watching.
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.store.GetRun(r.Context(), runID); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if snap, ok := s.procCache.get(runID); ok {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	snap := proctree.Scan(runID)
	s.procCache.put(runID, snap)
	writeJSON(w, http.StatusOK, snap)
}
