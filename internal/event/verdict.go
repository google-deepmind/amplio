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
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A verifier's verdict on an agent's conclusion (see internal/verify), as the
// stored events carry it.

// Verdicts a verifier gives.
const (
	VerdictPass = "PASS"
	VerdictFail = "FAIL"
)

// VerifyOutcome is what a verifier said about an agent's latest conclusion: a
// verdict with its findings or, for a verifier that stopped without one, its
// status and why. It rides on the VerifyEvent the verifier's own terminal write
// puts in the agent's stream (VerifyVerdict), and on what the harness tells the
// agent about it: a conclude call's result, or a verify message.
type VerifyOutcome struct {
	Verifier string `json:"verifier"`
	// Verdict is VerdictPass or VerdictFail, or the status of a verifier that
	// stopped without one (crashed, cancelled).
	Verdict string `json:"verdict"`
	// Findings are the verifier's findings; for a stop, why it stopped.
	Findings string `json:"findings,omitempty"`
	// Round and MaxRounds place a verdict told to the agent (MaxRounds 0 =
	// unbounded); neither is set on a stop (no round), a verdict on an earlier
	// candidate (none counted), or the verifier's own record.
	Round     int `json:"round,omitempty"`
	MaxRounds int `json:"max_rounds,omitempty"`
	// At is when the verifier concluded or stopped (its terminal transition):
	// with Verifier, which outcome this is, so that it is told once. Unique to
	// the millisecond, which a verifier's revive-and-conclude can't fit in.
	At time.Time `json:"at,omitzero"`
	// Earlier is set on a verdict told as information: on an earlier candidate
	// than the one pending, or after the verification moved on.
	Earlier bool `json:"earlier,omitempty"`
}

// Stopped reports whether the verifier stopped without a verdict.
func (o *VerifyOutcome) Stopped() bool { return o.Verdict != VerdictPass && o.Verdict != VerdictFail }

// VerifierOutcome is the record a verifier's terminal transition, at at,
// posts to the agent it verifies, in place of a child_result: its verdict,
// parsed from the result it concluded with (ParseVerdict), or, when it
// stopped (its status, e.g. crashed), why.
func VerifierOutcome(verifier, result, stopped string, at time.Time) *VerifyEvent {
	o := &VerifyOutcome{Verifier: verifier, Verdict: stopped, Findings: strings.TrimSpace(result), At: at}
	if stopped == "" {
		pass, findings := ParseVerdict(result)
		o.Verdict, o.Findings = VerdictFail, findings
		if pass {
			o.Verdict = VerdictPass
		}
	}
	return &VerifyEvent{Action: VerifyVerdict, By: ByHarness, Verifier: verifier, Outcome: o}
}

// VerdictArgs reads a typed verdict from a verifier's conclude call
// (ConcludeArgs.Verdict, any case). ok is false when it gives none.
func VerdictArgs(args string) (pass bool, findings string, ok bool) {
	var a ConcludeArgs
	if json.Unmarshal([]byte(args), &a) != nil {
		return false, "", false
	}
	switch strings.ToUpper(strings.TrimSpace(a.Verdict)) {
	case VerdictPass:
		return true, strings.TrimSpace(a.Findings), true
	case VerdictFail:
		return false, strings.TrimSpace(a.Findings), true
	}
	return false, "", false
}

// FormatVerdict is a typed verdict as a verifier's result text: the verdict
// word alone on the first line, then the findings, which ParseVerdict reads
// back exactly.
func FormatVerdict(pass bool, findings string) string {
	word := VerdictFail
	if pass {
		word = VerdictPass
	}
	if findings = strings.TrimSpace(findings); findings == "" {
		return word
	}
	return word + "\n\n" + findings
}

// NoVerdict labels the findings of a verifier whose result didn't start with
// PASS or FAIL. Silence is never a pass.
const NoVerdict = "verifier gave no verdict"

// ParseVerdict reads a verdict from a verifier's result text: the way a
// verifier without a typed verdict (an agent type that concludes with text)
// gives one. Its first line must start with PASS or FAIL as a whole word, in
// any case, markdown emphasis around it ignored. Reading PASS is strict, since
// a wrong PASS is the one mistake that matters. What follows the word must not
// join it to more ("PASS-FAIL", "Pass/Fail: FAIL", "PASS?"), start another
// word or a number past spaces ("Pass rate: 2/10", "Pass 1: ...", "PASS The
// ..."), or take it back past punctuation ("Pass: no.", "PASS: not quite",
// "Pass — partially"). So "PASS: ...", "PASS, ...", "PASS — ...", "PASS (3/3)"
// and "PASS ✅" pass, and the rest are no verdict. Anything else is a FAIL
// whose findings are the whole text, labelled NoVerdict.
func ParseVerdict(text string) (pass bool, findings string) {
	trimmed := strings.TrimSpace(text)
	first, rest, _ := strings.Cut(trimmed, "\n")
	line := strings.TrimLeft(first, emphasis+"#> \t")
	for _, word := range []string{VerdictPass, VerdictFail} {
		if len(line) < len(word) || !strings.EqualFold(line[:len(word)], word) {
			continue
		}
		after := strings.TrimLeft(line[len(word):], emphasis)
		if r := []rune(after); len(r) > 0 && (unicode.IsLetter(r[0]) || unicode.IsDigit(r[0])) {
			continue // "PASSED", "Failure": not the verdict word
		}
		if r := []rune(strings.TrimLeft(after, emphasis+" \t")); word == VerdictPass && (strings.HasPrefix(after, "-") ||
			len(r) > 0 && (unicode.IsLetter(r[0]) || unicode.IsDigit(r[0]) || r[0] == '/' || r[0] == '?')) {
			break
		}
		// The findings keep a leading code span ("`primes.txt` is missing"):
		// only bold and italic markers around the verdict are stripped.
		body := strings.TrimLeft(after, boldItalic+" \t")
		for _, p := range verdictPunctuation {
			body = strings.TrimPrefix(body, p)
		}
		body = strings.TrimLeft(body, boldItalic+" \t")
		if word == VerdictPass && takesBack(body) {
			break
		}
		return word == VerdictPass, strings.TrimSpace(body + "\n" + rest)
	}
	if trimmed == "" {
		return false, NoVerdict
	}
	return false, NoVerdict + ": " + trimmed
}

// takesBack reports whether text, what follows a PASS on its line, takes it
// back: a first word that negates it ("not", "false", "fail…", "partial…"),
// or "no" standing alone ("Pass: no.", but not "PASS: no issues found").
func takesBack(text string) bool {
	end := strings.IndexFunc(text, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(text)
	}
	switch word := strings.ToLower(text[:end]); {
	case word == "not", word == "false", strings.HasPrefix(word, "fail"), strings.HasPrefix(word, "partial"):
		return true
	case word == "no":
		next, _ := utf8.DecodeRuneInString(strings.TrimLeft(text[end:], " \t"))
		return !unicode.IsLetter(next) && !unicode.IsDigit(next)
	}
	return false
}

// emphasis is markdown's: what a model wraps a verdict word in.
const emphasis = boldItalic + "`"

const boldItalic = "*_"

// verdictPunctuation is stripped from the start of the findings.
var verdictPunctuation = []string{":", ".", ",", "!", "-", "–", "—"}
