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

// Package sqlcheck verifies csql results independently of database installation.
package sqlcheck

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Runner runs one SQL script against a database and returns csql's output.
type Runner func(string) (string, error)

// csqlError matches the ways csql reports a failed statement.
var csqlError = regexp.MustCompile(`(?m)^ERROR:|Failed to connect|ERROR CODE`)

// StatementError is the error of Run for a statement csql reported as failed,
// as opposed to a command that could not be run or an output that did not
// hold what a check looked for.
type StatementError struct {
	err error
	// out is csql's output; commandFailed is true when the command itself
	// also returned an error, such as a non-zero exit status.
	out           string
	commandFailed bool
}

func (e *StatementError) Error() string { return e.err.Error() }
func (e *StatementError) Unwrap() error { return e.err }

// Run runs sql and fails on a command error or on an error in the output:
// csql's exit status alone is not relied on.
func Run(run Runner, sql string) (string, error) {
	out, err := run(sql)
	loc := csqlError.FindStringIndex(out)
	if err != nil {
		err = fmt.Errorf("csql failed: %w\n%s", err, out)
		if loc != nil {
			return out, &StatementError{err: err, out: out, commandFailed: true}
		}
		return out, err
	}
	// The line csql reported comes first, so that a record cut short still
	// shows it, and not only a NOTIFICATION csql printed before it.
	if loc != nil {
		line := out[loc[0]:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		return out, &StatementError{err: fmt.Errorf("csql reported an error: %s\n%s", strings.TrimSpace(line), out),
			out: out}
	}
	return out, nil
}

// quotedValue matches one string value in csql's result table.
var quotedValue = regexp.MustCompile(`^\s*'(.*)'\s*$`)

// Values returns the values of a query that returns one string column, from
// csql's output. They follow the line of "=" under the column title; the
// title itself is the text of the expression and may be quoted too.
func Values(out string) ([]string, error) {
	_, table, found := strings.Cut(out, "\n==")
	if !found {
		if strings.Contains(out, "There are no results.") {
			return nil, nil
		}
		return nil, fmt.Errorf("no result table in the output of csql:\n%s", out)
	}
	var values []string
	for line := range strings.SplitSeq(table, "\n") {
		if m := quotedValue.FindStringSubmatch(line); m != nil {
			values = append(values, m[1])
		}
	}
	return values, nil
}

// s00Table is the table the S00 checks write and read.
const s00Table = "s00_check"

// WriteRow creates the table and commits one row with the given marker. It
// is safe to retry: an earlier attempt that created the table, or also wrote
// the row, leaves one row after the retry, never a second (#591). A table
// or row left by something else is not refused here: ReadRow then finds the
// other marker.
func WriteRow(run Runner, marker string) error {
	_, err := Run(run, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id INT PRIMARY KEY, marker VARCHAR(64)); "+
			"INSERT INTO %s (id, marker) SELECT 1, '%s' FROM db_root "+
			"WHERE NOT EXISTS (SELECT 1 FROM %s WHERE id = 1); COMMIT;",
		s00Table, s00Table, marker, s00Table))
	return err
}

// ReadRow checks that exactly the row written by WriteRow is there.
func ReadRow(run Runner, marker string) error {
	out, err := Run(run, fmt.Sprintf("SELECT id, marker FROM %s ORDER BY id;", s00Table))
	if err != nil {
		return err
	}
	return rowRead(out, marker)
}

// rowCount matches the line in which csql counts one selected row.
var rowCount = regexp.MustCompile(`(?m)^1 row selected\b`)

// rowRead checks that out, csql's output of ReadRow's query, holds exactly
// the row written by WriteRow: in the result table, the row with id 1 and the
// marker, then the count of one row. csql's other output, such as its
// messages on stderr, can come before or after the table and is not looked at.
func rowRead(out, marker string) error {
	_, result, found := strings.Cut(out, "=== <Result of SELECT")
	if found {
		_, result, found = strings.Cut(result, "\n==")
	}
	if !found {
		return fmt.Errorf("no result table in the output of csql:\n%s", out)
	}
	count := rowCount.FindStringIndex(result)
	if count == nil {
		return fmt.Errorf("expected exactly one row:\n%s", out)
	}
	row := regexp.MustCompile(`(?m)^\s*1\s+'` + regexp.QuoteMeta(marker) + `'\s*$`)
	if !row.MatchString(result[:count[0]]) {
		return fmt.Errorf("row with id 1 and marker %q not found:\n%s", marker, out)
	}
	return nil
}

// lostSessionErrors are the two errors csql prints, before or after the
// result, when it reports that the server stopped responding and that its
// transaction was aborted. The output does not say what happened to the
// session on the server.
var lostSessionErrors = []*regexp.Regexp{
	regexp.MustCompile(`ERROR CODE = -199\b`),
	regexp.MustCompile(`ERROR CODE = -111\b`),
}

// SessionLost reports whether csql's output holds both of those errors:
// -199 (server no longer responding) and -111 (transaction aborted). POC-44
// saw csql print the pair as it exited after statements that had committed,
// and POC-49 once found it in csql's own log for a run that printed neither;
// its cause is unknown. The pair alone does not say whether the statement
// took effect.
func SessionLost(out string) bool {
	for _, re := range lostSessionErrors {
		if !re.MatchString(out) {
			return false
		}
	}
	return true
}

// lostSessionCodes matches the two codes of SessionLost where csql prints
// them.
var lostSessionCodes = regexp.MustCompile(`ERROR CODE = -(199|111)\b`)

// LostSessionAfterRow reports whether err, an error of ReadRow, holds only
// the pair of SessionLost from a read that exited 0 and whose output holds
// exactly the row WriteRow wrote. Any other error in the output, a -199
// without -111 or without the row, and a command that failed are not such a
// report. It does not make the read conclusive: the pair leaves unknown what
// happened to the session, so a caller reads again rather than accept the row.
func LostSessionAfterRow(err error, marker string) bool {
	var statement *StatementError
	return errors.As(err, &statement) && !statement.commandFailed && SessionLost(statement.out) &&
		!csqlError.MatchString(lostSessionCodes.ReplaceAllString(statement.out, "")) &&
		rowRead(statement.out, marker) == nil
}

// readOnlyRefusal matches the error of a statement refused because the server
// does not take updates, as a slave answers a client (POC-52).
var readOnlyRefusal = regexp.MustCompile(`ERROR CODE = -581\b`)

// RefusedAsReadOnly reports whether err, an error of Run, is a statement the
// server refused with -581. Any other failure, such as a command that could
// not be run or the pair of SessionLost, says nothing about what the server
// would have done with the statement, so it is not a refusal, and neither is
// a -581 beside that pair.
func RefusedAsReadOnly(err error) bool {
	var statement *StatementError
	return errors.As(err, &statement) && readOnlyRefusal.MatchString(statement.out) && !SessionLost(statement.out)
}
