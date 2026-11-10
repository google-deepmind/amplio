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

package event

import (
	"strings"
	"testing"
)

// Harness-written user messages are recognised by Check, and those written
// before Check existed by the "[system] " prefix every harness nudge carries.
func TestUserEvent_IsHarness(t *testing.T) {
	for _, c := range []struct {
		e    UserEvent
		want bool
	}{
		{UserEvent{Content: "please also check X"}, false},
		{UserEvent{Content: "anything", Check: CheckLeftovers}, true},
		{UserEvent{Content: "[system] Before you conclude, these are still running:\n- ..."}, true},
	} {
		if got := c.e.IsHarness(); got != c.want {
			t.Errorf("IsHarness(%+v) = %v, want %v", c.e, got, c.want)
		}
	}
}

// Notes ride on the child_result header; an event stored before Notes existed,
// with only the old leftover field, still reads (and renders) as a note.
func TestChildResult_Notes(t *testing.T) {
	ev := &ChildResultEvent{ChildSessionID: "w", Verdict: "concluded", Content: "done",
		Notes: []ResultNote{{Kind: NoteLeftover, Text: "1 background process group"}}}
	data, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.(*ChildResultEvent).AllNotes(); len(got) != 1 || got[0] != ev.Notes[0] {
		t.Errorf("round trip notes = %+v", got)
	}
	if !strings.Contains(back.ToText(), "· left running: 1 background process group ====") {
		t.Errorf("header = %q", back.ToText())
	}

	old, err := Unmarshal([]byte(`{"type":"child_result","child_session_id":"w","verdict":"concluded","content":"done","leftover":"1 background process group still running"}`))
	if err != nil {
		t.Fatal(err)
	}
	notes := old.(*ChildResultEvent).AllNotes()
	if len(notes) != 1 || notes[0] != (ResultNote{Kind: NoteLeftover, Text: "1 background process group still running"}) {
		t.Errorf("legacy leftover read as %+v", notes)
	}
	if !strings.Contains(old.ToText(), "· left running: 1 background process group still running") {
		t.Errorf("legacy header = %q", old.ToText())
	}
}
