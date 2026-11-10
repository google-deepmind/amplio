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

import (
	"fmt"
	"strings"
	"time"
)

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
	Call      string     `json:"call,omitempty"` // the bash call that started it (AMPLIO_BASH_CALL)
	Orphan    bool       `json:"orphan"`         // reparented: its launching shell is gone
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

// Job is a process tree one session left running: the top-most process of that
// session in its tree, plus how many of its descendants belong to the session.
type Job struct {
	*Process
	Descendants int
}

// JobsOf returns sessionID's jobs in a scanned forest: each process of that
// session whose parent is not, in start order. A job's own process group is
// what stops it (kill -- -PGID).
func JobsOf(roots []*Process, sessionID string) []Job {
	return JobsWhere(roots, func(p *Process) bool { return p.SessionID == sessionID })
}

// JobsWhere is JobsOf for any grouping: the processes matching ours whose
// parent doesn't, each with its count of matching descendants.
func JobsWhere(roots []*Process, ours func(*Process) bool) []Job {
	var jobs []Job
	var walk func(ps []*Process, parentOurs bool)
	walk = func(ps []*Process, parentOurs bool) {
		for _, p := range ps {
			mine := ours(p)
			if mine && !parentOurs {
				jobs = append(jobs, Job{Process: p, Descendants: countWhere(p.Children, ours)})
			}
			walk(p.Children, mine)
		}
	}
	walk(roots, false)
	return jobs
}

func countWhere(ps []*Process, ours func(*Process) bool) int {
	n := 0
	for _, p := range ps {
		if ours(p) {
			n++
		}
		n += countWhere(p.Children, ours)
	}
	return n
}

// Walk calls fn for every process in a forest.
func Walk(roots []*Process, fn func(*Process)) {
	for _, p := range roots {
		fn(p)
		Walk(p.Children, fn)
	}
}

// maxCmdline bounds a job's command in Describe: enough to recognise it, short
// enough that a dozen jobs stay readable in a model's context.
const maxCmdline = 120

// Describe renders a job as one line for an agent, e.g.
// "process group 48213, 47 min: ./bench.sh --iters 50 (+1 child process)".
func (j Job) Describe() string {
	return fmt.Sprintf("process group %d, %s: %s", j.PGID, formatAge(j.Elapsed), j.Summary())
}

// Summary is Describe without the process group and age: the command and its
// child count, for a job that has only just started.
func (j Job) Summary() string {
	cmd := strings.Join(strings.Fields(j.Cmdline), " ")
	if cmd == "" {
		cmd = fmt.Sprintf("pid %d", j.PID)
	}
	if r := []rune(cmd); len(r) > maxCmdline {
		cmd = string(r[:maxCmdline-1]) + "…"
	}
	switch {
	case j.Descendants == 1:
		cmd += " (+1 child process)"
	case j.Descendants > 1:
		cmd += fmt.Sprintf(" (+%d child processes)", j.Descendants)
	}
	return cmd
}

// formatAge renders seconds coarsely: a model needs "47 min", not "2834.2s".
func formatAge(sec float64) string {
	s := int(sec)
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min", s/60)
	default:
		return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
	}
}
