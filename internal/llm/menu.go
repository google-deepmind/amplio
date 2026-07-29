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
	"net/url"
	"sort"
	"strings"
)

// A Menu is the set of llm models a server offers, and — when it is serving
// generations for someone else — the set it will accept. One mechanism does both
// jobs, which is the point: resolution has to consult the menu anyway to
// interpret nicknames, so the restriction costs nothing.
//
// THE RULE: the handle names WHICH model; the menu supplies HOW TO REACH IT.
//
//	handle              what it must match
//	------              ------------------
//	a nickname          exactly one menu entry's #nickname
//	a spec              exactly one menu entry's (provider, model)
//
// and then:
//
//   - the CLIENT BLOCK comes from the menu entry, never from the caller.
//     Endpoints, binaries and credential references are the server's business.
//   - the caller's MODEL ARGS are merged over the entry's, caller winning, so
//     temperature and thinking knobs stay freely variable. They are the model's
//     vocabulary, the model validates them, and nothing dangerous lives there.
//
// Matching on (provider, model) rather than on the whole canonical spec is
// deliberate: an exact-spec rule would refuse a perfectly reasonable
// ?temperature=0.2 on a model the operator has already allowed.
type Menu struct {
	// Specs are the menu's entries, verbatim and in display order.
	Specs []string
}

// Resolve turns a caller's handle into a spec this server will run, or explains
// why it won't. The returned spec is canonical and carries no nickname: it is
// for construction, not display.
func (m Menu) Resolve(handle string) (string, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return "", fmt.Errorf("no model requested; want a spec or a nickname from this server's menu (%s)", m.summary())
	}
	entries := m.parse()
	if len(entries) == 0 {
		return "", fmt.Errorf("this server offers no models")
	}

	if req, err := ParseSpec(handle); err == nil {
		if spec, err := resolveSpec(entries, req); err == nil {
			return spec, nil
		} else if _, isNick := matchNickname(entries, handle); !isNick {
			// Not a nickname either, so the spec-shaped error is the useful one.
			return "", err
		}
	}
	entry, ok := matchNickname(entries, handle)
	if !ok {
		return "", fmt.Errorf("%q is not in this server's menu; it offers %s", handle, m.summary())
	}
	return entry.String(), nil
}

// resolveSpec applies the rule above to a handle that parses as a spec.
func resolveSpec(entries []*Spec, req *Spec) (string, error) {
	if len(req.Client) > 0 {
		return "", fmt.Errorf("client args are not accepted from a caller (%s): this server supplies them",
			strings.Join(sortedKeys(req.Client), ", "))
	}
	reqModel, reqArgs, err := req.Model()
	if err != nil {
		return "", err
	}
	var matches []*Spec
	for _, e := range entries {
		model, _, err := e.Model()
		if err != nil {
			continue
		}
		if e.Provider == req.Provider && model == reqModel {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s:%s is not in this server's menu", req.Provider, reqModel)
	case 1:
	default:
		// Two entries for one model that differ in their client block (two
		// endpoints for the same model name, say). Picking one would be a guess
		// with a billing consequence.
		return "", fmt.Errorf("%s:%s matches %d menu entries; ask for one by nickname",
			req.Provider, reqModel, len(matches))
	}

	entry := matches[0]
	model, args, err := entry.Model()
	if err != nil {
		return "", err
	}
	// Caller's model args win: they are the model's own vocabulary, and the model
	// validates them.
	merged := url.Values{}
	for k, v := range args {
		merged[k] = v
	}
	for k, v := range reqArgs {
		merged[k] = v
	}
	tail := model
	if len(merged) > 0 {
		tail = model + "?" + EncodeArgs(merged)
	}
	resolved := &Spec{Provider: entry.Provider, Client: entry.Client, Tail: tail}
	return resolved.String(), nil
}

// matchNickname resolves a handle against the menu's nicknames. A nickname
// shared by two entries is refused rather than guessed — the operator wrote the
// same label twice, and only they know which they meant.
func matchNickname(entries []*Spec, handle string) (*Spec, bool) {
	var hit *Spec
	for _, e := range entries {
		if e.Nickname != handle {
			continue
		}
		if hit != nil {
			return nil, false // ambiguous
		}
		hit = e
	}
	if hit == nil {
		return nil, false
	}
	// Drop the nickname: it is a display label, and the resolved spec is for
	// construction.
	return &Spec{Provider: hit.Provider, Client: hit.Client, Tail: hit.Tail}, true
}

func (m Menu) parse() []*Spec {
	out := make([]*Spec, 0, len(m.Specs))
	for _, s := range m.Specs {
		sp, err := ParseSpec(s)
		if err != nil {
			continue // a menu entry that doesn't parse can't be run locally either
		}
		out = append(out, sp)
	}
	return out
}

// summary lists what a caller could have asked for. Nicknames first, since they
// are the stable handles; then bare provider:model pairs. Truncated, because an
// error is not a catalogue.
func (m Menu) summary() string {
	const maxItems = 12
	seen := map[string]bool{}
	var items []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		items = append(items, s)
	}
	for _, e := range m.parse() {
		add(e.Nickname)
	}
	for _, e := range m.parse() {
		if model, _, err := e.Model(); err == nil {
			add(e.Provider + ":" + model)
		}
	}
	if len(items) == 0 {
		return "nothing"
	}
	if len(items) > maxItems {
		return strings.Join(items[:maxItems], ", ") + fmt.Sprintf(", … (%d more)", len(items)-maxItems)
	}
	return strings.Join(items, ", ")
}

func sortedKeys(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
