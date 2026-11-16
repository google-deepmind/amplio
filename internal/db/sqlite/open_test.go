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

package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFileDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := openDB(filepath.Join(t.TempDir(), "amplio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// busy_timeout and foreign_keys are per connection, so every connection of a
// file database's pool must have them, not just the one Open ran them on.
func TestOpen_EveryPooledConnectionHasThePragmas(t *testing.T) {
	sqlDB := openFileDB(t)
	ctx := t.Context()
	// Held at once, so they are distinct connections.
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		c, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close() //nolint:errcheck
		conns[i] = c
	}
	for i, c := range conns {
		var busy int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busy)
		}
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO Session (run_id, session_id, status) VALUES ('no-such-run', 's', 'idle')`)
		_ = tx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
			t.Errorf("conn %d: a session of a missing run was accepted (err %v): foreign keys not enforced", i, err)
		}
	}
}

// A write that finds another connection holding the write lock waits for it,
// on whichever pooled connection it runs, instead of failing with SQLITE_BUSY.
func TestOpen_WriterWaitsForTheWriteLock(t *testing.T) {
	sqlDB := openFileDB(t)
	ctx := t.Context()
	holder, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close() //nolint:errcheck
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	wrote := make(chan error, 1)
	go func() {
		close(started)
		// holder is checked out, so this runs on another connection.
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO Run (run_id) VALUES ('r')`)
		wrote <- err
	}()
	<-started
	// Hold the lock a little past the writer's start; the writer must wait it
	// out however the two are scheduled.
	time.Sleep(50 * time.Millisecond)
	if _, err := holder.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Errorf("write while another connection held the lock: %v", err)
	}
}
