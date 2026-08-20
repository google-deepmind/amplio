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
	snap.Roots = forest(byPID)
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
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil || len(env) == 0 {
		return nil
	}
	run := envValue(env, config.EnvRunID)
	if run == "" || (runID != "" && run != runID) {
		return nil
	}
	p := &Process{PID: pid, RunID: run, SessionID: envValue(env, config.EnvSessionID)}
	if !readStat(dir, p, uptime) {
		return nil // exited between the readdir and here; not an error
	}
	p.Cmdline = readCmdline(dir)
	return p
}

// envValue pulls one NUL-separated KEY=value out of a /proc environ blob.
func envValue(env []byte, key string) string {
	want := append([]byte(key), '=')
	for _, kv := range bytes.Split(env, []byte{0}) {
		if bytes.HasPrefix(kv, want) {
			return string(kv[len(want):])
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
	p.StartTime = uint64(num(19))
	p.RSSBytes = num(21) * int64(os.Getpagesize())
	if uptime > 0 {
		p.Elapsed = max(0, uptime-float64(p.StartTime)/float64(clockTick))
	}
	return true
}

// readCmdline returns the argv with NULs turned into spaces. Empty for a kernel
// thread or a zombie, where the caller shows comm-less rows rather than lying.
func readCmdline(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(b) == 0 {
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
// whose parent is missing is a root: either its launching shell has exited (the
// interesting case — it was reparented and outlived its call) or the parent is
// amplio itself.
func forest(byPID map[int]*Process) []*Process {
	// Non-nil even when empty: the JSON must be [] rather than null, so the UI
	// renders "nothing running" instead of special-casing a missing list.
	roots := make([]*Process, 0, len(byPID))
	for _, p := range byPID {
		if parent, ok := byPID[p.PPID]; ok && parent != p {
			parent.Children = append(parent.Children, p)
			continue
		}
		p.Orphan = p.PPID == 1
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
