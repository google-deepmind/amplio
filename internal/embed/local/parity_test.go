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

package local

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"testing"

	"amplio/internal/vec"
)

// testdata/parity.json was generated once from the Python reference: token ids
// from model2vec's own tokenize(max_length=512) at the pinned revision, and
// pairwise cosines from the table the recipe was evaluated with. Cosines are
// rotation-invariant, so they hold for any table derived by the same recipe,
// whatever sign conventions its PCA solver picked.
type parityCase struct {
	Text string  `json:"text"`
	IDs  []int32 `json:"ids"`
}

// cosineTolerance allows for low-order differences in the PCA solve and the
// int8 rounding they occasionally flip; a tokenizer or pooling bug moves
// cosines by far more.
const cosineTolerance = 2e-3

func TestParityWithReference(t *testing.T) {
	if !Available() {
		t.Skip("binary built without the embedding table (run make embed-table)")
	}
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases   []parityCase `json:"cases"`
		Cosines []float64    `json:"cosines"` // upper triangle, row-major
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, len(golden.Cases))
	for i, c := range golden.Cases {
		texts[i] = c.Text
		if got := e.m.tok.tokenize(c.Text); !slices.Equal(got, c.IDs) && !(len(got) == 0 && len(c.IDs) == 0) {
			t.Errorf("case %d %.40q: token ids differ from the reference\n got  %v\n want %v", i, c.Text, got, c.IDs)
		}
	}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	k, worst := 0, 0.0
	for i := range vecs {
		for j := i + 1; j < len(vecs); j++ {
			got, want := vec.Dot(vecs[i], vecs[j]), golden.Cosines[k]
			k++
			worst = max(worst, math.Abs(got-want))
			if math.Abs(got-want) > cosineTolerance {
				t.Errorf("cos(case %d, case %d) = %.6f, reference %.6f", i, j, got, want)
			}
		}
	}
	if k != len(golden.Cosines) {
		t.Fatalf("compared %d cosines, golden has %d", k, len(golden.Cosines))
	}
	t.Logf("%d cases, %d pairwise cosines, max |delta| = %.2g", len(texts), k, worst)
}
