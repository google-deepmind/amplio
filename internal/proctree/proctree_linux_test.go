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

//go:build linux

package proctree

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"amplio/internal/config"
)

// startMarked launches a setsid'd shell carrying the run/session marker, the
// way the bash tool does, plus a child of its own. Returns the shell's pid.
func startMarked(t *testing.T, runID, sessionID, script string) int {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		config.EnvRunID+"="+runID,
		config.EnvSessionID+"="+sessionID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		// Kill the group, then reap, so a failing test leaves nothing behind.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return pid
}

// waitFor polls until cond holds, so the test tracks process startup instead of
// guessing a sleep long enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestScan_FindsMarkedTree(t *testing.T) {
	runID := fmt.Sprintf("tst%d", os.Getpid())
	shell := startMarked(t, runID, "worker-one", "sleep 30 & sleep 30")

	var snap Snapshot
	waitFor(t, "the shell and its child", func() bool {
		snap = Scan(runID)
		return snap.Total >= 3 // bash + two sleeps
	})

	if !snap.Supported {
		t.Fatal("Supported = false on linux")
	}
	// Every hit must carry the attribution, since that is the only reason we
	// consider a process ours at all.
	for _, p := range flatten(snap.Roots) {
		if p.RunID != runID {
			t.Errorf("pid %d has run_id %q, want %q", p.PID, p.RunID, runID)
		}
		if p.SessionID != "worker-one" {
			t.Errorf("pid %d has session %q, want worker-one", p.PID, p.SessionID)
		}
	}
	// The shell must be a root of the forest and its sleeps must hang off it,
	// not float as siblings: the tree shape is the point of the view.
	var root *Process
	for _, r := range snap.Roots {
		if r.PID == shell {
			root = r
		}
	}
	if root == nil {
		t.Fatalf("shell pid %d is not a root; roots=%v", shell, pidsOf(snap.Roots))
	}
	if len(root.Children) < 2 {
		t.Errorf("shell has %d children, want 2 (%v)", len(root.Children), pidsOf(root.Children))
	}
	if root.StartTime == 0 {
		t.Error("StartTime = 0; the (pid, starttime) identity pair needs it")
	}
	// Elapsed only has to be sane here, not positive: /proc/uptime and starttime
	// are both 10ms-granular, so a process younger than one tick measures exactly
	// 0. That it ADVANCES is TestScan_ElapsedTracksWallClock's job — asserting
	// "> 0" on a process born microseconds ago is a 56%-flaky coin toss.
	if root.State == "" || root.Elapsed < 0 {
		t.Errorf("state=%q elapsed=%v, want a state and a non-negative elapsed", root.State, root.Elapsed)
	}
}

// A different run's processes must not leak into this run's view — the filter
// is what makes the page per-run.
func TestScan_FiltersByRun(t *testing.T) {
	mine := fmt.Sprintf("mine%d", os.Getpid())
	theirs := fmt.Sprintf("thrs%d", os.Getpid())
	startMarked(t, mine, "s1", "sleep 30")
	startMarked(t, theirs, "s2", "sleep 30")

	var snap Snapshot
	waitFor(t, "both runs to appear", func() bool {
		return Scan(mine).Total > 0 && Scan(theirs).Total > 0
	})
	snap = Scan(mine)
	for _, p := range flatten(snap.Roots) {
		if p.RunID != mine {
			t.Errorf("run filter leaked pid %d from run %q", p.PID, p.RunID)
		}
	}
	if all := Scan(""); all.Total < snap.Total {
		t.Errorf("unfiltered scan (%d) smaller than filtered (%d)", all.Total, snap.Total)
	}
}

// An unmarked process must never be attributed to a run, however it is related
// to us: the marker is the only claim of ownership.
func TestScan_IgnoresUnmarked(t *testing.T) {
	runID := fmt.Sprintf("nm%d", os.Getpid())
	cmd := exec.Command("sleep", "30")
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no marker
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	for _, p := range flatten(Scan(runID).Roots) {
		if p.PID == cmd.Process.Pid {
			t.Errorf("unmarked pid %d was attributed to run %s", p.PID, runID)
		}
	}
}

func flatten(ps []*Process) []*Process {
	var out []*Process
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatten(p.Children)...)
	}
	return out
}

func pidsOf(ps []*Process) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

// Elapsed is computed against /proc/uptime. Caching that reading for the life of
// the server (rather than per scan) made every later snapshot wrong — a process
// started after the cached value got a NEGATIVE elapsed. Two scans a moment
// apart pin it: the clock has to advance, and never go backwards.
func TestScan_ElapsedTracksWallClock(t *testing.T) {
	runID := fmt.Sprintf("el%d", os.Getpid())
	startMarked(t, runID, "clock", "sleep 30")

	var first Snapshot
	waitFor(t, "the shell", func() bool {
		first = Scan(runID)
		return first.Total > 0
	})
	for _, p := range flatten(first.Roots) {
		if p.Elapsed < 0 {
			t.Fatalf("pid %d elapsed = %v, want >= 0", p.PID, p.Elapsed)
		}
	}
	time.Sleep(1200 * time.Millisecond)

	second := Scan(runID)
	before := map[int]float64{}
	for _, p := range flatten(first.Roots) {
		before[p.PID] = p.Elapsed
	}
	checked := 0
	for _, p := range flatten(second.Roots) {
		was, ok := before[p.PID]
		if !ok {
			continue
		}
		checked++
		if p.Elapsed <= was {
			t.Errorf("pid %d elapsed did not advance: %.2f then %.2f (stale uptime?)", p.PID, was, p.Elapsed)
		}
	}
	if checked == 0 {
		t.Fatal("no process survived both scans; the test proved nothing")
	}
}
