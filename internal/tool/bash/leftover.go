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

package bash

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amplio/internal/config"
	"amplio/internal/proctree"
)

// What a bash call left running.
//
// Every call writes stdout and stderr to two files, and runs in its own
// process group with AMPLIO_BASH_CALL=<call id> in its environment. When the
// call returns, the tool looks at the process table, not at the command text:
// processes still carrying the call's marker are what it left running (found
// even if they left the group with setsid). Any of them that still has one of
// the call's output files open is writing its output there, so that file is
// kept, under the run's artifact dir, instead of being deleted. The result then
// says what is still running, where its output goes, and how to check or stop
// it. It reports everything, including a server kept up on purpose: the tool
// can't tell those from forgotten jobs, so it states the facts and the agent
// decides.

// scanProcs is the run's process table; a var so tests can stand one in.
var scanProcs = proctree.Scan

// maxNoteJobs bounds the jobs a BACKGROUND note lists; the rest are counted.
const maxNoteJobs = 10

// callFiles are one call's stdout and stderr files.
type callFiles struct {
	id       string // unique per call: the files' base name, and AMPLIO_BASH_CALL
	out, err *os.File
}

// callDir is where a call's output files live: the run's artifact dir, so a
// file a background process keeps writing to survives the call and is
// readable; "" (the system temp dir) without a run.
func callDir(env []string) string {
	if envLookup(env, config.EnvRunID) == "" {
		return ""
	}
	if dir := envLookup(env, config.EnvArtifactDir); dir != "" {
		return filepath.Join(dir, "bg")
	}
	return ""
}

func newCallFiles(dir, label string) (*callFiles, error) {
	if label == "" {
		label = "call"
	}
	if dir == "" || os.MkdirAll(dir, 0o755) != nil {
		dir, label = os.TempDir(), "amplio-bash"
	}
	out, err := os.CreateTemp(dir, label+"-*.out")
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(out.Name(), ".out")
	errF, err := os.OpenFile(base+".err", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		out.Close()
		os.Remove(out.Name())
		return nil, err
	}
	return &callFiles{id: filepath.Base(base), out: out, err: errF}, nil
}

// close closes both files and deletes the ones nothing is still writing to.
func (f *callFiles) close(keepOut, keepErr bool) {
	f.out.Close()
	f.err.Close()
	if !keepOut {
		os.Remove(f.out.Name())
	}
	if !keepErr {
		os.Remove(f.err.Name())
	}
}

type leftovers struct {
	known            bool           // per-process facts available (Linux)
	jobs             []proctree.Job // what the call left running, when known
	pgid             int            // the call's own process group
	groupAlive       bool           // without per-process facts: the group still has members
	keepOut, keepErr bool           // a leftover still writes to that file
	outPath, errPath string
}

func findLeftovers(runID string, f *callFiles, pgid int) leftovers {
	l := leftovers{pgid: pgid, outPath: absPath(f.out.Name()), errPath: absPath(f.err.Name())}
	snap := scanProcs(runID)
	if snap.Supported {
		l.known = true
		mine := func(p *proctree.Process) bool { return p.Call == f.id }
		l.jobs = proctree.JobsWhere(snap.Roots, mine)
		proctree.Walk(snap.Roots, func(p *proctree.Process) {
			if !mine(p) {
				return
			}
			o, e := proctree.Outputs(p.PID)
			l.keepOut = l.keepOut || o == l.outPath || e == l.outPath
			l.keepErr = l.keepErr || o == l.errPath || e == l.errPath
		})
		return l
	}
	// No per-process facts: the call's own group is the only signal, and what is
	// left in it may be writing to either file, so keep both.
	if groupAlive(pgid) {
		l.groupAlive, l.keepOut, l.keepErr = true, true, true
	}
	return l
}

// note is the BACKGROUND section of the result, or "" when nothing is left.
func (l leftovers) note() string {
	var b strings.Builder
	var groups []int
	switch {
	case l.known && len(l.jobs) > 0:
		b.WriteString("Still running after this call returned:\n")
		seen := map[int]bool{}
		for i, j := range l.jobs {
			if i == maxNoteJobs {
				fmt.Fprintf(&b, "- and %d more\n", len(l.jobs)-i)
				break
			}
			fmt.Fprintf(&b, "- process group %d: %s\n", j.PGID, j.Summary())
			if !seen[j.PGID] {
				seen[j.PGID] = true
				groups = append(groups, j.PGID)
			}
		}
	case !l.known && l.groupAlive:
		fmt.Fprintf(&b, "Something this call started is still running (process group %d).\n", l.pgid)
		groups = []int{l.pgid}
	default:
		return ""
	}
	switch {
	case l.keepOut && l.keepErr:
		fmt.Fprintf(&b, "Its output goes to %s (stdout) and %s (stderr).\n", l.outPath, l.errPath)
	case l.keepOut:
		fmt.Fprintf(&b, "Its output goes to %s.\n", l.outPath)
	case l.keepErr:
		fmt.Fprintf(&b, "Its error output goes to %s.\n", l.errPath)
	}
	if len(groups) == 1 {
		fmt.Fprintf(&b, "Check with: kill -0 -- -%d · stop with: kill -- -%d", groups[0], groups[0])
	} else {
		b.WriteString("Check or stop each with: kill -0 -- -<process group> / kill -- -<process group>")
	}
	return b.String()
}

// absPath is p as /proc reports it in a process's fd links: absolute, with
// symlinks resolved (the kernel reports the real path). Compared unresolved, a
// symlink anywhere in the data dir's path would make a file still being
// written look unused, and it would be deleted.
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// envLookup returns KEY's value in a KEY=value list, "" if absent.
func envLookup(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}
