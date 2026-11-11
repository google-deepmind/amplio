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

package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// examplesDir is the repo's examples/skills, relative to this package.
var examplesDir = filepath.Join("..", "..", "examples", "skills")

// Every example skill loads the way amplio loads a [skills] dir: each
// directory under examples/skills has a SKILL.md that parses, named after its
// directory, and the scanner finds them all.
func TestExampleSkillsLoad(t *testing.T) {
	ents, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("read %s: %v", examplesDir, err)
	}
	dirs := map[string]bool{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dirs[e.Name()] = true
		path := filepath.Join(examplesDir, e.Name(), "SKILL.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: every example directory needs a SKILL.md: %v", e.Name(), err)
			continue
		}
		entry, ok := parseSkillMD(path, raw)
		if !ok {
			t.Errorf("%s: SKILL.md doesn't parse (needs frontmatter with name and description; see the log)", e.Name())
			continue
		}
		if entry.Name != e.Name() {
			t.Errorf("%s: SKILL.md is named %q; name it after its directory", e.Name(), entry.Name)
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no example skills under %s", examplesDir)
	}
	found := map[string]bool{}
	for _, e := range scanSkills(examplesDir) {
		found[filepath.Base(filepath.Dir(e.Path))] = true
	}
	for d := range dirs {
		if !found[d] {
			t.Errorf("%s: not found by the skills scanner", d)
		}
	}
}
