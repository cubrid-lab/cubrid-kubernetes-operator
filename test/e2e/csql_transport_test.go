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
	"slices"
	"strings"
	"testing"
)

// TestCSQLRunnerConnectsOverTCP checks that the shared csql runner reaches the
// member's own server over TCP, by the member's hostname, and not through the
// local Unix socket that "<db>@localhost" uses. Over that socket csql sometimes
// prints its whole result and then the -199/-111 pair of a lost session; over
// TCP it did not (POC-70, #238), and S02 failed on the pair on Kind.
func TestCSQLRunnerConnectsOverTCP(t *testing.T) {
	var args []string
	run := csqlRunner(context.Background(), "ns", "ha-0", "appdb", func(cmd *exec.Cmd) (string, error) {
		args = cmd.Args
		return "", nil
	})
	if _, err := run("SELECT 1;"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "localhost") {
		t.Errorf("csql connects through the local socket: %q", args)
	}
	if !slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, `"$0@$(hostname)"`) }) {
		t.Errorf("csql does not connect by the member's hostname: %q", args)
	}
	if !slices.Contains(args, "appdb") || !slices.Contains(args, "SELECT 1;") {
		t.Errorf("csql is not given the database and the statement: %q", args)
	}
}

// TestDBAPasswordLoginsConnectOverTCP checks the same for the DBA password
// test's logins on a member: over the local Unix socket, a login that
// succeeded could still print the -199/-111 pair of a lost session and fail
// the step on the word ERROR (#238, Kind run 45092507011).
func TestDBAPasswordLoginsConnectOverTCP(t *testing.T) {
	args := csqlOnArgs("dbaha-0", "appdb", `-p "$(cat /etc/cubrid-dba-password/password)"`, "SELECT 1")
	if strings.Contains(strings.Join(args, " "), "localhost") {
		t.Errorf("csql connects through the local socket: %q", args)
	}
	if !slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, `"$0@$(hostname)"`) }) {
		t.Errorf("csql does not connect by the member's hostname: %q", args)
	}
	if !slices.Contains(args, "appdb") || !slices.Contains(args, "SELECT 1") {
		t.Errorf("csql is not given the database and the statement: %q", args)
	}
}

// TestDBAPasswordTerminalConnectsOverTCP checks the same for the csql sessions
// on a pseudo-terminal, where the DBA password is typed at csql's prompt: the
// login check and the password change on the primary and on a slave. Over the
// local Unix socket csql could print its whole result and then the -199/-111
// pair of a lost session (POC-70, #238). Both build their command with
// kubectlExecTerminal.
func TestDBAPasswordTerminalConnectsOverTCP(t *testing.T) {
	for name, options := range map[string][]string{
		"login check":                    {"-u", "dba", "-c", "SELECT 1"},
		"password change on the primary": {"-u", "dba"},
		"password change on a slave":     {"--sysadm", "--write-on-standby", "-u", "dba"},
	} {
		args := kubectlExecTerminal("ns", "dbaha-0", "appdb", options...).Args
		if strings.Contains(strings.Join(args, " "), "localhost") {
			t.Errorf("%s: csql connects through the local socket: %q", name, args)
		}
		if !slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, `"$0@$(hostname)"`) }) {
			t.Errorf("%s: csql does not connect by the member's hostname: %q", name, args)
		}
		if !slices.Contains(args, "-it") || !slices.Contains(args, "appdb") {
			t.Errorf("%s: csql is not run on a terminal with the database: %q", name, args)
		}
		for _, o := range options {
			if !slices.Contains(args, o) {
				t.Errorf("%s: csql is not given the option %q: %q", name, o, args)
			}
		}
	}
}
