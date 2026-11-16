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
	"testing"
)

// TestMain is configtest.Main, which this package's own tests can't import.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "amplio-data-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = os.Setenv(EnvDataDir, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
