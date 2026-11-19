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

import "sync"

// locks serialize changes to one session's verification within this process:
// Set and Clear, and the harness's steps (starting or re-engaging the
// verifier and recording it, settling a verdict). Without it, a replace read
// before a verifier is recorded wouldn't cancel that verifier, which would
// then count for the new request. An entry lives while someone holds or waits
// for it.
var (
	locksMu sync.Mutex
	locks   = map[string]*sessionLock{} // "<run>/<session>"
)

type sessionLock struct {
	mu    sync.Mutex
	users int
}

// Lock takes the verification lock of a session and returns its unlock.
func Lock(runID, sessionID string) (unlock func()) {
	key := runID + "/" + sessionID
	locksMu.Lock()
	l := locks[key]
	if l == nil {
		l = &sessionLock{}
		locks[key] = l
	}
	l.users++
	locksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		locksMu.Lock()
		if l.users--; l.users == 0 {
			delete(locks, key)
		}
		locksMu.Unlock()
	}
}
