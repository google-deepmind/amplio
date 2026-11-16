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

// Package configtest keeps a package's tests out of the data dir of the shell
// that runs them, which config.DataDir refuses under `go test`.
package configtest

import (
	"fmt"
	"os"
	"testing"

	"amplio/internal/config"
)

// Main runs m with $AMPLIO_DATA_DIR pointing at a fresh temp dir, removed
// afterwards. Use it as the TestMain of a package whose tests resolve the data
// dir:
//
//	func TestMain(m *testing.M) { configtest.Main(m) }
//
// The env var, not config.SetDataDir, so that tests may still pin and unpin
// their own (SetDataDir(dir) … SetDataDir("")) and subprocesses inherit it.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "amplio-data-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "configtest:", err)
		return 2
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort, in the temp dir
	if err := os.Setenv(config.EnvDataDir, dir); err != nil {
		fmt.Fprintln(os.Stderr, "configtest:", err)
		return 2
	}
	return m.Run()
}
