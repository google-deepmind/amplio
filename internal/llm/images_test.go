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

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// imageHistory is an agent-style history: one tool result per image, image i
// (1-based, tool call "call<i>") being sizes[i-1] base64 bytes.
func imageHistory(sizes ...int) []Message {
	msgs := []Message{{Role: RoleUser, Content: "task"}}
	for i, n := range sizes {
		id := fmt.Sprintf("call%d", i+1)
		msgs = append(msgs,
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "view_file"}}},
			Message{Role: RoleToolResult, ToolCallID: id, Content: fmt.Sprintf("Image: %d.png", i+1),
				Attachments: []Attachment{{MimeType: "image/png", Base64Data: strings.Repeat("A", n)}}})
	}
	return msgs
}

func repeatSize(n, size int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = size
	}
	return s
}

// keptImages lists the 1-based numbers of the images still attached.
func keptImages(msgs []Message) []int {
	var kept []int
	for _, m := range msgs {
		if m.Role == RoleToolResult && len(m.Attachments) > 0 {
			n, _ := strconv.Atoi(strings.TrimPrefix(m.ToolCallID, "call"))
			kept = append(kept, n)
		}
	}
	return kept
}

func totalImageBytes(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		for _, a := range m.Attachments {
			total += len(a.Base64Data)
		}
	}
	return total
}

func TestFitImages_WithinLimitsIsANoOp(t *testing.T) {
	msgs := imageHistory(10, 10, 10)
	for _, lim := range []ImageLimits{{}, {MaxImages: 3, MaxImageBytes: 10, MaxTotalBytes: 30}} {
		out, omitted := FitImages(msgs, lim)
		if omitted != 0 || &out[0] != &msgs[0] {
			t.Errorf("%+v: omitted %d, copied=%v; want the input back untouched", lim, omitted, &out[0] != &msgs[0])
		}
	}
}

func TestFitImages_CountCapKeepsTheNewest(t *testing.T) {
	msgs := imageHistory(repeatSize(10, 1)...)
	out, omitted := FitImages(msgs, ImageLimits{MaxImages: 4})
	if got, want := keptImages(out), []int{7, 8, 9, 10}; !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if omitted != 6 {
		t.Errorf("omitted = %d, want 6", omitted)
	}
	// Each dropped image leaves a note in its own tool result, so the model knows
	// it saw an image there and can view it again.
	for _, m := range out {
		if m.Role != RoleToolResult || len(m.Attachments) > 0 {
			continue
		}
		if !strings.HasPrefix(m.Content, "Image: ") || !strings.HasSuffix(m.Content, "\n"+olderImageNote) {
			t.Errorf("dropped image's result = %q, want its text plus the note", m.Content)
		}
	}
}

func TestFitImages_ByteBudgetKeepsTheNewest(t *testing.T) {
	msgs := imageHistory(30, 30, 30, 30, 30, 30, 50)
	lim := ImageLimits{MaxTotalBytes: 100}
	out, _ := FitImages(msgs, lim)
	kept := keptImages(out)
	if got := totalImageBytes(out); got > lim.MaxTotalBytes {
		t.Errorf("kept %d bytes, over the %d budget", got, lim.MaxTotalBytes)
	}
	if kept[len(kept)-1] != 7 {
		t.Errorf("kept %v; the newest image must survive", kept)
	}
	for i := 1; i < len(kept); i++ {
		if kept[i] != kept[i-1]+1 {
			t.Errorf("kept %v; want a run of the newest images", kept)
		}
	}
}

func TestFitImages_DropsAnImageTooLargeOnItsOwn(t *testing.T) {
	msgs := imageHistory(10, 10, 500)
	out, omitted := FitImages(msgs, ImageLimits{MaxImageBytes: 100})
	if got, want := keptImages(out), []int{1, 2}; !slices.Equal(got, want) || omitted != 1 {
		t.Fatalf("kept %v (omitted %d), want %v", got, omitted, want)
	}
	last := out[len(out)-1]
	if want := fmt.Sprintf(largeImageNote, 500, 100); !strings.HasSuffix(last.Content, want) {
		t.Errorf("oversized image's result = %q, want the size note", last.Content)
	}
	// The request total is a per-image limit too: one image can't exceed it.
	if _, omitted := FitImages(imageHistory(500), ImageLimits{MaxTotalBytes: 100}); omitted != 1 {
		t.Errorf("an image over the whole budget was kept")
	}
}

// Once a trim happens, the following requests keep the same cut until the
// budget overflows again, so they can reuse the cached prefix; the cut never
// moves back.
func TestFitImages_CutMovesForwardInSteps(t *testing.T) {
	const n = 40
	full := imageHistory(repeatSize(n, 1)...)
	lim := ImageLimits{MaxImages: 8}
	var prev []Message
	changes := 0
	for k := 1; k <= n; k++ {
		msgs := full[:1+2*k] // the history after k images
		out, _ := FitImages(msgs, lim)
		if kept := keptImages(out); len(kept) > lim.MaxImages || kept[len(kept)-1] != k {
			t.Fatalf("after %d images kept %v", k, kept)
		}
		if prev != nil {
			shared := out[:len(prev)]
			if !slices.EqualFunc(shared, prev, sameMessage) {
				changes++
				// Changed: only by dropping more of the oldest images.
				if first := keptImages(shared)[0]; first <= keptImages(prev)[0] {
					t.Fatalf("after %d images the cut moved back or sideways", k)
				}
			}
		}
		prev = out
	}
	// Each trim goes down to 3/4 of the cap (6 of 8), so the prefix changes on
	// every third request past the 8th image, not on every one.
	if want := (n - lim.MaxImages + 2) / 3; changes > want { // ⌈32/3⌉ = 11
		t.Errorf("prefix changed on %d of %d requests; want at most %d", changes, n, want)
	}
}

func sameMessage(a, b Message) bool {
	return a.Content == b.Content && slices.Equal(a.Attachments, b.Attachments)
}

func TestFitImages_LeavesTheInputAlone(t *testing.T) {
	msgs := imageHistory(1, 1, 1)
	before := slices.Clone(msgs)
	if _, omitted := FitImages(msgs, ImageLimits{MaxImages: 1}); omitted == 0 {
		t.Fatal("expected some images omitted")
	}
	if !slices.EqualFunc(msgs, before, sameMessage) {
		t.Error("FitImages modified its input")
	}
}

// A message carrying several images (a user turn) gets one note per dropped
// image, and an empty text gets no leading newline.
func TestFitImages_SeveralImagesInOneMessage(t *testing.T) {
	att := Attachment{MimeType: "image/png", Base64Data: "AAAA"}
	msgs := []Message{
		{Role: RoleUser, Attachments: []Attachment{att, att, att}},
		{Role: RoleUser, Content: "newest", Attachments: []Attachment{att}},
	}
	out, omitted := FitImages(msgs, ImageLimits{MaxImages: 1})
	if omitted != 3 || len(out[0].Attachments) != 0 || len(out[1].Attachments) != 1 {
		t.Fatalf("omitted %d; kept %d and %d images", omitted, len(out[0].Attachments), len(out[1].Attachments))
	}
	if want := strings.Join([]string{olderImageNote, olderImageNote, olderImageNote}, "\n"); out[0].Content != want {
		t.Errorf("content = %q, want three notes", out[0].Content)
	}
}
