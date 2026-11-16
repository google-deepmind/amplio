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

package proctree

import "testing"

func TestEnvValue(t *testing.T) {
	for _, c := range []struct {
		name, env, want string
	}{
		{"entry", "A=1\x00AMPLIO_RUN_ID=r1\x00B=2\x00", "r1"},
		{"last entry", "A=1\x00AMPLIO_RUN_ID=r1\x00", "r1"},
		{"empty value", "AMPLIO_RUN_ID=\x00B=2\x00", ""},
		{"absent", "A=1\x00AMPLIO_RUN_IDX=r1\x00", ""},
		{"a longer name", "AMPLIO_RUN_ID_2=x\x00AMPLIO_RUN_ID=r1\x00", "r1"},
		// bash importing its environment: the '=' is a NUL for a moment
		// (seen in /proc/<pid>/environ of a bash starting up).
		{"split in place by bash", "A=1\x00AMPLIO_RUN_ID\x00r1\x00B=2\x00", "r1"},
		{"split, value empty", "AMPLIO_RUN_ID\x00\x00B=2\x00", ""},
	} {
		if got := envValue([]byte(c.env), "AMPLIO_RUN_ID"); got != c.want {
			t.Errorf("%s: envValue = %q, want %q", c.name, got, c.want)
		}
	}
}
