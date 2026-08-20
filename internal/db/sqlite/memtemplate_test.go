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
)

// The in-memory fast path restores a page image instead of running the DDL, so
// the two must be indistinguishable — every table, column, index and trigger.
// A schema change that only the DDL path can express would otherwise be invisible
// until a test that needs it failed somewhere else entirely.
func TestInitSchema_MemoryImageMatchesDDL(t *testing.T) {
	fromDDL := schemaDump(t, filepath.Join(t.TempDir(), "ddl.db"))
	fromImage := schemaDump(t, memoryPath)
	if fromImage != fromDDL {
		t.Errorf("in-memory schema differs from the DDL schema:\nimage:\n%s\nddl:\n%s", fromImage, fromDDL)
	}
	if !strings.Contains(fromDDL, "last_seen_at") {
		t.Error("dump does not mention a migrate()-added column; the comparison is not covering migrations")
	}
}

// schemaDump opens a database at path through initSchema and returns its full
// schema as text.
func schemaDump(t *testing.T, path string) string {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close() //nolint:errcheck
	sqlDB.SetMaxOpenConns(1)
	if err := initSchema(sqlDB, path); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.Query(`SELECT type, name, tbl_name, ifnull(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl, ddl string
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + " " + name + " " + tbl + " " + ddl + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if b.Len() == 0 {
		t.Fatalf("empty schema for %s", path)
	}
	return b.String()
}
