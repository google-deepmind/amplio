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

//go:build !linux

package proctree

import (
	"runtime"
	"time"
)

// Scan reports the feature as unsupported off Linux: attribution reads each
// process's own environment out of /proc, which only Linux has. The UI drops
// the entry rather than showing an empty page.
//
// A reduced version is possible on macOS — sysctl KERN_PROC_ALL gives pid, ppid
// and pgid — but not the environment of another process, which is what carries
// the run and session. That would cover live calls and lose exactly the two
// cases the marker buys: orphans found after a restart, and anything that left
// its session.
func Scan(string) Snapshot {
	return Snapshot{Supported: false, Platform: runtime.GOOS, TakenAt: time.Now(), Roots: []*Process{}}
}
