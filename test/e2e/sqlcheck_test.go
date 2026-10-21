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
	"context"
	"os/exec"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

// sqlRunner runs one SQL script against a database and returns csql's output.
// The SQL checks below depend only on this, not on how the database was
// installed, so they can run against a cluster another operator created (#93).
type sqlRunner = sqlcheck.Runner

// csqlInPod runs csql inside a DB Pod, as a client on that host would.
func csqlInPod(ctx context.Context, namespace, pod, database string) sqlRunner {
	return func(sql string) (string, error) {
		return utils.Run(exec.CommandContext(ctx, "kubectl", "-n", namespace, "exec", pod, "--",
			"bash", "-c", `PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"`, database+"@localhost", sql))
	}
}

// runSQL runs sql and checks csql output for errors.
func runSQL(run sqlRunner, sql string) (string, error) {
	return sqlcheck.Run(run, sql)
}

// s00Table is the table the S00 checks write and read.
const s00Table = "s00_check"

// writeS00Row creates the table and commits one row with the given marker.
func writeS00Row(run sqlRunner, marker string) error {
	return sqlcheck.WriteRow(run, marker)
}

// readS00Row checks that exactly the row written by writeS00Row is there.
func readS00Row(run sqlRunner, marker string) error {
	return sqlcheck.ReadRow(run, marker)
}
