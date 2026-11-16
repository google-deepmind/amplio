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

// The build tag is `linux`, not `unix`: macOS compiles as unix but has no
// /proc, so sharing the tag would give a scanner that silently finds nothing.

package proctree

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"amplio/internal/config"
)

// scanWorkers bounds the parallel /proc readers. The work is syscall-bound, not
// CPU-bound: 8 workers cut a full pass from ~83ms to ~28ms on a 1500-process
// machine, and more than that buys latency back only at rising CPU cost.
const scanWorkers = 8

// A process in the middle of execve reads as having no environ: the kernel
// installs the new address space, then copies the arguments into it and then
// the environment, so for a while the cmdline already reads the new command
// while the environ is still empty (most of the exec, in fact, since the
// environment is the longer part). The window is short on an idle machine (a
// median of about 0.6 ms and at most 1.4 ms for /bin/sleep), but a busy one can
// deschedule the process inside it for tens of milliseconds, and a scan that
// lands there would miss a marked process entirely, or name it "pid N". So a
// scan waits, with growing pauses up to execSettleTimeout, while a live
// process is mid-exec (see midExec). A zombie's environ stays empty, and so do
// a kernel thread's (it has no user address space; a scan running as root
// reads them) and a process's with a genuinely empty environment; none of
// those is waited on.
var execSettleTimeout = 500 * time.Millisecond

// execMaxPause caps one pause of that wait.
const execMaxPause = 20 * time.Millisecond

// readProcFile reads a /proc file; a var so tests can stand in a process
// caught mid-exec.
var readProcFile = readWhole

// settleClock times readSettled's wait; a var so tests that stand in /proc
// reads can run the wait on virtual time.
var settleClock = struct {
	now   func() time.Time
	sleep func(time.Duration)
}{time.Now, time.Sleep}

var clockTick = int64(100) // _SC_CLK_TCK; 100 on every Linux port Go supports

// Scan samples the live processes carrying an AMPLIO_RUN_ID marker. A non-empty
// runID keeps only that run's; empty returns every run's, which is the
// "what is this machine still running" view.
func Scan(runID string) Snapshot {
	start := time.Now()
	snap := Snapshot{Supported: true, Platform: runtime.GOOS, Roots: []*Process{}}

	pids := readPIDs()
	uid := os.Getuid()
	uptime := uptimeSeconds()
	found := make(chan *Process, len(pids))
	var wg sync.WaitGroup
	work := make(chan int, len(pids))
	for _, p := range pids {
		work <- p
	}
	close(work)
	for i := 0; i < scanWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pid := range work {
				if p := inspect(pid, uid, runID, uptime); p != nil {
					found <- p
				}
			}
		}()
	}
	wg.Wait()
	close(found)

	byPID := map[int]*Process{}
	for p := range found {
		byPID[p.PID] = p
	}
	snap.Total = len(byPID)
	snap.Roots = forest(byPID, os.Getpid())
	snap.TakenAt = time.Now()
	snap.ScanMillis = time.Since(start).Milliseconds()
	return snap
}

// inspect returns the process if it carries our marker, nil otherwise. Reading
// another user's environ fails with EACCES, so skip foreign processes before
// paying for the open: on a shared workstation that is most of them.
func inspect(pid, uid int, runID string, uptime float64) *Process {
	dir := "/proc/" + strconv.Itoa(pid)
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil || int(st.Uid) != uid {
		return nil
	}
	env := readSettled(dir, "environ")
	if len(env) == 0 {
		return nil
	}
	run := envValue(env, config.EnvRunID)
	if run == "" || (runID != "" && run != runID) {
		return nil
	}
	p := &Process{PID: pid, RunID: run, SessionID: envValue(env, config.EnvSessionID), Call: envValue(env, config.EnvBashCall)}
	if !readStat(dir, p, uptime) {
		return nil // exited between the readdir and here; not an error
	}
	p.Cmdline = readCmdline(dir)
	return p
}

// Outputs reports where a live process's stdout and stderr point (the targets
// of /proc/<pid>/fd/1 and fd/2), "" where unreadable.
func Outputs(pid int) (stdout, stderr string) {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd/"
	stdout, _ = os.Readlink(dir + "1")
	stderr, _ = os.Readlink(dir + "2")
	return stdout, stderr
}

// envValue pulls one NUL-separated KEY=value out of a /proc environ blob.
// bash, as it imports its environment at startup, splits each entry in place:
// it writes a NUL over the '=' and puts the '=' back once it has the entry. A
// scan that reads a bash starting up (a script just put in the background,
// say) can see KEY and value as two entries, so a bare KEY takes the entry
// after it.
func envValue(env []byte, key string) string {
	want := append([]byte(key), '=')
	kvs := bytes.Split(env, []byte{0})
	for i, kv := range kvs {
		if bytes.HasPrefix(kv, want) {
			return string(kv[len(want):])
		}
		if string(kv) == key && i+1 < len(kvs) {
			return string(kvs[i+1])
		}
	}
	return ""
}

// readStat fills the /proc/<pid>/stat fields. comm is parenthesised and may
// itself contain spaces and parentheses, so the fixed fields are counted from
// the LAST ')' rather than by splitting the whole line.
func readStat(dir string, p *Process, uptime float64) bool {
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	f := bytes.Fields(b[i+2:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return false
	}
	num := func(n int) int64 { v, _ := strconv.ParseInt(string(f[n]), 10, 64); return v }
	p.State = string(f[0])
	p.PPID = int(num(1))
	p.PGID = int(num(2))
	p.SID = int(num(3))
	p.CPUMillis = (num(11) + num(12)) * 1000 / clockTick // utime + stime
	p.StartTime, _ = strconv.ParseUint(string(f[19]), 10, 64)
	p.RSSBytes = num(21) * int64(os.Getpagesize())
	if uptime > 0 {
		p.Elapsed = max(0, uptime-float64(p.StartTime)/float64(clockTick))
	}
	return true
}

// readSettled reads a /proc file of a process, waiting out an empty read while
// the process is in the middle of an exec (see execSettleTimeout). nil if the
// process is gone, or the file stays empty.
func readSettled(dir, name string) []byte {
	path := filepath.Join(dir, name)
	deadline := settleClock.now().Add(execSettleTimeout)
	pause := time.Millisecond
	for {
		b, err := readProcFile(path)
		if err != nil {
			return nil // gone, or not ours to read
		}
		if len(b) > 0 || settleClock.now().After(deadline) {
			return b
		}
		if !midExec(dir) {
			// Empty for good, or the exec settled since the read: read once more.
			if b, err = readProcFile(path); err != nil {
				return nil
			}
			return b
		}
		settleClock.sleep(pause)
		pause = min(2*pause, execMaxPause)
	}
}

// midExec reports whether a process is in the middle of an exec: it is alive,
// not a kernel thread, and its address space has no code yet. The kernel sets
// the code range (startcode in /proc/<pid>/stat) of the new address space only
// after it has copied both the arguments and the environment into it, so a
// zero there, read before the environ or cmdline, means those aren't final
// yet; once a process runs any program it is non-zero. A process whose
// environment is genuinely empty has code, so it isn't waited on.
func midExec(dir string) bool {
	st, ok := statOf(dir)
	return ok && st.state != 'Z' && st.state != 'X' && st.flags&pfKThread == 0 && st.startCode == 0
}

// zombie reports whether the process has exited and awaits reaping (or is
// unreadable): its environ and cmdline are empty for good.
func zombie(dir string) bool {
	st, ok := statOf(dir)
	return !ok || st.state == 'Z' || st.state == 'X'
}

// pfKThread is the kernel's PF_KTHREAD task flag, set on kernel threads.
const pfKThread = 0x00200000

// procStat is what midExec and zombie need from /proc/<pid>/stat.
type procStat struct {
	state     byte   // field 3
	flags     uint64 // field 9, the task flags
	startCode uint64 // field 26: 0 without code (mid-exec, a kernel thread, exited); 1 when not ours to read
}

// statOf reads a process's procStat; ok is false if the file can't be read or
// parsed.
func statOf(dir string) (st procStat, ok bool) {
	b, err := readProcFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procStat{}, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return procStat{}, false
	}
	f := bytes.Fields(b[i+2:]) // f[0] is field 3 (state)
	if len(f) < 24 {
		return procStat{}, false
	}
	st.state = f[0][0]
	if st.flags, err = strconv.ParseUint(string(f[6]), 10, 64); err != nil {
		return procStat{}, false
	}
	if st.startCode, err = strconv.ParseUint(string(f[23]), 10, 64); err != nil {
		return procStat{}, false
	}
	return st, true
}

// readCmdline returns the argv with NULs turned into spaces. Empty for a kernel
// thread or a zombie, where the caller shows comm-less rows rather than lying.
func readCmdline(dir string) string {
	b := readSettled(dir, "cmdline")
	if len(b) == 0 {
		return ""
	}
	b = bytes.TrimRight(b, "\x00")
	return string(bytes.ReplaceAll(b, []byte{0}, []byte(" ")))
}

// uptimeSeconds is sampled once per SCAN and threaded through: every row in one
// snapshot must share a clock (or siblings disagree), but caching it for the
// life of the server makes every later scan wrong — a process started after the
// cached reading gets a NEGATIVE elapsed, which is how this was found.
func uptimeSeconds() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	if f := bytes.Fields(b); len(f) > 0 {
		v, _ := strconv.ParseFloat(string(f[0]), 64)
		return v
	}
	return 0
}

func readPIDs() []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(ents))
	for _, e := range ents {
		// Only numeric top-level entries are processes. Threads live under
		// /proc/<pid>/task/<tid> and never appear here, so "processes, not
		// threads" needs no filtering of its own.
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// forest links each process to its parent WITHIN the matched set. A process
// whose parent is missing is a root: either the parent is amplio itself (self:
// the bash tool's shell for a call still running), or its launching shell has
// exited and it was reparented — the interesting case, an orphan that outlived
// its call. Who adopts an orphan depends on the host: init, but also the nearest
// subreaper, e.g. `systemd --user` when amplio runs under a systemd unit, or a
// container's init. So "orphan" is "parent is not amplio", not "parent is 1".
func forest(byPID map[int]*Process, self int) []*Process {
	// Non-nil even when empty: the JSON must be [] rather than null, so the UI
	// renders "nothing running" instead of special-casing a missing list.
	roots := make([]*Process, 0, len(byPID))
	for _, p := range byPID {
		if parent, ok := byPID[p.PPID]; ok && parent != p {
			parent.Children = append(parent.Children, p)
			continue
		}
		p.Orphan = p.PPID != self
		roots = append(roots, p)
	}
	sortTree(roots)
	return roots
}

// sortTree orders by start time then pid: stable across snapshots, and reads as
// the order things were spawned rather than jumping around between polls.
func sortTree(ps []*Process) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].StartTime != ps[j].StartTime {
			return ps[i].StartTime < ps[j].StartTime
		}
		return ps[i].PID < ps[j].PID
	})
	for _, p := range ps {
		sortTree(p.Children)
	}
}
