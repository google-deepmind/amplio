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

package proctree

import (
	"strings"
	"testing"
)

func TestJobsOf(t *testing.T) {
	// One tree with two sessions in it: worker's job is the shell plus its own
	// child; a sub-agent's process started under it is helper's job.
	child := &Process{PID: 3, SessionID: "worker"}
	helperLeaf := &Process{PID: 5, SessionID: "helper"}
	helper := &Process{PID: 4, SessionID: "helper", Children: []*Process{helperLeaf}}
	shell := &Process{PID: 2, PGID: 2, SessionID: "worker", Children: []*Process{child, helper}}
	other := &Process{PID: 9, PGID: 9, SessionID: "worker"}
	roots := []*Process{shell, other}

	jobs := JobsOf(roots, "worker")
	if len(jobs) != 2 || jobs[0].PID != 2 || jobs[0].Descendants != 1 || jobs[1].PID != 9 || jobs[1].Descendants != 0 {
		t.Errorf("worker jobs = %+v, want pid 2 (+1) and pid 9", jobs)
	}
	jobs = JobsOf(roots, "helper")
	if len(jobs) != 1 || jobs[0].PID != 4 || jobs[0].Descendants != 1 {
		t.Errorf("helper jobs = %+v, want pid 4 (+1)", jobs)
	}
	if jobs := JobsOf(roots, "nobody"); len(jobs) != 0 {
		t.Errorf("unknown session has jobs %+v", jobs)
	}
}

func TestJobDescribe(t *testing.T) {
	for _, c := range []struct {
		job  Job
		want string
	}{
		{Job{Process: &Process{PID: 7, PGID: 48213, Elapsed: 2834, Cmdline: "./bench.sh  --iters 50"}, Descendants: 1},
			"process group 48213, 47 min: ./bench.sh --iters 50 (+1 child process)"},
		{Job{Process: &Process{PID: 7, PGID: 7, Elapsed: 7830, Cmdline: "bash -c while kill -0 1;\ndo sleep 15; done"}, Descendants: 3},
			"process group 7, 2 h 10 min: bash -c while kill -0 1; do sleep 15; done (+3 child processes)"},
		{Job{Process: &Process{PID: 8, PGID: 8, Elapsed: 12}}, "process group 8, 12 s: pid 8"},
	} {
		if got := c.job.Describe(); got != c.want {
			t.Errorf("Describe() = %q, want %q", got, c.want)
		}
	}
	long := Job{Process: &Process{PGID: 1, Cmdline: strings.Repeat("x", 500)}}
	if got := long.Describe(); len([]rune(got)) > maxCmdline+40 || !strings.HasSuffix(got, "…") {
		t.Errorf("long command not truncated: %d runes", len([]rune(got)))
	}
}
