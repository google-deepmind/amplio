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

package bash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"amplio/internal/config"
	"amplio/internal/proctree"
)

// runEnv is a run-bound bash environment over a fresh data dir; it returns the
// env and the dir where the run's call output files live.
func runEnv(t *testing.T) ([]string, string) {
	t.Helper()
	config.SetDataDir(t.TempDir())
	t.Cleanup(func() { config.SetDataDir("") })
	runID := fmt.Sprintf("tstbg%d", os.Getpid())
	return agentEnv(runID, "swift-fox"), filepath.Join(config.ArtifactDir(runID), "bg")
}

var stopRe = regexp.MustCompile(`kill -- -(\d+)`)

// run executes cmd and stops whatever it reports as still running when the
// test ends, so a failing test leaves nothing behind.
func run(t *testing.T, env []string, cmd string) string {
	t.Helper()
	out := execute(context.Background(), cmd, t.TempDir(), env, 10*time.Second, DefaultMaxOutputLength)
	for _, m := range stopRe.FindAllStringSubmatch(out, -1) {
		pg, _ := strconv.Atoi(m[1])
		t.Cleanup(func() { _ = syscall.Kill(-pg, syscall.SIGKILL) })
	}
	return out
}

func callFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

var outPathRe = regexp.MustCompile(`Its output goes to (\S+\.out)`)

// A background job that wasn't redirected keeps writing to the call's stdout
// file after the call returns: the file is kept, under the run's artifact dir,
// and the result says so, with the handle to check or stop the job.
func TestBash_BackgroundOutputIsKept(t *testing.T) {
	env, bg := runEnv(t)
	out := run(t, env, `( sleep 1; echo late ) & echo started`)
	for _, want := range []string{"started", "BACKGROUND:\nStill running after this call returned:", "- process group ", "kill -0 -- -", "kill -- -"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	m := outPathRe.FindStringSubmatch(out)
	if m == nil || !strings.HasPrefix(m[1], bg+string(os.PathSeparator)) {
		t.Fatalf("result names no kept output file under %s:\n%s", bg, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(m[1]); strings.Contains(string(b), "late") {
			if !strings.Contains(string(b), "started") {
				t.Errorf("kept file lost the call's own output: %q", b)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(m[1])
	t.Fatalf("background output never reached the kept file; it holds %q", b)
}

// A redirected job is still reported, but nothing writes to the call's files,
// so they are deleted. The redirect is on a group, which bash applies before it
// forks the job. With `( sleep 2 ) > /dev/null 2>&1 &` the forked child
// redirects itself, and a scan that runs before it has (the parent shell exits
// at once) sees it still writing to the call's files, which are then kept.
func TestBash_RedirectedBackgroundFilesDeleted(t *testing.T) {
	env, bg := runEnv(t)
	out := run(t, env, `{ sleep 2 & } > /dev/null 2>&1`)
	if !strings.Contains(out, "BACKGROUND:") || strings.Contains(out, "Its output goes to") {
		t.Errorf("want a BACKGROUND note without an output file:\n%s", out)
	}
	if files := callFilesIn(t, bg); len(files) != 0 {
		t.Errorf("call files left behind: %v", files)
	}
}

// A job that left the call's process group with setsid is still found, by the
// call's marker in its environment.
func TestBash_SetsidJobFound(t *testing.T) {
	env, _ := runEnv(t)
	out := run(t, env, `setsid sleep 2 > /dev/null 2>&1 &`)
	if !strings.Contains(out, "BACKGROUND:") || !strings.Contains(out, "sleep 2") {
		t.Errorf("setsid job not reported:\n%s", out)
	}
}

// Nothing left running: no note, and no files left behind.
func TestBash_NothingLeftNoNote(t *testing.T) {
	env, bg := runEnv(t)
	out := run(t, env, `echo hi`)
	if strings.Contains(out, "BACKGROUND") {
		t.Errorf("note for a call that left nothing running:\n%s", out)
	}
	if files := callFilesIn(t, bg); len(files) != 0 {
		t.Errorf("call files left behind: %v", files)
	}
}

// Without a run (ephemeral helper loops) the tool behaves as before: no note,
// output files in the temp dir, deleted.
func TestBash_NoRunNoNote(t *testing.T) {
	out := run(t, nil, `( sleep 1 ) > /dev/null 2>&1 &`)
	if strings.Contains(out, "BACKGROUND") {
		t.Errorf("note without a run:\n%s", out)
	}
}

// Without per-process facts the call's own process group is the only signal:
// while it has members, both files are kept and the note names the group.
func TestBash_NoProcessInfoFallsBackToGroup(t *testing.T) {
	env, bg := runEnv(t)
	orig := scanProcs
	scanProcs = func(string) proctree.Snapshot { return proctree.Snapshot{Supported: false} }
	t.Cleanup(func() { scanProcs = orig })
	out := run(t, env, `( sleep 2; echo late ) &`)
	if !strings.Contains(out, "Something this call started is still running (process group ") ||
		!strings.Contains(out, "(stdout) and ") {
		t.Errorf("want the group fallback with both files kept:\n%s", out)
	}
	if files := callFilesIn(t, bg); len(files) != 2 {
		t.Errorf("want both call files kept, got %v", files)
	}
}

// /proc reports a process's open files by their real path. With a symlink in
// the data dir's path, the kept-output check must still match, or the file a
// background job writes to would be deleted under it.
func TestBash_SymlinkedDataDirKeepsOutput(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "data-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	config.SetDataDir(link)
	t.Cleanup(func() { config.SetDataDir("") })
	out := run(t, agentEnv(fmt.Sprintf("tstsym%d", os.Getpid()), "swift-fox"), `( sleep 1; echo late ) & echo started`)
	if !strings.Contains(out, "Its output goes to") {
		t.Errorf("output of a job writing through a symlinked data dir was not kept:\n%s", out)
	}
}
