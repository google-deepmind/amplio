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
	"strings"
	"testing"
)

func testMenu() Menu {
	return Menu{Specs: []string{
		"vertex-claude:claude-opus-5?cache_ttl=1h&thinking.type=adaptive#opus",
		"vertex-claude:claude-sonnet-5?cache_ttl=1h",
		"openai{base_url=http://localhost:4000/v1&profile=litellm}:claude#via-proxy",
		"subprocess{model=exp-endpoint-7}:/opt/bridges/corp#experiment",
	}}
}

func TestMenuResolve(t *testing.T) {
	tests := []struct {
		name   string
		handle string
		want   string
	}{
		{
			name:   "nickname resolves to the entry, without the label",
			handle: "opus",
			want:   "vertex-claude:claude-opus-5?cache_ttl=1h&thinking.type=adaptive",
		},
		{
			name:   "spec matches on provider and model",
			handle: "vertex-claude:claude-sonnet-5",
			want:   "vertex-claude:claude-sonnet-5?cache_ttl=1h",
		},
		{
			name:   "caller's model args merge over the entry's, caller winning",
			handle: "vertex-claude:claude-opus-5?thinking.type=enabled&temperature=0",
			want:   "vertex-claude:claude-opus-5?cache_ttl=1h&temperature=0&thinking.type=enabled",
		},
		{
			name:   "the entry supplies the client block the caller cannot see",
			handle: "via-proxy",
			want:   "openai{base_url=http://localhost:4000/v1&profile=litellm}:claude",
		},
		{
			name:   "a nickname is enough for an entry whose model position is a path",
			handle: "experiment",
			want:   "subprocess{model=exp-endpoint-7}:/opt/bridges/corp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := testMenu().Resolve(tt.handle)
			if err != nil {
				t.Fatalf("Resolve(%q) = error %v", tt.handle, err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%q) =\n %q\nwant %q", tt.handle, got, tt.want)
			}
		})
	}
}

// TestMenuRefuses covers the half that matters for a server lending its
// credentials: everything a caller must not be able to ask for.
func TestMenuRefuses(t *testing.T) {
	tests := []struct {
		name   string
		handle string
		want   string
	}{
		{
			name:   "a model that is not on the menu",
			handle: "vertex-claude:claude-opus-9",
			want:   "not in this server's menu",
		},
		{
			name:   "an unknown nickname",
			handle: "nope",
			want:   "not in this server's menu",
		},
		{
			// The dangerous one: a caller-supplied bin= is arbitrary execution on
			// the machine holding the credentials.
			name:   "a caller-supplied client block, even for a menu model",
			handle: "subprocess{model=x}:/bin/sh",
			want:   "client args are not accepted",
		},
		{
			// And the other dangerous one: a caller-supplied base_url is an SSRF
			// read primitive.
			name:   "a caller-supplied endpoint",
			handle: "openai{base_url=http://169.254.169.254/latest/meta-data/}:claude",
			want:   "client args are not accepted",
		},
		{
			name:   "an empty handle",
			handle: "",
			want:   "no model requested",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := testMenu().Resolve(tt.handle)
			if err == nil {
				t.Fatalf("Resolve(%q) = %q, want a refusal", tt.handle, got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestMenuAmbiguity(t *testing.T) {
	// Two entries, one nickname: the operator labelled two things the same, and
	// only they know which they meant.
	dup := Menu{Specs: []string{
		"vertex-claude:claude-opus-5#fast",
		"vertex-claude:claude-sonnet-5#fast",
	}}
	if _, err := dup.Resolve("fast"); err == nil {
		t.Error("an ambiguous nickname should be refused, not guessed")
	}

	// Two entries for one model differing only in their client block: picking one
	// would be a guess with a billing consequence.
	twoEndpoints := Menu{Specs: []string{
		"openai{base_url=http://a/v1}:gpt-5#a",
		"openai{base_url=http://b/v1}:gpt-5#b",
	}}
	_, err := twoEndpoints.Resolve("openai:gpt-5")
	if err == nil {
		t.Fatal("an ambiguous model should be refused")
	}
	if !strings.Contains(err.Error(), "nickname") {
		t.Errorf("error %q should point at the way to disambiguate", err)
	}
	// …and by nickname it resolves.
	if got, err := twoEndpoints.Resolve("b"); err != nil || got != "openai{base_url=http://b/v1}:gpt-5" {
		t.Errorf("Resolve(b) = %q, %v", got, err)
	}
}

// TestMenuErrorsAreActionable: a refusal that doesn't say what WOULD have worked
// leaves the caller guessing, and the caller may be a container someone is
// debugging over a tunnel.
func TestMenuErrorsAreActionable(t *testing.T) {
	_, err := testMenu().Resolve("nope")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"opus", "via-proxy", "vertex-claude:claude-sonnet-5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list %q as an option", err, want)
		}
	}
}
