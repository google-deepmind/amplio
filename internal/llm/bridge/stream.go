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
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"

	"amplio/internal/llm"
)

// subStream adapts the bridge's NDJSON /generate response to llm.Stream. It owns
// the HTTP response body and releases it in Close.
type subStream struct {
	resp   *http.Response
	sc     *bufio.Scanner
	cur    llm.StreamEvent
	final  *llm.Response
	err    error
	replay bool // cur is a delta Stream read ahead; the next Next returns it
	done   bool // a final, an error or EOF was reached; Next stays false
}

func (s *subStream) Next() bool {
	if s.replay {
		s.replay = false
		return true
	}
	if s.done {
		return false
	}
	if !s.next() {
		s.done = true
		return false
	}
	return true
}

// next reads lines up to the next delta (true), or to a final, an error or the
// end of the stream (false).
func (s *subStream) next() bool {
	for s.sc.Scan() {
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var wl wireLine
		if err := json.Unmarshal(line, &wl); err != nil {
			s.err = &Error{Code: CodeProtocol, Message: fmt.Sprintf("bridge: bad protocol line: %s", err)}
			return false
		}
		switch wl.Type {
		case "delta":
			s.cur = wl.toStreamEvent()
			return true
		case "final":
			if wl.Response != nil {
				s.final = wl.Response.toLLM()
			}
			return false
		case "error":
			s.err = &Error{Code: cmp.Or(wl.Code, CodeProvider), Message: "bridge error: " + wl.Error}
			return false
		}
		// unknown line types are skipped
	}
	if err := s.sc.Err(); err != nil {
		s.err = fmt.Errorf("bridge: read stream: %w", err)
	}
	return false
}

func (s *subStream) Event() llm.StreamEvent { return s.cur }

func (s *subStream) Response() *llm.Response {
	if s.final != nil {
		return s.final
	}
	return &llm.Response{}
}

func (s *subStream) Err() error { return s.err }

func (s *subStream) Close() { _ = s.resp.Body.Close() }
