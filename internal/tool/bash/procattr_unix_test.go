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

// Package bash provides the bash command execution tool.

//go:build unix

package bash

import "testing"

// Group ids that aren't a real background group must never probe as alive:
// kill(0) would probe amplio's own group and kill(-1) every process.
func TestGroupAlive_RejectsSpecialIDs(t *testing.T) {
	for _, pgid := range []int{0, 1, -5} {
		if groupAlive(pgid) {
			t.Errorf("groupAlive(%d) = true, want false", pgid)
		}
	}
}
