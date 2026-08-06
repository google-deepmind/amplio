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

package briefing

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
)

// builtinFS holds the briefings shipped with the binary. They are parsed
// through the same front-matter path as user files, so a shipped briefing
// cannot drift from the format an operator's file must satisfy.
//
//go:embed builtin/*.md
var builtinFS embed.FS

func init() {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		panic(fmt.Sprintf("briefings: embedded dir unreadable: %v", err))
	}
	for _, e := range entries {
		p := path.Join("builtin", e.Name())
		raw, err := fs.ReadFile(builtinFS, p)
		if err != nil {
			panic(fmt.Sprintf("briefings: embedded %s unreadable: %v", p, err))
		}
		b, ok := Parse(p, raw)
		if !ok {
			// Unreachable unless a shipped file is malformed, which a test
			// catches before it ships.
			panic(fmt.Sprintf("briefings: embedded %s is malformed", p))
		}
		Register(b)
	}
}
