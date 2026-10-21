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

package bridge

import (
	"context"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"strings"
	"time"
)

// Transient failures on the far side of a bridge (no capacity, an internal
// error, an upstream deadline, a backend that never sent its first token) used
// to reach the event loop as-is, and an LLM call error the loop cannot compact
// away crashes the session. The other providers retry such blips themselves
// (the Anthropic SDK, gemini.withRetry, openai); a bridge is often the only hop
// in front of a shared fleet, so it retries too.

// maxAttempts is the total number of tries (1 initial + retries) for a bridge
// call that fails transiently before producing any output.
const maxAttempts = 4

// baseRetryDelay is the first backoff; it doubles each retry (capped) with
// bounded jitter. A var so tests can zero it out.
var baseRetryDelay = 2 * time.Second

const maxRetryDelay = 20 * time.Second

// transientSignatures mark a far-side failure worth retrying. Bridges relay
// the upstream error text verbatim (see Error), so these are the shapes seen
// from real backends: gRPC status names, "(code 5xx)" suffixes, and the far
// side's own first-token watchdog.
var transientSignatures = []string{
	"UNAVAILABLE",
	"DEADLINE_EXCEEDED",
	"RESOURCE_EXHAUSTED",
	"(code 429)",
	"(code 500)",
	"(code 502)",
	"(code 503)",
	"(code 504)",
	"no first token within",
	"no capacity",
	"overloaded",
}

// overflowSignatures mark a context-window overflow. It must never be retried:
// the event loop's reactive compaction depends on seeing it, and retrying the
// same oversized request cannot succeed.
var overflowSignatures = []string{
	"too long",
	"too many tokens",
	"context length",
	"context window",
	"maximum context",
	"exceeds the maximum",
	"token count exceeds",
}

// isTransient reports whether a bridge call error is a retryable far-side blip.
// Only provider-class errors qualify: a refused handle, a bad token or a
// protocol mismatch will fail the same way again, and transport errors are the
// transport's business (the subprocess transport restarts its own bridge).
func isTransient(err error) bool {
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeProvider {
		return false
	}
	msg := strings.ToLower(be.Message)
	for _, sig := range overflowSignatures {
		if strings.Contains(msg, sig) {
			return false
		}
	}
	for _, sig := range transientSignatures {
		if strings.Contains(msg, strings.ToLower(sig)) {
			return true
		}
	}
	return false
}

// retryDelay is the wait before the given (1-based) retry: exponential from
// baseRetryDelay, capped at maxRetryDelay, at 75-100% of the target so a retry
// never fires straight back into an overloaded fleet.
func retryDelay(retry int) time.Duration {
	d := baseRetryDelay << (retry - 1)
	if d > maxRetryDelay || d < 0 {
		d = maxRetryDelay
	}
	if d == 0 {
		return 0
	}
	quarter := int64(d) / 4
	//nolint:gosec // retry-backoff jitter, not a security context; math/rand is correct.
	return time.Duration(int64(d) - quarter + mrand.Int64N(quarter+1))
}

// withRetry runs attempt until it succeeds, fails non-transiently, runs out of
// attempts, or ctx ends. The last error is returned unchanged, so callers (and
// the event loop's overflow judge) see exactly what the far side said.
func withRetry[T any](ctx context.Context, model string, attempt func() (T, error)) (T, error) {
	var zero T
	for n := 1; ; n++ {
		out, err := attempt()
		if err == nil || n >= maxAttempts || !isTransient(err) || ctx.Err() != nil {
			return out, err
		}
		d := retryDelay(n)
		slog.Warn("bridge call failed transiently; retrying",
			"model", model, "attempt", n, "of", maxAttempts, "retry_in", d.Round(time.Millisecond), "error", err)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(d):
		}
	}
}
