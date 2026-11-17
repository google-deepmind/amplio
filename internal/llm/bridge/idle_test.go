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

package bridge

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// The configured idle timeout holds for the whole response, not only until
// its first byte: each read that brings data restarts the configured wait.
// (It used to restart the default, 5 minutes, so idle_timeout= applied only
// to the wait for the first byte.)
func TestIdleReader_KeepsTheConfiguredTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	r := newIdleReader(pr, 100*time.Millisecond, func() { pw.CloseWithError(context.Canceled) }, "http://far")
	defer r.Close()
	go func() { _, _ = pw.Write([]byte("first line\n")) }() // then nothing
	buf := make([]byte, 64)
	if n, err := r.Read(buf); err != nil || n == 0 {
		t.Fatalf("first read = %d, %v", n, err)
	}
	stalled := make(chan error, 1)
	go func() { _, err := r.Read(buf); stalled <- err }() // the stream stalls after its first line
	select {
	case err := <-stalled:
		if err == nil || !strings.Contains(err.Error(), "no data for 100ms") {
			t.Fatalf("stalled read = %v, want the configured 100ms in the error", err)
		}
	case <-time.After(5 * time.Second):
		pw.Close() // unblock the read
		t.Fatal("still waiting 5s into a stall, with a 100ms idle timeout")
	}
}
