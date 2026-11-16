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

package config

import (
	"fmt"
	"os"
	"runtime/debug"
	"testing"
)

// liveDataDirs are, in a test binary, the data dirs of the shell running it:
// its $AMPLIO_DATA_DIR and its ~/.amplio. On a developer's machine or inside an
// agent's shell that is a live data dir. Nil outside tests.
var liveDataDirs = inheritedDataDirs()

func inheritedDataDirs() []string {
	if !testing.Testing() {
		return nil
	}
	dirs := []string{defaultDataDir()}
	if dir := os.Getenv(EnvDataDir); dir != "" {
		dirs = append(dirs, expandTilde(dir))
	}
	return dirs
}

// refuseLiveDataDir ends a test binary that resolves a live data dir, where it
// would leave run artifacts and logs among the real ones and depend on the
// developer's shims and config. Tests use a temp data dir instead:
// configtest.Main as TestMain, config.SetDataDir or t.Setenv(EnvDataDir, …).
// It exits rather than panics because net/http recovers a handler's panic, and
// handlers are where most of the server's resolutions happen.
func refuseLiveDataDir(dir string) {
	for _, live := range liveDataDirs {
		if dir == live {
			fmt.Fprintf(os.Stderr, "config: a test resolved the live data dir %s; "+
				"use a temp one (configtest.Main as the package's TestMain, SetDataDir or t.Setenv(%s, …))\n%s",
				dir, EnvDataDir, debug.Stack())
			os.Exit(2)
		}
	}
}
