//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// sqlRunner runs one SQL script against a database and returns csql's output.
// The SQL checks below depend only on this, not on how the database was
// installed, so they can run against a cluster another operator created (#93).
type sqlRunner func(sql string) (string, error)

// csqlInPod runs csql inside a DB Pod, as a client on that host would.
func csqlInPod(namespace, pod, database string) sqlRunner {
	return func(sql string) (string, error) {
		return utils.Run(exec.Command("kubectl", "-n", namespace, "exec", pod, "--",
			"bash", "-c", `PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"`, database+"@localhost", sql))
	}
}

// csqlError matches the ways csql reports a failed statement.
var csqlError = regexp.MustCompile(`(?m)^ERROR:|Failed to connect|ERROR CODE`)

// runSQL runs sql and fails on a command error or on an error in the output:
// csql's exit status alone is not relied on.
func runSQL(run sqlRunner, sql string) (string, error) {
	out, err := run(sql)
	if err != nil {
		return out, fmt.Errorf("csql failed: %w\n%s", err, out)
	}
	if csqlError.MatchString(out) {
		return out, fmt.Errorf("csql reported an error:\n%s", out)
	}
	return out, nil
}

// s00Table is the table the S00 checks write and read.
const s00Table = "s00_check"

// writeS00Row creates the table and commits one row with the given marker.
func writeS00Row(run sqlRunner, marker string) error {
	_, err := runSQL(run, fmt.Sprintf(
		"CREATE TABLE %s (id INT PRIMARY KEY, marker VARCHAR(64)); INSERT INTO %s VALUES (1, '%s'); COMMIT;",
		s00Table, s00Table, marker))
	return err
}

// readS00Row checks that exactly the row written by writeS00Row is there.
func readS00Row(run sqlRunner, marker string) error {
	out, err := runSQL(run, fmt.Sprintf("SELECT id, marker FROM %s ORDER BY id;", s00Table))
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
