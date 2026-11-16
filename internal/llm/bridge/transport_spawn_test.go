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

import "testing"

// Output arrives in arbitrary chunks; the tail holds whole lines, the last one
// included even without its newline once the process is gone (flush).
func TestLineLog_KeepsWholeLines(t *testing.T) {
	tail := newRingBuffer(2)
	w := &lineLog{binary: "b", stream: "stderr", tail: tail}
	for _, chunk := range []string{"first\nsec", "ond\n", "thi", "rd"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := tail.String(); got != "first\nsecond" {
		t.Errorf("before flush: tail.String() = %q, want %q", got, "first\nsecond")
	}
	w.flush()
	if got := tail.String(); got != "second\nthird" {
		t.Errorf("after flush: tail.String() = %q, want %q", got, "second\nthird")
	}
}
