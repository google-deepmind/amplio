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

package nickname

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// PickUnique indexes poolAdj/poolNoun directly, so a reservation list that
// emptied either one would panic instead of falling back to suffix mode.
func TestPoolComponentsNonEmpty(t *testing.T) {
	if len(poolAdj) == 0 || len(poolNoun) == 0 {
		t.Fatalf("poolAdj=%d poolNoun=%d, want both non-empty", len(poolAdj), len(poolNoun))
	}
}

func TestPickUnique_Basic(t *testing.T) {
	name := PickUnique(nil, nil)
	parts := strings.SplitN(name, "-", 2)
	if len(parts) != 2 {
		t.Fatalf("expected adj-noun, got %q", name)
	}
	if parts[0] == "" || parts[1] == "" {
		t.Fatalf("empty component in %q", name)
	}
}

func TestPickUnique_NeverReturnsReserved(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // test determinism
	for range 200 {
		name := PickUnique(nil, rng)
		if IsReserved(name) {
			t.Fatalf("got reserved name %q", name)
		}
	}
}

func TestPickUnique_RespectsUsed(t *testing.T) {
	used := map[string]bool{"swift-fox": true}
	rng := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // test determinism
	for range 500 {
		name := PickUnique(used, rng)
		if name == "swift-fox" {
			t.Fatal("returned a used name")
		}
	}
}

func TestPickUnique_Deterministic(t *testing.T) {
	a := PickUnique(nil, rand.New(rand.NewPCG(99, 0))) //nolint:gosec // test determinism
	b := PickUnique(nil, rand.New(rand.NewPCG(99, 0))) //nolint:gosec // test determinism
	if a != b {
		t.Fatalf("same seed produced different names: %q vs %q", a, b)
	}
}

// poolNames enumerates the dynamic pool independently of the production
// poolAdj/poolNoun slices, so tests still pin down what the pool must contain.
func poolNames() []string {
	var names []string
	for _, a := range adjectives {
		if reservedAdj[a] {
			continue
		}
		for _, n := range nouns {
			if reservedNoun[n] {
				continue
			}
			names = append(names, a+"-"+n)
		}
	}
	return names
}

// usedExcept marks the whole pool as used except the last keep names.
func usedExcept(keep int) (used map[string]bool, free []string) {
	names := poolNames()
	split := len(names) - keep
	used = make(map[string]bool, split)
	for _, n := range names[:split] {
		used[n] = true
	}
	return used, names[split:]
}

func TestPickUnique_FallbackOnExhaustion(t *testing.T) {
	used, _ := usedExcept(0)            // fill the entire dynamic pool
	rng := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // test determinism
	name := PickUnique(used, rng)
	// Fallback names have 3 segments.
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		t.Fatalf("expected adj-noun-suffix, got %q", name)
	}
	if len(parts[2]) != fallbackSuffixLen {
		t.Fatalf("suffix length %d, want %d, in %q", len(parts[2]), fallbackSuffixLen, name)
	}
	if reservedAdj[parts[0]] || reservedNoun[parts[1]] {
		t.Fatalf("fallback used a reserved component: %q", name)
	}
}

// TestPickUnique_LastFreeName is the boundary the bounded retry loop must not
// get wrong: one free name left is not exhaustion, so no suffix fallback.
func TestPickUnique_LastFreeName(t *testing.T) {
	for _, keep := range []int{1, 2, 8} {
		used, free := usedExcept(keep)
		want := map[string]bool{}
		for _, n := range free {
			want[n] = true
		}
		for seed := range uint64(4) {
			rng := rand.New(rand.NewPCG(seed, 0)) //nolint:gosec // test determinism
			if got := PickUnique(used, rng); !want[got] {
				t.Fatalf("keep=%d seed=%d: got %q, want one of the %d free names", keep, seed, got, keep)
			}
		}
	}
}

func TestPickUnique_NoReservedComponents(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // test determinism
	for range 500 {
		name := PickUnique(nil, rng)
		parts := strings.SplitN(name, "-", 2)
		if reservedAdj[parts[0]] {
			t.Fatalf("returned reserved adjective %q in %q", parts[0], name)
		}
		if reservedNoun[parts[1]] {
			t.Fatalf("returned reserved noun %q in %q", parts[1], name)
		}
	}
}

func TestIsReserved(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{RootAgent, true},
		{Chatbot, true},
		{"keen-critic", false}, // no longer reserved
		{"swift-fox", false},
		{"main-fox", false}, // "main" is reserved adj but "main-fox" is not a reserved name
		{"", false},
	}
	for _, tt := range tests {
		if got := IsReserved(tt.name); got != tt.want {
			t.Errorf("IsReserved(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// withGrid replaces the component pools with a size x size synthetic grid for
// the duration of the test and returns every name in it.
func withGrid(t *testing.T, size int) []string {
	t.Helper()
	adjs := make([]string, size)
	nns := make([]string, size)
	for i := range size {
		adjs[i] = fmt.Sprintf("a%d", i)
		nns[i] = fmt.Sprintf("n%d", i)
	}
	origAdj, origNoun := poolAdj, poolNoun
	poolAdj, poolNoun = adjs, nns
	t.Cleanup(func() { poolAdj, poolNoun = origAdj, origNoun })

	names := make([]string, 0, size*size)
	for _, a := range adjs {
		for _, n := range nns {
			names = append(names, a+"-"+n)
		}
	}
	return names
}

// TestPickUnique_UniformOverFreeNames exercises both sampling paths: a mostly
// free grid is always answered by a rejection draw, while 4 free names out of
// 144 sends ~17% of calls through the enumeration fallback, so a bias in either
// path — or between them — shows up. Seeded, so the bound cannot flake.
func TestPickUnique_UniformOverFreeNames(t *testing.T) {
	tests := []struct {
		name    string
		grid    int
		free    int
		samples int
	}{
		{"rejection_draws", 6, 12, 6000},
		{"with_enumeration_fallback", 12, 4, 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names := withGrid(t, tt.grid)
			used := make(map[string]bool, len(names))
			// Spread the free names across the grid so a positional bias shows.
			stride := len(names) / tt.free
			free := make(map[string]int, tt.free)
			for i, n := range names {
				if i%stride == 0 && len(free) < tt.free {
					free[n] = 0
					continue
				}
				used[n] = true
			}

			rng := rand.New(rand.NewPCG(7, 0)) //nolint:gosec // test determinism
			for range tt.samples {
				got := PickUnique(used, rng)
				if _, ok := free[got]; !ok {
					t.Fatalf("picked %q, which is not free", got)
				}
				free[got]++
			}

			// 5 sigma around N/k; sigma = sqrt(N/k * (1-1/k)) < sqrt(N/k).
			want := float64(tt.samples) / float64(tt.free)
			tolerance := 5 * math.Sqrt(want)
			for name, count := range free {
				if math.Abs(float64(count)-want) > tolerance {
					t.Errorf("%q picked %d times, want %.0f +/- %.0f", name, count, want, tolerance)
				}
			}
		})
	}
}

func BenchmarkPickUnique(b *testing.B) {
	half := len(poolNames()) / 2
	oneUsed := map[string]bool{"swift-fox": true}
	halfFull, _ := usedExcept(half)
	almostFull, _ := usedExcept(100)
	lastFree, _ := usedExcept(1)
	exhausted, _ := usedExcept(0)

	cases := []struct {
		name string
		used map[string]bool
	}{
		{"empty", nil},
		{"one_used", oneUsed},
		{"half_full", halfFull},
		{"100_free", almostFull},
		{"1_free", lastFree},
		{"exhausted", exhausted},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			rng := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // benchmark determinism
			b.ReportAllocs()
			for b.Loop() {
				sink = PickUnique(tc.used, rng)
			}
		})
	}
}

var sink string
