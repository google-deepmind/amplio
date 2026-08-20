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

// Package proctree lists the OS processes an agent's bash calls left running.
//
// The bash tool starts every call with Setsid and exports AMPLIO_RUN_ID /
// AMPLIO_SESSION_ID into its environment (see internal/tool/bash). Children
// inherit that environment, so a process's OWN environment says which run and
// session it belongs to. That is the whole attribution mechanism: no registry to
// keep in sync, nothing to persist, and it survives an amplio restart or a lost
// database, because the marker lives inside the process rather than in our
// bookkeeping.
//
// What it cannot see: a descendant that replaces its environment (execve with a
// fresh env, e.g. `env -i`). Escaping the session or double-forking does NOT
// hide a process — only losing the marker does. Treat the result as best
// effort, not an audit.
//
// Cost is one directory read plus one environ read per same-uid process
// (~30ms for 1500 processes, of which ~200 are ours), then a stat per match.
// Scan only when someone is looking.
package proctree

import "time"

// Process is one live process attributed to a run.
//
// StartTime is the kernel's start time in clock ticks since boot. It exists so a
// caller can act on a process later — the pair (PID, StartTime) is stable where
// a bare PID can be recycled onto an unrelated process.
type Process struct {
	PID       int        `json:"pid"`
	PPID      int        `json:"ppid"`
	PGID      int        `json:"pgid"`
	SID       int        `json:"sid"`
	StartTime uint64     `json:"start_time"`
	State     string     `json:"state"` // R running, S sleeping, D uninterruptible, Z zombie, T stopped
	RSSBytes  int64      `json:"rss_bytes"`
	CPUMillis int64      `json:"cpu_millis"`
	Elapsed   float64    `json:"elapsed_seconds"`
	Cmdline   string     `json:"cmdline"`
	RunID     string     `json:"run_id"`
	SessionID string     `json:"session_id"`
	Orphan    bool       `json:"orphan"` // reparented: its launching shell is gone
	Children  []*Process `json:"children,omitempty"`
}

// Snapshot is one sample. Process state has no change events to subscribe to —
// the kernel does not notify us when a descendant forks or exits — so this is a
// sample by nature and carries the time it was taken.
type Snapshot struct {
	Supported  bool       `json:"supported"`
	Platform   string     `json:"platform"`
	TakenAt    time.Time  `json:"taken_at"`
	ScanMillis int64      `json:"scan_millis"`
	Total      int        `json:"total"`
	Roots      []*Process `json:"roots"`
}
