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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	pairTTL      = 120 * time.Second
	maxPairCodes = 16 // minting beyond this evicts the oldest live code
)

// pairCodes holds the one-time codes behind "Open on phone": an authed browser
// mints one and shows it as a QR; the phone trades it for the auth cookie via
// POST /api/auth/login?pair=. Memory only, so a restart voids every code. Keys
// are SHA-256 digests, so a lookup never compares the secret itself and the
// map does not hold usable codes. The zero value is ready to use.
type pairCodes struct {
	mu    sync.Mutex
	codes map[[sha256.Size]byte]time.Time // digest → expiry
	now   func() time.Time                // nil → time.Now; tests inject a clock
}

func (p *pairCodes) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// pruneLocked drops expired codes. Caller holds p.mu.
func (p *pairCodes) pruneLocked(now time.Time) {
	for k, exp := range p.codes {
		if !now.Before(exp) {
			delete(p.codes, k)
		}
	}
}

// mint returns a fresh 256-bit code and its expiry.
func (p *pairCodes) mint() (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(raw[:])

	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	p.pruneLocked(now)
	if p.codes == nil {
		p.codes = make(map[[sha256.Size]byte]time.Time)
	}
	for len(p.codes) >= maxPairCodes {
		var oldest [sha256.Size]byte
		var oldestExp time.Time
		for k, exp := range p.codes {
			if oldestExp.IsZero() || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(p.codes, oldest)
	}
	exp := now.Add(pairTTL)
	p.codes[sha256.Sum256([]byte(code))] = exp
	return code, exp, nil
}

// redeem burns code if present and reports whether it was still live.
func (p *pairCodes) redeem(code string) bool {
	if code == "" {
		return false
	}
	key := sha256.Sum256([]byte(code))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	exp, ok := p.codes[key]
	delete(p.codes, key)
	p.pruneLocked(now)
	return ok && now.Before(exp)
}

// handleAuthPair mints a pairing code for the "Open on phone" QR. Behind
// requireAuth. expires_in lets the client count down without trusting its own
// clock against the server's.
func (s *Server) handleAuthPair(w http.ResponseWriter, _ *http.Request) {
	code, exp, err := s.pairs.mint()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot mint pairing code")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": exp.UTC().Format(time.RFC3339),
		"expires_in": int(pairTTL / time.Second),
	})
}
