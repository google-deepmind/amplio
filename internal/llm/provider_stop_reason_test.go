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

package llm

import "testing"

func TestIsOutputLimitStopReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{"length", true},
		{"max_tokens", true},
		{"MAX_TOKENS", true},
		{" Max_Tokens ", true},
		{"stop", false},
		{"end_turn", false},
		{"tool_calls", false},
		{"SAFETY", false},
		{"content_filter", false},
		{"model_context_window_exceeded", false},
		{"", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			if got := IsOutputLimitStopReason(tc.reason); got != tc.want {
				t.Fatalf("IsOutputLimitStopReason(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}
