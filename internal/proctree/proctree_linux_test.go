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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// An orphan is a root whose parent is not amplio: init adopts orphans on some
// hosts, but a subreaper (systemd --user under a systemd unit, a container's
// init) does on others, and "parent is 1" missed those.
func TestForest_OrphanMeansParentIsNotAmplio(t *testing.T) {
	const self = 100
	byPID := map[int]*Process{
		10: {PID: 10, PPID: self}, // the shell of a bash call still running
		11: {PID: 11, PPID: 1},    // outlived its call; adopted by init
		12: {PID: 12, PPID: 4242}, // outlived its call; adopted by a subreaper
		13: {PID: 13, PPID: 10},   // a child inside the matched set: not a root
	}
	roots := forest(byPID, self)
	got := map[int]bool{}
	for _, r := range roots {
		got[r.PID] = r.Orphan
	}
	want := map[int]bool{10: false, 11: true, 12: true}
	if len(got) != len(want) {
		t.Fatalf("roots = %v, want pids 10, 11, 12", got)
	}
	for pid, orphan := range want {
		if got[pid] != orphan {
			t.Errorf("pid %d: orphan = %v, want %v", pid, got[pid], orphan)
		}
	}
}

// readsOf stands in readProcFile for the test, counting reads of path's base
// name per pid and returning empty for the first `empty` of each listed file.
func readsOf(t *testing.T, pid int, empty map[string]int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	var mu sync.Mutex
	orig := readProcFile
	readProcFile = func(path string) ([]byte, error) {
		if filepath.Dir(path) == "/proc/"+strconv.Itoa(pid) {
			name := filepath.Base(path)
			mu.Lock()
			counts[name]++
			n := counts[name]
			mu.Unlock()
			if n <= empty[name] {
				return []byte{}, nil
			}
		}
		return orig(path)
	}
	t.Cleanup(func() { readProcFile = orig })
	return counts
}

// waitSettled waits until the run's one marked process, started as `bash -c
// "exec sleep 30"`, runs sleep. A scan can find it first as bash, before that
// exec; a test that stubs /proc reads afterwards must not race the real exec
// (which reads as mid-exec while it lasts).
func waitSettled(t *testing.T, runID string) {
	t.Helper()
	waitFor(t, "the marked process to run sleep 30", func() bool {
		roots := Scan(runID).Roots
		return len(roots) == 1 && roots[0].Cmdline == "sleep 30"
	})
}

// midExecFor stands in readProcFile so that pid reads as a process mid-exec,
// its environ and its cmdline both empty, for the first n reads of either; the
// kernel sets both at the same moment, so they settle together.
func midExecFor(t *testing.T, pid, n int) {
	t.Helper()
	var mu sync.Mutex
	reads := 0
	orig := readProcFile
	readProcFile = func(path string) ([]byte, error) {
		if filepath.Dir(path) == "/proc/"+strconv.Itoa(pid) {
			if name := filepath.Base(path); name == "environ" || name == "cmdline" {
				mu.Lock()
				reads++
				empty := reads <= n
				mu.Unlock()
				if empty {
					return []byte{}, nil
				}
			}
		}
		return orig(path)
	}
	t.Cleanup(func() { readProcFile = orig })
}

// A process caught in the middle of an exec reads as having no environ and no
// cmdline for a moment. The scan waits, so it still finds the process, with
// its command line.
func TestInspect_RetriesAProcessCaughtMidExec(t *testing.T) {
	runID := fmt.Sprintf("tstexec%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	// 1 and 2: the exec settles while the scan checks whether it is mid-exec.
	for _, n := range []int{1, 2, 3, 6} {
		t.Run(fmt.Sprintf("%d_empty_reads", n), func(t *testing.T) {
			midExecFor(t, pid, n)
			p := inspect(pid, os.Getuid(), runID, uptimeSeconds())
			if p == nil {
				t.Fatal("a process caught mid-exec was missed")
			}
			if !strings.Contains(p.Cmdline, "sleep 30") {
				t.Errorf("cmdline = %q, want the command once the exec settles", p.Cmdline)
			}
		})
	}
}

// A busy machine can hold a process inside its exec for tens of milliseconds:
// many more empty reads than an idle one ever shows. The scan still waits it
// out.
func TestInspect_WaitsOutALongExec(t *testing.T) {
	runID := fmt.Sprintf("tstlong%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	midExecFor(t, pid, 60) // the idle-machine window shows a handful at most
	start := time.Now()
	p := inspect(pid, os.Getuid(), runID, uptimeSeconds())
	if p == nil || !strings.Contains(p.Cmdline, "sleep 30") {
		t.Fatalf("a process held in its exec was missed or unnamed: %+v", p)
	}
	if d := time.Since(start); d > execSettleTimeout {
		t.Errorf("took %v, more than the settle timeout %v", d, execSettleTimeout)
	}
}

// A process with a genuinely empty environment still has a cmdline: it isn't
// mid-exec, so the scan doesn't wait on it.
func TestInspect_EmptyEnvironIsNotWaitedOn(t *testing.T) {
	runID := fmt.Sprintf("tstempty%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	counts := readsOf(t, pid, map[string]int{"environ": 1 << 30})
	start := time.Now()
	if p := inspect(pid, os.Getuid(), runID, uptimeSeconds()); p != nil {
		t.Fatalf("found a process whose environ never had the marker: %+v", p)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("waited %v on a process that isn't mid-exec", d)
	}
	if counts["environ"] > 3 {
		t.Errorf("environ read %d times, want at most 3 (no waiting)", counts["environ"])
	}
}

// A process that keeps reading empty in both is given up on after the settle
// timeout.
func TestInspect_GivesUpAfterTheSettleTimeout(t *testing.T) {
	runID := fmt.Sprintf("tststuck%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	orig := execSettleTimeout
	execSettleTimeout = 60 * time.Millisecond
	t.Cleanup(func() { execSettleTimeout = orig })
	readsOf(t, pid, map[string]int{"environ": 1 << 30, "cmdline": 1 << 30})
	start := time.Now()
	if p := inspect(pid, os.Getuid(), runID, uptimeSeconds()); p != nil {
		t.Fatalf("found a process whose environ never had the marker: %+v", p)
	}
	if d := time.Since(start); d < execSettleTimeout || d > execSettleTimeout+time.Second {
		t.Errorf("gave up after %v, want just past %v", d, execSettleTimeout)
	}
}

// A zombie's cmdline is empty for good: no retries. (Its environ can't be read
// at all, so inspect already drops it there.)
func TestInspect_DoesNotRetryAZombie(t *testing.T) {
	runID := fmt.Sprintf("tstzombie%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exit 0") // unreaped until cleanup
	dir := "/proc/" + strconv.Itoa(pid)
	waitFor(t, "the process to exit", func() bool { return zombie(dir) })

	counts := readsOf(t, pid, nil)
	if p := inspect(pid, os.Getuid(), runID, uptimeSeconds()); p != nil {
		t.Fatalf("found a zombie: %+v", p)
	}
	if got := readCmdline(dir); got != "" {
		t.Errorf("zombie cmdline = %q, want empty", got)
	}
	if counts["environ"] != 1 || counts["cmdline"] > 2 {
		t.Errorf("reads = %v, want one of environ and at most two of cmdline (no waiting on a zombie)", counts)
	}
}

// fakeProc stands in readProcFile for a process directory that doesn't exist:
// its stat line carries flags, and its environ and cmdline read empty.
func fakeProc(t *testing.T, dir string, flags uint64) {
	t.Helper()
	orig := readProcFile
	readProcFile = func(path string) ([]byte, error) {
		if filepath.Dir(path) != dir {
			return orig(path)
		}
		switch filepath.Base(path) {
		case "stat":
			return []byte(fmt.Sprintf("4242 (kworker/0:1) I 2 0 0 0 -1 %d 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", flags)), nil
		case "environ", "cmdline":
			return []byte{}, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readProcFile = orig })
}

// A kernel thread reads empty in environ and cmdline for good (a scan running
// as root reads them): it isn't mid-exec, so nothing waits on it. A user
// process in the same state is.
func TestMidExec_NotAKernelThread(t *testing.T) {
	dir := "/proc/4242424242"
	fakeProc(t, dir, pfKThread|0x8040)
	if midExec(dir) {
		t.Error("a kernel thread read as mid-exec")
	}
	start := time.Now()
	if b := readSettled(dir, "environ"); len(b) != 0 {
		t.Errorf("environ = %q", b)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("waited %v on a kernel thread", d)
	}

	fakeProc(t, dir, 0x400000)
	if !midExec(dir) {
		t.Error("a user process reading empty in both didn't read as mid-exec")
	}
}

// statOf reads the task flags the kernel writes; kthreadd (pid 2) is a kernel
// thread wherever there is one.
func TestStatOf_KernelThreadFlag(t *testing.T) {
	comm, err := os.ReadFile("/proc/2/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "kthreadd" {
		t.Skip("no kthreadd at pid 2 (e.g. in a container)")
	}
	if _, flags, ok := statOf("/proc/2"); !ok || flags&pfKThread == 0 {
		t.Errorf("kthreadd: flags %#x, ok %v; want PF_KTHREAD", flags, ok)
	}
	if _, flags, ok := statOf("/proc/self"); !ok || flags&pfKThread != 0 {
		t.Errorf("this process: flags %#x, ok %v; want no PF_KTHREAD", flags, ok)
	}
}
