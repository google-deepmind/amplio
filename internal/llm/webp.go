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
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"sync"

	"amplio/internal/imageutil"
)

// Images are stored as lossless WebP when that is smaller (imageutil.ToWebP),
// and most servers accept WebP. A provider whose server may not says so with a
// webp=false default, and sends its requests through WithoutWebP.

// webpFailedNote stands in for a WebP image that could not be converted for a
// server that takes no WebP.
const webpFailedNote = "[image omitted from this request: it is WebP, which this model's server does not accept, and converting it to PNG failed: %v]"

// WebPArg reads the webp= client arg, which says whether the server accepts
// WebP images. def is the provider's default, used when the arg is absent.
func WebPArg(clientArgs url.Values, def bool) (bool, error) {
	v := clientArgs.Get("webp")
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("webp=%q: want true or false", v)
	}
	return b, nil
}

// WithoutWebP returns msgs with every WebP image re-encoded as PNG, for a
// server that does not accept WebP. A stored WebP is lossless, so the PNG shows
// exactly the picture that was stored. Conversions are cached by content, since
// the same images are resent every turn. An image that cannot be converted is
// replaced by a note in its message's text, as FitImages does, rather than
// failing the request. msgs itself is not modified.
func WithoutWebP(msgs []Message) []Message {
	var out []Message
	for i, m := range msgs {
		if !slices.ContainsFunc(m.Attachments, isWebP) {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		var kept []Attachment
		text := m.Content
		for _, a := range m.Attachments {
			if !isWebP(a) {
				kept = append(kept, a)
				continue
			}
			png, err := webpToPNG(a.Base64Data)
			if err != nil {
				if text != "" {
					text += "\n"
				}
				text += fmt.Sprintf(webpFailedNote, err)
				continue
			}
			kept = append(kept, Attachment{MimeType: "image/png", Base64Data: png})
		}
		out[i].Attachments, out[i].Content = kept, text
	}
	if out == nil {
		return msgs
	}
	return out
}

func isWebP(a Attachment) bool { return a.MimeType == "image/webp" }

// toPNG converts one image; a variable so a test can count conversions.
var toPNG = imageutil.ToPNG

// webpToPNG converts one base64 WebP to a base64 PNG, through the cache.
func webpToPNG(b64 string) (string, error) {
	key := sha256.Sum256([]byte(b64))
	if v, ok := pngs.get(key); ok {
		return v, nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	png, err := toPNG(data)
	if err != nil {
		return "", err
	}
	v := base64.StdEncoding.EncodeToString(png)
	pngs.put(key, v)
	return v, nil
}

// pngCacheMaxBytes bounds the converted images kept, across every provider in
// the process. A conversion takes tens of milliseconds, so without the cache a
// history of a hundred screenshots would add seconds to every turn. 128 MiB
// holds a few hundred screenshots, and costs nothing unless a server that
// takes no WebP is in use.
const pngCacheMaxBytes = 128 << 20

var pngs = &pngCache{max: pngCacheMaxBytes}

// pngCache is a least-recently-used cache of base64 PNGs, keyed by the SHA-256
// of the base64 WebP each was converted from, and bounded by their total size.
type pngCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order list.List // of *pngEntry, most recently used first
	items map[[sha256.Size]byte]*list.Element
}

type pngEntry struct {
	key [sha256.Size]byte
	b64 string
}

func (c *pngCache) get(key [sha256.Size]byte) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*pngEntry).b64, true
}

func (c *pngCache) put(key [sha256.Size]byte, b64 string) {
	if len(b64) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return // converted twice at once; the first one in stays
	}
	if c.items == nil {
		c.items = map[[sha256.Size]byte]*list.Element{}
	}
	c.items[key] = c.order.PushFront(&pngEntry{key: key, b64: b64})
	c.size += len(b64)
	for c.size > c.max {
		old := c.order.Back()
		e := old.Value.(*pngEntry)
		c.order.Remove(old)
		delete(c.items, e.key)
		c.size -= len(e.b64)
	}
}
