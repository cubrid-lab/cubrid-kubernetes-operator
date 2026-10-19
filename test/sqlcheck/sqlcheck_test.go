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
	const want = "CREATE TABLE s00_check (id INT PRIMARY KEY, marker VARCHAR(64)); " +
		"INSERT INTO s00_check VALUES (1, 'marker-123'); COMMIT;"
	if script != want {
		t.Errorf("SQL = %q, want %q", script, want)
	}
	err = WriteRow(func(string) (string, error) { return "ERROR: insert failed", nil }, "marker-123")
	if err == nil {
		t.Fatal("write accepted csql statement error")
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
