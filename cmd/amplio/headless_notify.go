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

package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"amplio/internal/db"
	"amplio/internal/server"
)

// serveHeadlessNotify gives a headless process the one server endpoint its
// agents rely on: `amplio-notify` (which agents are prompted to use for
// background jobs) finds it through server.json, exactly as under serve. It
// listens on loopback with a per-process token. The returned stop function
// closes it and removes server.json.
func serveHeadlessNotify(dataDir string, store db.Store) (func(), error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: server.NotifyHandler(store, token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("headless notify endpoint stopped", "error", err)
		}
	}()
	addr := "http://" + ln.Addr().String()
	if err := writeServerInfo(dataDir, serverInfo{PID: os.Getpid(), URL: addr, Addr: addr, Token: token}); err != nil {
		_ = srv.Close()
		return nil, err
	}
	return func() {
		_ = srv.Close()
		_ = os.Remove(serverInfoPath(dataDir))
	}, nil
}

// startHeadlessNotify is serveHeadlessNotify for the headless entry points,
// where a missing endpoint degrades notifications rather than the run.
func startHeadlessNotify(dataDir string, store db.Store) func() {
	stop, err := serveHeadlessNotify(dataDir, store)
	if err != nil {
		slog.Warn("amplio-notify will not reach agents in this process", "error", err)
		return func() {}
	}
	return stop
}
