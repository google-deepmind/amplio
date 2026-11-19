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
	"time"
)

// Only PASS or FAIL at the start of the first line is a verdict; anything else
// is a FAIL labelled as no verdict, never a pass.
func TestParseVerdict(t *testing.T) {
	for _, c := range []struct {
		text     string
		pass     bool
		findings string
	}{
		{"PASS", true, ""},
		{"pass: all three tests ran green", true, "all three tests ran green"},
		{"FAIL: the build is broken\n- main.go:12", false, "the build is broken\n- main.go:12"},
		{"Fail missing units", false, "missing units"},
		{"**PASS** — checked", true, "checked"},
		{"**FAIL:** the file is missing", false, "the file is missing"},
		{"FAIL: `primes.txt` does not exist", false, "`primes.txt` does not exist"},
		{"FAIL `main.go` doesn't build", false, "`main.go` doesn't build"},
		{"`FAIL`: no tests", false, "no tests"},
		{"PASS `tests` ran", false, NoVerdict + ": PASS `tests` ran"},
		{"PASS.", true, ""},
		{"Pass rate: 2/10", false, NoVerdict + ": Pass rate: 2/10"},
		{"Pass 1: reading the trajectory", false, NoVerdict + ": Pass 1: reading the trajectory"},
		{"Pass/Fail: FAIL — build broken", false, NoVerdict + ": Pass/Fail: FAIL — build broken"},
		{"PASS all good", false, NoVerdict + ": PASS all good"},
		{"\n\n  FAIL:\nno tests were run", false, "no tests were run"},
		{"PASSED with flying colours", false, NoVerdict + ": PASSED with flying colours"},
		{"Failure to launch", false, NoVerdict + ": Failure to launch"},
		{"FAIL", false, ""},
		{"PASS ✅", true, "✅"},
		{"**PASS** ✅ all good", true, "✅ all good"},
		{"PASS, all criteria met", true, "all criteria met"},
		{"PASS (3/3 tests)", true, "(3/3 tests)"},
		{"PASS: the answer is correct.", true, "the answer is correct."},
		{"PASS The answer is correct.", false, NoVerdict + ": PASS The answer is correct."},
		{"Looks good to me. PASS", false, NoVerdict + ": Looks good to me. PASS"},
		// Joined to more, or taken back.
		{"PASS-FAIL verdict: FAIL, the file is missing", false, NoVerdict + ": PASS-FAIL verdict: FAIL, the file is missing"},
		{"Pass-rate: 2/10 tests", false, NoVerdict + ": Pass-rate: 2/10 tests"},
		{"Pass: no. The answer has no source.", false, NoVerdict + ": Pass: no. The answer has no source."},
		{"**Pass:** No", false, NoVerdict + ": **Pass:** No"},
		{"PASS? Not quite: the unit is wrong", false, NoVerdict + ": PASS? Not quite: the unit is wrong"},
		{"PASS ? maybe", false, NoVerdict + ": PASS ? maybe"},
		{"Pass — partially; the key test fails", false, NoVerdict + ": Pass — partially; the key test fails"},
		{"PASS: not all tests ran", false, NoVerdict + ": PASS: not all tests ran"},
		{"PASS: false, the file is empty", false, NoVerdict + ": PASS: false, the file is empty"},
		{"PASS — FAIL", false, NoVerdict + ": PASS — FAIL"},
		{"PASS: no issues found", true, "no issues found"},
		{"PASS - all checks green", true, "all checks green"},
		{"PASS (0 issues)", true, "(0 issues)"},
		{"FAIL-ish: close", false, "ish: close"},
		{"", false, NoVerdict},
	} {
		pass, findings := ParseVerdict(c.text)
		if pass != c.pass || findings != c.findings {
			t.Errorf("ParseVerdict(%q) = %v, %q; want %v, %q", c.text, pass, findings, c.pass, c.findings)
		}
	}
}

// A typed verdict reads back exactly: as a verifier's result text it is the
// verdict word alone on the first line, so findings that start like the other
// verdict can't flip it.
func TestFormatVerdict_ReadsBackExactly(t *testing.T) {
	for _, c := range []struct {
		pass     bool
		findings string
	}{
		{false, "PASS: Wait, let me reconsider. FAIL: primes.txt does not exist."},
		{true, "FAIL would be wrong: all 3 tests ran green\n- `go test ./...` ok"},
		{false, "**bold** first, then `code`"},
		{true, ""},
	} {
		text := FormatVerdict(c.pass, c.findings)
		if pass, findings := ParseVerdict(text); pass != c.pass || findings != c.findings {
			t.Errorf("ParseVerdict(FormatVerdict(%v, %q)) = %v, %q", c.pass, c.findings, pass, findings)
		}
	}
}

// A verifier's conclude call: the typed verdict as given (any case), and as
// the result it offers.
func TestVerdictArgs(t *testing.T) {
	for _, c := range []struct {
		args     string
		pass, ok bool
		findings string
	}{
		{`{"verdict":"FAIL","findings":"PASS: wait, no. FAIL: missing file"}`, false, true, "PASS: wait, no. FAIL: missing file"},
		{`{"verdict":"pass","findings":" ran the tests "}`, true, true, "ran the tests"},
		{`{"verdict":"MAYBE","findings":"x"}`, false, false, ""},
		{`{"result":"PASS"}`, false, false, ""},
		{`not json`, false, false, ""},
	} {
		pass, findings, ok := VerdictArgs(c.args)
		if pass != c.pass || ok != c.ok || findings != c.findings {
			t.Errorf("VerdictArgs(%s) = %v, %q, %v", c.args, pass, findings, ok)
		}
	}
	if got := ConcludeResult(`{"verdict":"FAIL","findings":"no unit"}`); got != "FAIL\n\nno unit" {
		t.Errorf("ConcludeResult(verdict) = %q", got)
	}
	// Both forms: the call is refused (the event loop), and reads as its result.
	if got := ConcludeResult(`{"verdict":"FAIL","findings":"no unit","result":"It is 42."}`); got != "It is 42." {
		t.Errorf("ConcludeResult(both forms) = %q, want the result", got)
	}
	a := &AssistantEvent{ToolCalls: []ToolCall{{Name: ConcludeTool, Arguments: `{"verdict":"PASS","findings":"ok"}`}}}
	if r, ok := a.Offered(); !ok || r != "PASS\n\nok" {
		t.Errorf("Offered = %q, %v", r, ok)
	}
	if txt := a.ToText(); !strings.Contains(txt, "PASS\n\nok") {
		t.Errorf("ToText shows the verdict as written? %s", txt)
	}
}

// What a verifier's terminal transition posts to the agent it verifies: its
// verdict read from its result, or how it stopped.
func TestVerifierOutcome(t *testing.T) {
	at := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		stopped, result string
		want            VerifyOutcome
	}{
		{"", FormatVerdict(false, "PASS: no. The file is missing."),
			VerifyOutcome{Verifier: "v", Verdict: VerdictFail, Findings: "PASS: no. The file is missing.", At: at}},
		{"", "PASS: all good", VerifyOutcome{Verifier: "v", Verdict: VerdictPass, Findings: "all good", At: at}},
		{"", "Looks fine", VerifyOutcome{Verifier: "v", Verdict: VerdictFail, Findings: NoVerdict + ": Looks fine", At: at}},
		{"crashed", "llm call: 404\n", VerifyOutcome{Verifier: "v", Verdict: "crashed", Findings: "llm call: 404", At: at}},
		{"cancelled", "parent worker concluded",
			VerifyOutcome{Verifier: "v", Verdict: "cancelled", Findings: "parent worker concluded", At: at}},
	} {
		e := VerifierOutcome("v", c.result, c.stopped, at)
		if e.Action != VerifyVerdict || e.Verifier != "v" || e.Outcome == nil || *e.Outcome != c.want {
			t.Errorf("VerifierOutcome(%q, %q) = %+v %+v, want %+v", c.result, c.stopped, e, e.Outcome, c.want)
		}
		if e.Outcome.Stopped() != (c.stopped != "") {
			t.Errorf("%q: Stopped() = %v", c.stopped, e.Outcome.Stopped())
		}
	}
	if txt := VerifierOutcome("v", "FAIL: no unit", "", at).ToText(); !strings.Contains(txt, "verdict=FAIL") ||
		!strings.Contains(txt, "no unit") {
		t.Errorf("ToText = %s", txt)
	}
}
