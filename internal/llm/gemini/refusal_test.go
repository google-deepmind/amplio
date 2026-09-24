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

package gemini

import (
	"testing"

	"amplio/internal/llm"

	"google.golang.org/genai"
)

// streamOf replays chunks through geminiStream — the path the event loop uses
// for interactive agents — and returns the accumulated response.
func streamOf(t *testing.T, chunks ...*genai.GenerateContentResponse) *llm.Response {
	t.Helper()
	i := 0
	s := &geminiStream{
		next: func() (*genai.GenerateContentResponse, error, bool) {
			if i >= len(chunks) {
				return nil, nil, false
			}
			i++
			return chunks[i-1], nil, true
		},
		stop: func() {},
	}
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return s.Response()
}

// callOf runs one response through the blocking Call path's accumulation.
func callOf(resp *genai.GenerateContentResponse) *llm.Response {
	var acc respAcc
	acc.add(resp)
	return acc.response()
}

var dangerousHigh = &genai.SafetyRating{
	Category: genai.HarmCategoryDangerousContent, Probability: genai.HarmProbabilityHigh, Blocked: true,
}

// A blocked PROMPT comes back as promptFeedback with no candidates at all.
// Before, that turn persisted as an empty reply with an EMPTY stop reason.
func TestRefusal_PromptBlocked(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		PromptFeedback: &genai.GenerateContentResponsePromptFeedback{
			BlockReason:        genai.BlockedReasonProhibitedContent,
			BlockReasonMessage: "The prompt was blocked due to prohibited content.",
			SafetyRatings:      []*genai.SafetyRating{dangerousHigh},
		},
	}
	for path, r := range map[string]*llm.Response{"Call": callOf(resp), "Stream": streamOf(t, resp)} {
		if r.StopReason != promptBlockedStopReason {
			t.Errorf("%s: StopReason = %q, want %q", path, r.StopReason, promptBlockedStopReason)
		}
		ref := r.Refusal
		if ref == nil {
			t.Errorf("%s: Refusal = nil, want set", path)
			continue
		}
		if ref.Category != "PROHIBITED_CONTENT" || ref.Explanation != "The prompt was blocked due to prohibited content." {
			t.Errorf("%s: Refusal = %+v", path, ref)
		}
	}
}

// With no blockReasonMessage, the explanation falls back to the ratings that
// actually caused the block (non-blocking ratings are left out).
func TestRefusal_PromptBlockedExplainsFromRatings(t *testing.T) {
	r := callOf(&genai.GenerateContentResponse{
		PromptFeedback: &genai.GenerateContentResponsePromptFeedback{
			BlockReason: genai.BlockedReasonSafety,
			SafetyRatings: []*genai.SafetyRating{
				{Category: genai.HarmCategoryHarassment, Probability: genai.HarmProbabilityLow},
				dangerousHigh,
			},
		},
	})
	if r.Refusal == nil || r.Refusal.Category != "SAFETY" {
		t.Fatalf("Refusal = %+v, want category SAFETY", r.Refusal)
	}
	if want := "blocked: HARM_CATEGORY_DANGEROUS_CONTENT (HIGH)"; r.Refusal.Explanation != want {
		t.Errorf("Explanation = %q, want %q", r.Refusal.Explanation, want)
	}
}

// Output stopped by a policy filter: the candidate's finishReason names it.
func TestRefusal_OutputBlocked(t *testing.T) {
	resp := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		FinishReason:  genai.FinishReasonSafety,
		SafetyRatings: []*genai.SafetyRating{dangerousHigh},
	}}}
	for path, r := range map[string]*llm.Response{"Call": callOf(resp), "Stream": streamOf(t, resp)} {
		if r.StopReason != "SAFETY" {
			t.Errorf("%s: StopReason = %q, want SAFETY (native, unchanged)", path, r.StopReason)
		}
		ref := r.Refusal
		if ref == nil {
			t.Errorf("%s: Refusal = nil, want set", path)
			continue
		}
		if ref.Category != "SAFETY" || ref.Explanation != "blocked: HARM_CATEGORY_DANGEROUS_CONTENT (HIGH)" {
			t.Errorf("%s: Refusal = %+v", path, ref)
		}
	}
}

// Streaming: text arrives first, then a later chunk stops it with a filter
// reason and message. The partial text is kept and the refusal recorded.
func TestRefusal_OutputBlockedMidStream(t *testing.T) {
	r := streamOf(t,
		&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{{Text: "Here is "}}},
		}}},
		&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			FinishReason:  genai.FinishReasonRecitation,
			FinishMessage: "Output stopped: recitation of copyrighted material.",
		}}},
	)
	if r.Content != "Here is " {
		t.Errorf("Content = %q, want the partial text kept", r.Content)
	}
	if r.Refusal == nil || r.Refusal.Category != "RECITATION" ||
		r.Refusal.Explanation != "Output stopped: recitation of copyrighted material." {
		t.Errorf("Refusal = %+v, want RECITATION with the finishMessage", r.Refusal)
	}
}

// Ordinary endings — including non-policy failures like a malformed tool call —
// are not refusals.
func TestRefusal_NormalEndingsUntouched(t *testing.T) {
	for _, fr := range []genai.FinishReason{
		genai.FinishReasonStop, genai.FinishReasonMaxTokens, genai.FinishReasonMalformedFunctionCall, genai.FinishReasonOther,
	} {
		r := callOf(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			FinishReason:  fr,
			FinishMessage: "some message",
			Content:       &genai.Content{Parts: []*genai.Part{{Text: "ok"}}},
		}}})
		if r.Refusal != nil {
			t.Errorf("%s: Refusal = %+v, want nil", fr, r.Refusal)
		}
		if r.StopReason != string(fr) {
			t.Errorf("%s: StopReason = %q, want it unchanged", fr, r.StopReason)
		}
	}
}

// A non-refusal abnormal stop keeps Gemini's finishMessage as StopMessage (the
// shape of a real MALFORMED_FUNCTION_CALL response). On a refusal the message
// is the Refusal's Explanation instead, not both.
func TestStopMessage(t *testing.T) {
	const malformed = "Malformed function call: print(default_api.get_symbol_details(id=1))"
	r := streamOf(t, &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		FinishReason: genai.FinishReasonMalformedFunctionCall, FinishMessage: malformed,
	}}})
	if r.Refusal != nil || r.StopReason != "MALFORMED_FUNCTION_CALL" || r.StopMessage != malformed {
		t.Errorf("malformed call: stop=%q message=%q refusal=%+v, want the finishMessage as StopMessage", r.StopReason, r.StopMessage, r.Refusal)
	}

	const recited = "The generated content was filtered because it may contain material that resembles existing copyrighted works."
	r = callOf(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		FinishReason: genai.FinishReasonRecitation, FinishMessage: recited,
	}}})
	if r.Refusal == nil || r.Refusal.Explanation != recited || r.StopMessage != "" {
		t.Errorf("recitation: refusal=%+v message=%q, want the finishMessage only as the Explanation", r.Refusal, r.StopMessage)
	}
}
