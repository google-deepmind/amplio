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
)

// ImageLimits are the limits a provider puts on the images of one request (see
// FitImages). A zero field means no limit.
type ImageLimits struct {
	MaxImages     int // images in the request
	MaxImageBytes int // base64 bytes of one image
	MaxTotalBytes int // base64 bytes of all images together
}

// imageLowWater is how far FitImages trims once the kept images exceed a cap: down
// to this fraction of it, not just under it. The cut then moves in large steps, so
// consecutive requests share their prefix (and its prompt cache) instead of every
// new image evicting one old one.
const imageLowWater = 0.75

// The text that stands in for an omitted image, appended to its own message.
const (
	olderImageNote = "[image omitted from this request: older images are dropped to stay within the model's image limits; view it again if you still need it]"
	largeImageNote = "[image omitted from this request: %d bytes base64, over the model's limit of %d per image]"
)

// FitImages returns msgs with every image that would break lim replaced by a
// short note in its message's text, plus how many it replaced. An image too
// large on its own is always replaced; of the rest, the newest are kept. The cut
// depends only on msgs, so a history that only grows keeps the same cut until
// the next trim. msgs itself is not modified.
func FitImages(msgs []Message, lim ImageLimits) ([]Message, int) {
	type ref struct{ msg, att int }
	size := func(r ref) int { return len(msgs[r.msg].Attachments[r.att].Base64Data) }
	perImage := lim.MaxImageBytes
	if lim.MaxTotalBytes > 0 && (perImage == 0 || lim.MaxTotalBytes < perImage) {
		perImage = lim.MaxTotalBytes
	}
	over := func(count, bytes int, frac float64) bool {
		return lim.MaxImages > 0 && count > int(frac*float64(lim.MaxImages)) ||
			lim.MaxTotalBytes > 0 && bytes > int(frac*float64(lim.MaxTotalBytes))
	}

	// Walk oldest to newest, as the history grew, trimming the oldest kept
	// images whenever the newest push the window over a cap.
	notes := map[ref]string{}
	var window []ref
	count, total := 0, 0
	for i, m := range msgs {
		for j := range m.Attachments {
			r := ref{i, j}
			if perImage > 0 && size(r) > perImage {
				notes[r] = fmt.Sprintf(largeImageNote, size(r), perImage)
				continue
			}
			window = append(window, r)
			count, total = count+1, total+size(r)
			if !over(count, total, 1) {
				continue
			}
			for len(window) > 1 && over(count, total, imageLowWater) {
				old := window[0]
				window = window[1:]
				count, total = count-1, total-size(old)
				notes[old] = olderImageNote
			}
		}
	}
	if len(notes) == 0 {
		return msgs, 0
	}

	out := slices.Clone(msgs)
	for i, m := range msgs {
		var kept []Attachment
		text, omitted := m.Content, false
		for j, a := range m.Attachments {
			note, ok := notes[ref{i, j}]
			if !ok {
				kept = append(kept, a)
				continue
			}
			if text != "" {
				text += "\n"
			}
			text, omitted = text+note, true
		}
		if omitted {
			out[i].Attachments, out[i].Content = kept, text
		}
	}
	return out, len(notes)
}
