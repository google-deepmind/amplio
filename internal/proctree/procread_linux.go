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
	"bytes"
	"sync"
	"syscall"
)

// readWhole reads a /proc file in a single read call. A process's environ is
// read from the address space its open file pinned, so if the process execs
// between two reads, the second finds that address space gone and returns
// EOF: a read in chunks (os.ReadFile starts at 512 bytes) of a process caught
// at its exec returns a cut-off environ, without the marker that comes last,
// and the scan takes the process for someone else's. One read sees one address
// space, the old or the new, whole (or none yet, mid-exec: see readSettled).
//
// A file larger than the buffer is read again from the start with a bigger
// one, on the same file: if the process exec'd in between, that read comes
// back empty, and readSettled's mid-exec check reopens the file.
func readWhole(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd) //nolint:errcheck // read-only
	bp := readBufs.Get().(*[]byte)
	defer readBufs.Put(bp)
	for {
		n, err := pread(fd, *bp)
		if err != nil {
			return nil, err
		}
		if n < len(*bp) {
			return bytes.Clone((*bp)[:n]), nil
		}
		*bp = make([]byte, 4*len(*bp))
	}
}

// readBufs recycles readWhole's buffers: a scan reads three files of every
// process of ours, a few hundred on a workstation, after every bash call.
var readBufs = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

func pread(fd int, buf []byte) (int, error) {
	for {
		n, err := syscall.Pread(fd, buf, 0)
		if err != syscall.EINTR {
			return n, err
		}
	}
}
