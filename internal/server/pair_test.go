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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for pairCodes.now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newPairServer(t *testing.T) (*httptest.Server, *fakeClock) {
	t.Helper()
	srv, _, _ := newTestServer(t)
	clk := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	srv.pairs.now = clk.now
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, clk
}

// mintPair mints a code with the owner's token and returns it.
func mintPair(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	code, body := doReq(t, http.MethodPost, ts.URL+"/api/auth/pair?token=secret", "")
	if code != http.StatusOK {
		t.Fatalf("mint: %d %s", code, body)
	}
	var got struct {
		Code      string `json:"code"`
		ExpiresAt string `json:"expires_at"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(got.Code); err != nil || len(raw) != 32 {
		t.Fatalf("code %q: want 32 bytes base64url (err %v)", got.Code, err)
	}
	if got.ExpiresAt != "2026-01-02T03:06:05Z" || got.ExpiresIn != 120 {
		t.Errorf("expiry = %q / %ds, want 2026-01-02T03:06:05Z / 120s", got.ExpiresAt, got.ExpiresIn)
	}
	return got.Code
}

// pairLogin redeems code and returns the status and the auth cookie it set.
func pairLogin(t *testing.T, ts *httptest.Server, query string) (int, *http.Cookie) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/auth/login?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == authCookie {
			return resp.StatusCode, c
		}
	}
	return resp.StatusCode, nil
}

func TestPair_MintRequiresAuth(t *testing.T) {
	t.Parallel()
	ts, _ := newPairServer(t)
	for _, q := range []string{"", "?token=wrong"} {
		if code, _ := doReq(t, http.MethodPost, ts.URL+"/api/auth/pair"+q, ""); code != http.StatusUnauthorized {
			t.Errorf("mint %q: %d, want 401", q, code)
		}
	}
	// GET falls through to the SPA handler (index.html when a build is
	// embedded), never to the minting handler.
	if _, body := doReq(t, http.MethodGet, ts.URL+"/api/auth/pair?token=secret", ""); strings.Contains(string(body), `"code"`) {
		t.Errorf("GET minted a code: %s", body)
	}
}

func TestPair_SingleUse(t *testing.T) {
	t.Parallel()
	ts, _ := newPairServer(t)
	code := mintPair(t, ts)

	status, pc := pairLogin(t, ts, "pair="+code)
	if status != http.StatusOK || pc == nil {
		t.Fatalf("first redeem: %d cookie=%v, want 200 + cookie", status, pc)
	}
	if status, c := pairLogin(t, ts, "pair="+code); status != http.StatusUnauthorized || c != nil {
		t.Errorf("second redeem: %d cookie=%v, want 401, no cookie", status, c)
	}

	// The cookie is byte-for-byte the one token login sets.
	_, tc := pairLogin(t, ts, "token=secret")
	if tc == nil {
		t.Fatal("token login set no cookie")
	}
	if pc.String() != tc.String() {
		t.Errorf("pair cookie %q != token cookie %q", pc.String(), tc.String())
	}
	if !pc.HttpOnly || pc.SameSite != http.SameSiteLaxMode || pc.Path != "/" || pc.Value != "secret" {
		t.Errorf("cookie attributes: %+v", pc)
	}
}

func TestPair_Expired(t *testing.T) {
	t.Parallel()
	ts, clk := newPairServer(t)
	live := mintPair(t, ts)
	dead := mintPair(t, ts)

	clk.advance(pairTTL - time.Second)
	if status, _ := pairLogin(t, ts, "pair="+live); status != http.StatusOK {
		t.Errorf("redeem at TTL-1s: %d, want 200", status)
	}
	clk.advance(time.Second)
	if status, c := pairLogin(t, ts, "pair="+dead); status != http.StatusUnauthorized || c != nil {
		t.Errorf("redeem at TTL: %d cookie=%v, want 401, no cookie", status, c)
	}
}

func TestPair_Forged(t *testing.T) {
	t.Parallel()
	ts, _ := newPairServer(t)
	minted := mintPair(t, ts)
	forged := []string{
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		minted[:len(minted)-1],
		strings.ToUpper(minted),
		"secret", // the long-lived token is not a pairing code
	}
	for _, f := range forged {
		if f == minted {
			continue
		}
		if status, c := pairLogin(t, ts, "pair="+f); status != http.StatusUnauthorized || c != nil {
			t.Errorf("forged %q: %d cookie=%v, want 401, no cookie", f, status, c)
		}
	}
	// Forgeries do not burn the minted code.
	if status, _ := pairLogin(t, ts, "pair="+minted); status != http.StatusOK {
		t.Errorf("minted code after forgeries: %d, want 200", status)
	}
	// An empty pair falls through to the unchanged token path.
	if status, _ := pairLogin(t, ts, "pair=&token=secret"); status != http.StatusOK {
		t.Errorf("empty pair + token: %d, want 200", status)
	}
}

func TestPair_PruneAndCap(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	p := &pairCodes{now: clk.now}

	first, _, _ := p.mint()
	codes := []string{first}
	for range maxPairCodes {
		clk.advance(time.Second)
		c, _, err := p.mint()
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	if len(p.codes) != maxPairCodes {
		t.Fatalf("live codes = %d, want cap %d", len(p.codes), maxPairCodes)
	}
	if p.redeem(first) {
		t.Error("oldest code survived the cap, want evicted")
	}
	if !p.redeem(codes[1]) {
		t.Error("second-oldest code evicted, want live")
	}

	// Minting after everything expired leaves only the new code.
	clk.advance(pairTTL)
	if _, _, err := p.mint(); err != nil {
		t.Fatal(err)
	}
	if len(p.codes) != 1 {
		t.Errorf("after expiry, live codes = %d, want 1 (pruned)", len(p.codes))
	}
}
