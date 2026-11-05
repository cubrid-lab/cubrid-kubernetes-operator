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

// csqlConnected is what csql prints before the result when it connects to a
// server on another host than its default: a NOTIFICATION, not an error.
const csqlConnected = "Time: 11/05/26 04:08:57.265 - NOTIFICATION *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE = -971, " +
	"Tran = -1, EID = 1\nProgram 'csql' (pid 4988) connected to database server 'appdb@localhost' " +
	"on the host 'localhost' (port 1523).\n\n"

func TestSQLChecksRunIgnoresANotification(t *testing.T) {
	out := csqlConnected + "=== <Result of SELECT Command in Line 1> ===\n\n  v\n=======\n  '42'\n"
	got, err := Run(func(string) (string, error) { return out, nil }, "SELECT 1;")
	if err != nil {
		t.Fatalf("a NOTIFICATION before the result was taken as an error: %v", err)
	}
	if values, err := Values(got); err != nil || len(values) != 1 || values[0] != "42" {
		t.Errorf("values after a NOTIFICATION = %v, %v", values, err)
	}
}

func TestSQLChecksRunNamesTheErrorLineFirst(t *testing.T) {
	out := csqlConnected + "In line 1, column 1,\n\nERROR: Server is not ready yet.\n"
	_, err := Run(func(string) (string, error) { return out, nil }, "SELECT 1;")
	// A short record of the error, as a sampler keeps it, must still show
	// the line csql reported, not only the NOTIFICATION before it.
	if err == nil || !strings.HasPrefix(err.Error(), "csql reported an error: ERROR: Server is not ready yet.") {
		t.Errorf("error = %v, want it to start with the line csql reported", err)
	}
}

// TestSQLChecksStatementError checks that only an error csql reported is a
// StatementError: not a command that could not be run, and not a row that
// does not match.
func TestSQLChecksStatementError(t *testing.T) {
	for name, tc := range map[string]struct {
		run  func() error
		want bool
	}{
		"reported statement": {func() error {
			_, err := Run(func(string) (string, error) { return "ERROR: bad SQL", nil }, "x")
			return err
		}, true},
		"connection failure with a failed command": {func() error {
			_, err := Run(func(string) (string, error) { return "Failed to connect", errors.New("exit 1") }, "x")
			return err
		}, true},
		"command that could not be run": {func() error {
			_, err := Run(func(string) (string, error) { return "", errors.New("transport") }, "x")
			return err
		}, false},
		"row of another marker": {func() error {
			return ReadRow(func(string) (string, error) { return "1 'other'\n1 row selected", nil }, "m")
		}, false},
		"read statement that failed": {func() error {
			return ReadRow(func(string) (string, error) { return "ERROR: no table", nil }, "m")
		}, true},
	} {
		var se *StatementError
		if got := errors.As(tc.run(), &se); got != tc.want {
			t.Errorf("%s: StatementError = %v, want %v", name, got, tc.want)
		}
	}
}

// ciLostSessionRead is csql's output of the read of S00's marker row at the
// end of the smoke checks in the Kind e2e run 41986930319 of PR #698
// (2026-11-05), as the run's log printed it: the row and "1 row selected",
// with -199 and -111 printed on stderr, which came before stdout.
const ciLostSessionRead = "\n" +
	"Time: 11/05/26 08:08:58.431 - NOTIFICATION *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  " +
	"CODE = -971, Tran = -1, EID = 1\n" +
	"Program 'csql' (pid 376) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).\n" +
	"\n" +
	"Time: 11/05/26 08:08:58.445 - ERROR *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/communication/network_cl.c, line 177 " +
	"ERROR CODE = -199, Tran = 3, EID = 2\n" +
	"Server no longer responding.... Invalid argument\n" +
	"\n" +
	"Time: 11/05/26 08:08:58.445 - ERROR *** file " +
	"/home/jenkins/workspace/cubrid_release_11.4/src/transaction/transaction_cl.c, line 600 " +
	"ERROR CODE = -111, Tran = 3, EID = 3\n" +
	"Your transaction has been aborted by the system due to server failure or mode change.\n" +
	"\n" +
	"=== <Result of SELECT Command in Line 1> ===\n" +
	"\n" +
	"           id  marker              \n" +
	"===================================\n" +
	"            1  's00-before-pod-delete'\n" +
	"\n" +
	"1 row selected. (0.001000 sec) Committed. (0.000000 sec) \n"

// ciMarker is the marker of the row in ciLostSessionRead.
const ciMarker = "s00-before-pod-delete"

// TestSessionLostNeedsBothErrors checks that csql's output is taken for a
// lost session only when it holds both -199 (server no longer responding)
// and -111 (transaction aborted), the pair POC-44 saw csql print after
// statements that took effect.
func TestSessionLostNeedsBothErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want bool
	}{
		"the read of the Kind run": {ciLostSessionRead, true},
		"-199 alone":               {"ERROR CODE = -199, Tran = 6, EID = 2\nServer no longer responding", false},
		"-111 alone":               {"ERROR CODE = -111, Tran = 6, EID = 3\nYour transaction has been aborted", false},
		"other codes":              {"ERROR CODE = -1990, Tran = 6\nERROR CODE = -1110, Tran = 6", false},
		"a clean read":             {"=== <Result of SELECT Command in Line 1> ===\n\n1 row selected.", false},
	} {
		if got := SessionLost(tc.out); got != tc.want {
			t.Errorf("%s: SessionLost = %v, want %v", name, got, tc.want)
		}
	}
}

// TestLostSessionAfterRow checks which errors of ReadRow are only csql's
// report of a lost session after it printed the row: both -199 and -111,
// exactly the row WriteRow wrote, and no command error. A -199 without the
// row, or with only one of the two codes, stays a failed statement.
func TestLostSessionAfterRow(t *testing.T) {
	read := func(out string, cmdErr error) error {
		return ReadRow(func(string) (string, error) { return out, cmdErr }, ciMarker)
	}
	cut, _, _ := strings.Cut(ciLostSessionRead, "=== <Result")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the read of the Kind run": {read(ciLostSessionRead, nil), true},
		"the same output after a failed command": {read(ciLostSessionRead, errors.New("exit status 1")),
			false},
		"-199 and -111 without a result": {read(cut, nil), false},
		"-199 alone with the row": {read(strings.Replace(ciLostSessionRead, "ERROR CODE = -111", "CODE = -971",
			1), nil), false},
		"another marker": {read(strings.Replace(ciLostSessionRead, ciMarker, "other", 1), nil), false},
		"two rows": {read(strings.Replace(ciLostSessionRead, "1 row selected", "2 rows selected", 1), nil),
			false},
		"a clean read":  {nil, false},
		"another error": {read("ERROR: no table", nil), false},
		"another error besides the pair": {read(strings.Replace(ciLostSessionRead, "=== <Result",
			"ERROR CODE = -1, Tran = 3, EID = 4\n=== <Result", 1), nil), false},
	} {
		if got := LostSessionAfterRow(tc.err, ciMarker); got != tc.want {
			t.Errorf("%s: LostSessionAfterRow = %v, want %v", name, got, tc.want)
		}
	}
	if err := read(ciLostSessionRead, nil); err == nil {
		t.Error("ReadRow accepted the read of the Kind run; it keeps reporting the -199 as an error")
	}
}
