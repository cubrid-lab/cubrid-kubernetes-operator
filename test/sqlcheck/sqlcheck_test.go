/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sqlcheck

import (
	"errors"
	"strings"
	"testing"
)

func TestSQLChecksRun(t *testing.T) {
	processErr := errors.New("command exited")
	for _, tc := range []struct {
		name       string
		output     string
		processErr error
		wantError  bool
	}{
		{name: "success", output: "statement executed"},
		{name: "process failure", output: "partial output", processErr: processErr, wantError: true},
		{name: "statement error", output: "heading\nERROR: bad SQL", wantError: true},
		{name: "connection failure", output: "Failed to connect to database", wantError: true},
		{name: "error code", output: "ERROR CODE = -1", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Run(func(sql string) (string, error) {
				if sql != "SELECT 1;" {
					t.Errorf("unexpected SQL: %q", sql)
				}
				return tc.output, tc.processErr
			}, "SELECT 1;")
			if got != tc.output {
				t.Errorf("output = %q, want %q", got, tc.output)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
			if tc.processErr != nil && !errors.Is(err, tc.processErr) {
				t.Errorf("process error not wrapped: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), tc.output) {
				t.Errorf("error omits output: %v", err)
			}
		})
	}
}

func TestSQLChecksWriteRow(t *testing.T) {
	var script string
	err := WriteRow(func(sql string) (string, error) { script = sql; return "committed", nil }, "marker-123")
	if err != nil {
		t.Fatal(err)
	}
	const want = "CREATE TABLE IF NOT EXISTS s00_check (id INT PRIMARY KEY, marker VARCHAR(64)); " +
		"INSERT INTO s00_check (id, marker) SELECT 1, 'marker-123' FROM db_root " +
		"WHERE NOT EXISTS (SELECT 1 FROM s00_check WHERE id = 1); COMMIT;"
	if script != want {
		t.Errorf("SQL = %q, want %q", script, want)
	}
	err = WriteRow(func(string) (string, error) { return "ERROR: insert failed", nil }, "marker-123")
	if err == nil {
		t.Fatal("write accepted csql statement error")
	}
}

// s00Store stands in for csql against a database: it knows whether the
// table exists and how many rows it holds, and answers as CUBRID does to the
// statements WriteRow sends (#591). abortAfterCreate makes the next script
// fail once the table is created, as a session aborted mid-script does.
type s00Store struct {
	table, abortAfterCreate bool
	rows                    int
}

func (s *s00Store) run(sql string) (string, error) {
	switch {
	case strings.Contains(sql, "CREATE TABLE IF NOT EXISTS"):
		s.table = true
	case strings.Contains(sql, "CREATE TABLE"):
		if s.table {
			return "ERROR: Class dba.s00_check already exists.", nil
		}
		s.table = true
	}
	if s.abortAfterCreate {
		s.abortAfterCreate = false
		return "ERROR CODE = -111 Your transaction has been aborted", nil
	}
	switch {
	case strings.Contains(sql, "WHERE NOT EXISTS"):
		if s.rows == 0 {
			s.rows = 1
		}
	case strings.Contains(sql, "INSERT"):
		if s.rows > 0 {
			return "ERROR: Operation would have caused one or more unique constraint violations.", nil
		}
		s.rows++
	}
	return "Committed.", nil
}

// A retry after an attempt that created the table and then failed writes the
// row once, and a further retry writes nothing more (#591).
func TestSQLChecksWriteRowRetry(t *testing.T) {
	s := &s00Store{abortAfterCreate: true}
	if err := WriteRow(s.run, "marker-123"); err == nil {
		t.Fatal("the aborted attempt was reported as written")
	}
	if !s.table || s.rows != 0 {
		t.Fatalf("after the aborted attempt: table %t, rows %d", s.table, s.rows)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		if err := WriteRow(s.run, "marker-123"); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if s.rows != 1 {
			t.Fatalf("attempt %d left %d rows, want 1", attempt, s.rows)
		}
	}
}

// A row left by something else is not overwritten, and ReadRow then refuses
// it for its marker.
func TestSQLChecksWriteRowKeepsAnOldRow(t *testing.T) {
	s := &s00Store{table: true, rows: 1}
	if err := WriteRow(s.run, "marker-123"); err != nil {
		t.Fatal(err)
	}
	if s.rows != 1 {
		t.Fatalf("rows %d, want the old one only", s.rows)
	}
	if err := ReadRow(func(string) (string, error) { return "1 'old-marker'\n1 row selected", nil },
		"marker-123"); err == nil {
		t.Fatal("ReadRow accepted the old row")
	}
}

func TestSQLChecksReadRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		wantError bool
	}{
		{name: "success", output: "1 'marker-123'\n1 row selected"},
		{name: "missing marker", output: "1 row selected", wantError: true},
		{name: "wrong marker", output: "1 'other'\n1 row selected", wantError: true},
		{name: "unquoted marker", output: "1 marker-123\n1 row selected", wantError: true},
		{name: "wrong row count", output: "1 'marker-123'\n2 rows selected", wantError: true},
		{name: "missing row count", output: "1 'marker-123'", wantError: true},
		{name: "statement error", output: "ERROR: query failed", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ReadRow(func(sql string) (string, error) {
				if sql != "SELECT id, marker FROM s00_check ORDER BY id;" {
					t.Errorf("unexpected SQL: %q", sql)
				}
				return tc.output, nil
			}, "marker-123")
			if (err != nil) != tc.wantError {
				t.Errorf("error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
