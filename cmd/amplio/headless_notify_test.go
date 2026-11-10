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
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/db/sqlite"
	"amplio/internal/event"
)

func TestHeadlessNotifyDeliversToTheSession(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateRun(ctx, db.RunRecord{RunID: "r", Config: config.RunConfig{Task: "t"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, db.SessionRecord{
		RunID: "r", SessionID: "main-agent", AgentType: "standard_agent", Status: db.SessionOngoing,
	}); err != nil {
		t.Fatal(err)
	}

	stop, err := serveHeadlessNotify(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop) // also on an early failure; a second stop is a no-op
	info, err := readServerInfo(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Addr, "http://127.0.0.1:") || info.Token == "" {
		t.Errorf("server.json = %+v, want a loopback address and a token", info)
	}
	t.Setenv(config.EnvDataDir, dataDir)
	t.Setenv(config.EnvRunID, "r")
	t.Setenv(config.EnvSessionID, "main-agent")
	if err := executeNotify(ctx, "job done", "", ""); err != nil {
		t.Fatalf("notify: %v", err)
	}
	events, err := store.GetEvents(ctx, "r", "main-agent", db.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the one notification", len(events))
	}
	msg, ok := events[0].Event.(*event.MessageEvent)
	if !ok || msg.Content != "job done" || msg.SenderType != event.SenderTypeEnvironment {
		t.Fatalf("got %#v, want the notification", events[0].Event)
	}
	// The sender carries the notifier's process group, as under serve.
	wantSender := "environment"
	if pgid := notifierGroup(); pgid > 1 {
		wantSender = fmt.Sprintf("environment (pgid=%d)", pgid)
	}
	if msg.Sender != wantSender {
		t.Errorf("sender = %q, want %q", msg.Sender, wantSender)
	}

	stop()
	if _, err := os.Stat(serverInfoPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("server.json survives stop: %v", err)
	}
}
