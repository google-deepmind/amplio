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
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// memoryPath is SQLite's magic filename for a private in-memory database — the
// mode Open documents for tests.
const memoryPath = ":memory:"

// initSchema brings a freshly opened database up to the current schema.
//
// An in-memory database starts empty every time, so the DDL below never has
// anything to do but rebuild the same result — and that rebuild is expensive in
// exactly the situation it is used in: ~50ms per open under -race, executed
// under SQLite's process-global page-cache mutex, so a test suite that opens one
// store per test pays it serially even when the tests run in parallel. Restore a
// process-cached page image of the finished schema instead; it is produced by
// running this very DDL once, so the resulting database is identical.
func initSchema(sqlDB *sql.DB, path string) error {
	if path == memoryPath {
		img, err := schemaImage()
		if err != nil {
			return err
		}
		return withRawConn(sqlDB, func(c pageImageConn) error { return c.Deserialize(img) })
	}
	if _, err := sqlDB.Exec(schema); err != nil {
		return fmt.Errorf("sqlite schema: %w", err)
	}
	return migrate(sqlDB)
}

// schemaImage is the page image of an empty, fully migrated database. The bytes
// are read-only after the first build: Deserialize copies them into SQLite's own
// memory, so concurrent opens can share one image.
var schemaImage = sync.OnceValues(buildSchemaImage)

func buildSchemaImage() ([]byte, error) {
	sqlDB, err := sql.Open("sqlite", memoryPath)
	if err != nil {
		return nil, fmt.Errorf("sqlite schema image: %w", err)
	}
	defer sqlDB.Close() //nolint:errcheck // read-only template DB
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schema); err != nil {
		return nil, fmt.Errorf("sqlite schema image: %w", err)
	}
	if err := migrate(sqlDB); err != nil {
		return nil, fmt.Errorf("sqlite schema image: %w", err)
	}
	var img []byte
	err = withRawConn(sqlDB, func(c pageImageConn) error {
		var err error
		img, err = c.Serialize()
		return err
	})
	if err != nil {
		return nil, err
	}
	return img, nil
}

// pageImageConn is the driver's whole-database image API (sqlite3_serialize /
// sqlite3_deserialize), which database/sql only exposes through Conn.Raw.
type pageImageConn interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

func withRawConn(sqlDB *sql.DB, fn func(pageImageConn) error) error {
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("sqlite raw conn: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returning it to the pool
	return conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(pageImageConn)
		if !ok {
			return fmt.Errorf("sqlite driver conn %T has no page-image API", driverConn)
		}
		return fn(c)
	})
}
