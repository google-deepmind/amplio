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

// Package vcstest makes the git and jj repos of tests independent of the
// developer's configuration of those tools.
package vcstest

import (
	"os"
	"path/filepath"
	"testing"
)

// Name and Email are the identity Hermetic commits as.
const (
	Name  = "tester"
	Email = "t@example.com"
)

// Hermetic makes git and jj, run by the test or by the code under test (which
// inherits the environment), ignore the user's and the system's config:
// commit signing, hooks and templates there make a test repo slow and
// machine-dependent. It sets a fixed commit identity instead, and gives jj a
// temp config dir for the per-repo state it keeps there.
func Hermetic(t testing.TB) {
	t.Helper()
	dir := t.TempDir()
	jjConfig := filepath.Join(dir, "jj.toml")
	identity := "[user]\nname = \"" + Name + "\"\nemail = \"" + Email + "\"\n"
	if err := os.WriteFile(jjConfig, []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     Name,
		"GIT_AUTHOR_EMAIL":    Email,
		"GIT_COMMITTER_NAME":  Name,
		"GIT_COMMITTER_EMAIL": Email,
		"JJ_CONFIG":           jjConfig,
		"XDG_CONFIG_HOME":     dir,
	} {
		t.Setenv(k, v)
	}
}
