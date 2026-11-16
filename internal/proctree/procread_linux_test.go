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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"amplio/internal/config"
)

// A scan that reads a process as it execs sees its environ before the exec or
// after it, never cut off in between. The marker comes last, as the bash
// tool's does, and a 200 kB environment makes a read in chunks span the exec
// in most rounds: such a read lost the marker, and with it the process. bash
// waits on a FIFO first, so that the reads race the exec and not bash's own
// startup.
func TestInspect_FindsAProcessThatExecsWhileRead(t *testing.T) {
	runID := fmt.Sprintf("ex%d", os.Getpid())
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		cmd := exec.Command("bash", "-c", `read -r _ < "$1"; exec sleep 30`, "bash", gate)
		cmd.Env = os.Environ()
		for i := range 20 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("PROCTREE_TEST_PAD%d=%s", i, strings.Repeat("x", 10_000)))
		}
		cmd.Env = append(cmd.Env, config.EnvRunID+"="+runID)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		pid := cmd.Process.Pid
		stop := func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = cmd.Wait() }
		dir := "/proc/" + strconv.Itoa(pid)
		// Opening the FIFO for writing blocks until bash has opened it to
		// read, i.e. until bash is past its startup; the write releases it.
		if err := os.WriteFile(gate, []byte("go\n"), 0o600); err != nil {
			stop()
			t.Fatal(err)
		}
		uptime := uptimeSeconds()
		for deadline := time.Now().Add(5 * time.Second); ; {
			if inspect(pid, os.Getuid(), runID, uptime) == nil {
				stop()
				t.Fatalf("round %d: the marked process wasn't recognised while it exec'd sleep", round)
			}
			if comm, _ := os.ReadFile(dir + "/comm"); strings.TrimSpace(string(comm)) == "sleep" {
				break
			}
			if time.Now().After(deadline) {
				stop()
				t.Fatalf("round %d: bash never exec'd sleep", round)
			}
		}
		stop()
	}
}

// One scan of every process on the machine: the cost of the post-call scan
// the bash tool runs after every call.
func BenchmarkScan(b *testing.B) {
	for b.Loop() {
		Scan("")
	}
}
