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
	"fmt"
	"regexp"
	"strings"
)

// Runner runs one SQL script against a database and returns csql's output.
type Runner func(string) (string, error)

// csqlError matches the ways csql reports a failed statement.
var csqlError = regexp.MustCompile(`(?m)^ERROR:|Failed to connect|ERROR CODE`)

// Run runs sql and fails on a command error or on an error in the output:
// csql's exit status alone is not relied on.
func Run(run Runner, sql string) (string, error) {
	out, err := run(sql)
	if err != nil {
		return out, fmt.Errorf("csql failed: %w\n%s", err, out)
	}
	if csqlError.MatchString(out) {
		return out, fmt.Errorf("csql reported an error:\n%s", out)
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

// WriteRow creates the table and commits one row with the given marker.
func WriteRow(run Runner, marker string) error {
	_, err := Run(run, fmt.Sprintf(
		"CREATE TABLE %s (id INT PRIMARY KEY, marker VARCHAR(64)); "+
			"INSERT INTO %s VALUES (1, '%s'); COMMIT;",
		s00Table, s00Table, marker))
	return err
}

// ReadRow checks that exactly the row written by WriteRow is there.
func ReadRow(run Runner, marker string) error {
	out, err := Run(run, fmt.Sprintf("SELECT id, marker FROM %s ORDER BY id;", s00Table))
	if err != nil {
		return err
	}
	if !strings.Contains(out, "'"+marker+"'") {
		return fmt.Errorf("row with marker %q not found:\n%s", marker, out)
	}
	if !strings.Contains(out, "1 row selected") {
		return fmt.Errorf("expected exactly one row:\n%s", out)
	}
	return nil
}
