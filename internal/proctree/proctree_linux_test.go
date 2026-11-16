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
// started after the cached value got a NEGATIVE elapsed. Later scans pin it: the
// clock has to advance, and never go backwards. Both readings are 10 ms
// granular, so it advances within a few polls; a cached uptime never does.
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
	before := map[int]float64{}
	for _, p := range flatten(first.Roots) {
		before[p.PID] = p.Elapsed
	}

	waitFor(t, "elapsed to advance (stale uptime?)", func() bool {
		checked, advanced := 0, 0
		for _, p := range flatten(Scan(runID).Roots) {
			was, ok := before[p.PID]
			if !ok {
				continue
			}
			checked++
			if p.Elapsed < was {
				t.Fatalf("pid %d elapsed went backwards: %.2f then %.2f", p.PID, was, p.Elapsed)
			}
			if p.Elapsed > was {
				advanced++
			}
		}
		if checked == 0 {
			t.Fatal("no process survived both scans; the test proved nothing")
		}
		return advanced == checked
	})
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

// midExecFor stands in readProcFile so that pid reads as a process mid-exec
// for the first n reads of its environ: the environ empty and the stat line's
// startcode 0. The cmdline reads empty too unless argsCopied, the later part
// of an exec, when the kernel has copied the arguments but not yet the
// environment. n < 0 keeps it mid-exec for good.
func midExecFor(t *testing.T, pid, n int, argsCopied bool) {
	t.Helper()
	var mu sync.Mutex
	reads := 0
	orig := readProcFile
	readProcFile = func(path string) ([]byte, error) {
		if filepath.Dir(path) != "/proc/"+strconv.Itoa(pid) {
			return orig(path)
		}
		name := filepath.Base(path)
		mu.Lock()
		if name == "environ" {
			reads++
		}
		mid := n < 0 || reads <= n
		mu.Unlock()
		switch {
		case !mid:
		case name == "environ", name == "cmdline" && !argsCopied:
			return []byte{}, nil
		case name == "stat":
			b, err := orig(path)
			if err != nil {
				return nil, err
			}
			return withStartCode(t, b, 0), nil
		}
		return orig(path)
	}
	t.Cleanup(func() { readProcFile = orig })
}

// withStartCode returns a /proc/<pid>/stat line with its startcode (field 26)
// replaced.
func withStartCode(t *testing.T, stat []byte, v uint64) []byte {
	t.Helper()
	i := strings.LastIndexByte(string(stat), ')')
	f := strings.Fields(string(stat[i+2:]))
	if len(f) < 24 {
		t.Fatalf("short stat line %q", stat)
	}
	f[23] = strconv.FormatUint(v, 10)
	return []byte(string(stat[:i+2]) + strings.Join(f, " ") + "\n")
}

// virtualClock runs readSettled's wait on virtual time for the test: a pause
// advances the clock instantly, so what a test with stand-in /proc reads checks
// is the wait's budget, not how promptly a loaded machine wakes the scan. It
// returns the time paused so far.
func virtualClock(t *testing.T) (paused func() time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var total time.Duration
	start := time.Now()
	orig := settleClock
	settleClock.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return start.Add(total)
	}
	settleClock.sleep = func(d time.Duration) {
		mu.Lock()
		total += d
		mu.Unlock()
	}
	t.Cleanup(func() { settleClock = orig })
	return func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return total
	}
}

// A process caught in the middle of an exec reads as having no environ for a
// moment: early on with no cmdline either, later with the new command's
// cmdline while the kernel copies the environment. The scan waits, so it still
// finds the process, with its command line.
func TestInspect_RetriesAProcessCaughtMidExec(t *testing.T) {
	runID := fmt.Sprintf("tstexec%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	for _, argsCopied := range []bool{false, true} {
		for _, n := range []int{1, 2, 3, 6} {
			t.Run(fmt.Sprintf("%d_empty_reads_args_copied_%v", n, argsCopied), func(t *testing.T) {
				virtualClock(t)
				midExecFor(t, pid, n, argsCopied)
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
}

// A busy machine can hold a process inside its exec for tens of milliseconds:
// many more empty reads than an idle one ever shows. The scan still waits it
// out.
func TestInspect_WaitsOutALongExec(t *testing.T) {
	runID := fmt.Sprintf("tstlong%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	// 20 empty reads, about 330 ms of pauses; the idle-machine window shows a
	// handful at most.
	paused := virtualClock(t)
	midExecFor(t, pid, 20, true)
	p := inspect(pid, os.Getuid(), runID, uptimeSeconds())
	if p == nil || !strings.Contains(p.Cmdline, "sleep 30") {
		t.Fatalf("a process held in its exec was missed or unnamed after %v of pauses: %+v", paused(), p)
	}
}

// A process with a genuinely empty environment has code (a non-zero startcode):
// it isn't mid-exec, so the scan doesn't wait on it.
func TestInspect_EmptyEnvironIsNotWaitedOn(t *testing.T) {
	runID := fmt.Sprintf("tstempty%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	paused := virtualClock(t)
	counts := readsOf(t, pid, map[string]int{"environ": 1 << 30})
	if p := inspect(pid, os.Getuid(), runID, uptimeSeconds()); p != nil {
		t.Fatalf("found a process whose environ never had the marker: %+v", p)
	}
	if d := paused(); d != 0 {
		t.Errorf("waited %v on a process that isn't mid-exec", d)
	}
	if counts["environ"] > 3 {
		t.Errorf("environ read %d times, want at most 3 (no waiting)", counts["environ"])
	}
}

// A process that keeps reading as mid-exec is given up on after the settle
// timeout.
func TestInspect_GivesUpAfterTheSettleTimeout(t *testing.T) {
	runID := fmt.Sprintf("tststuck%d", os.Getpid())
	pid := startMarked(t, runID, "worker", "exec sleep 30")
	waitSettled(t, runID)

	paused := virtualClock(t)
	midExecFor(t, pid, -1, false)
	if p := inspect(pid, os.Getuid(), runID, uptimeSeconds()); p != nil {
		t.Fatalf("found a process whose environ never had the marker: %+v", p)
	}
	if d := paused(); d < execSettleTimeout || d > execSettleTimeout+execMaxPause {
		t.Errorf("gave up after %v of pauses, want just past %v", d, execSettleTimeout)
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

// fakeProcDir is a process directory that doesn't exist, for fakeProc.
const fakeProcDir = "/proc/4242424242"

// fakeProc stands in readProcFile for fakeProcDir: its stat line carries flags
// and startCode, and its environ and cmdline read empty.
func fakeProc(t *testing.T, flags, startCode uint64) {
	t.Helper()
	dir := fakeProcDir
	orig := readProcFile
	readProcFile = func(path string) ([]byte, error) {
		if filepath.Dir(path) != dir {
			return orig(path)
		}
		switch filepath.Base(path) {
		case "stat":
			return []byte(fmt.Sprintf("4242 (kworker/0:1) I 2 0 0 0 -1 %d 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0 0 %d 0 0", flags, startCode)), nil
		case "environ", "cmdline":
			return []byte{}, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readProcFile = orig })
}

// A kernel thread reads empty in environ and cmdline for good (a scan running
// as root reads them), and has no code: it isn't mid-exec, so nothing waits on
// it. A user process without code is.
func TestMidExec_NotAKernelThread(t *testing.T) {
	dir := fakeProcDir
	fakeProc(t, pfKThread|0x8040, 0)
	if midExec(dir) {
		t.Error("a kernel thread read as mid-exec")
	}
	paused := virtualClock(t)
	if b := readSettled(dir, "environ"); len(b) != 0 {
		t.Errorf("environ = %q", b)
	}
	if d := paused(); d != 0 {
		t.Errorf("waited %v on a kernel thread", d)
	}

	fakeProc(t, 0x400000, 0)
	if !midExec(dir) {
		t.Error("a user process without code didn't read as mid-exec")
	}
	fakeProc(t, 0x400000, 0x55d0c0de0000)
	if midExec(dir) {
		t.Error("a user process with code read as mid-exec")
	}
	fakeProc(t, 0x400000, 1) // the kernel's placeholder when not ours to read
	if midExec(dir) {
		t.Error("a process not ours to read read as mid-exec")
	}
}

// statOf reads the task flags the kernel writes; kthreadd (pid 2) is a kernel
// thread wherever there is one.
func TestStatOf_KernelThreadFlag(t *testing.T) {
	comm, err := os.ReadFile("/proc/2/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "kthreadd" {
		t.Skip("no kthreadd at pid 2 (e.g. in a container)")
	}
	if st, ok := statOf("/proc/2"); !ok || st.flags&pfKThread == 0 || st.startCode != 0 {
		t.Errorf("kthreadd: %+v, ok %v; want PF_KTHREAD and no code", st, ok)
	}
	if st, ok := statOf("/proc/self"); !ok || st.flags&pfKThread != 0 || st.startCode == 0 {
		t.Errorf("this process: %+v, ok %v; want no PF_KTHREAD, and code", st, ok)
	}
}

// The real kernel, racing real execs: a scan started the moment a marked
// process begins its exec finds it, with its command line. A large
// environment stretches the part of the exec where the cmdline already reads
// the new command but the environ is still empty, so most rounds land there.
// It also checks what midExec relies on: once the stat line shows code, the
// environ is final.
func TestInspect_FindsAProcessStillInItsExec(t *testing.T) {
	runID := fmt.Sprintf("tstrace%d", os.Getpid())
	env := append(os.Environ(), config.EnvRunID+"="+runID)
	for i := range 20000 {
		env = append(env, fmt.Sprintf("AMPLIO_TEST_PAD%05d=x", i))
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	caught := 0
	for range 20 {
		cmd := exec.Command(sleep, "30")
		cmd.Env = env
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		pid := cmd.Process.Pid
		dir := "/proc/" + strconv.Itoa(pid)
		if b, _ := readProcFile(dir + "/environ"); len(b) == 0 {
			caught++
		}
		p := inspect(pid, os.Getuid(), runID, uptimeSeconds())
		if st, ok := statOf(dir); ok && st.startCode != 0 {
			if b, _ := readProcFile(dir + "/environ"); !strings.Contains(string(b), config.EnvRunID+"="+runID) {
				t.Errorf("pid %d has code but its environ lacks the marker (%d bytes)", pid, len(b))
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if p == nil || p.Cmdline != "sleep 30" && !strings.HasSuffix(p.Cmdline, "/sleep 30") {
			t.Fatalf("a process scanned during its exec was missed or unnamed: %+v", p)
		}
	}
	t.Logf("%d of 20 rounds read an empty environ right after the exec began", caught)
}
