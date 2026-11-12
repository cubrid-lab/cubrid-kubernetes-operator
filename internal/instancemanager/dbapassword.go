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

package instancemanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DBAPasswordFileEnv names the environment variable that holds the path of
// the file with the DBA password requested for a database this member
// creates. Unset, no password is set and nothing below runs.
const DBAPasswordFileEnv = "CUBRID_DBA_PASSWORD_FILE"

// maxDBAPasswordBytes bounds the value read from the password file. It is
// this manager's own bound, not CUBRID's (which was not measured): it keeps a
// misplaced large file from being read whole, and keeps the value far below
// the 4095 bytes a Linux terminal takes on one line, where a later check
// types it at csql's password prompt (docs/poc/RESULTS.md, POC-61, Q1). A
// longer value is refused, never cut.
const maxDBAPasswordBytes = 256

// HasDBAPasswordControlByte reports bytes that csql's password terminal can
// interpret as editing, flow control or signals instead of literal password
// input. RunOnTerminal disables echo, but retains the terminal line
// discipline (ISIG and ICANON). Reject these values before creating a
// database: setting one via SQL stdin then failing to confirm it over a
// terminal could strand the member on subsequent starts. This predicate is
// also used by the Operator's Secret check.
func HasDBAPasswordControlByte(value []byte) bool {
	for _, b := range value {
		if b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

// Events of the DBA password step.
const (
	eventDBAPasswordApplied    = "dba_password_applied"
	eventDBAPasswordAlreadySet = "dba_password_already_set"
)

// errNoStdin is returned when the CLI cannot feed a command's stdin.
var errNoStdin = errors.New("the command runner cannot feed standard input")

// ReadDBAPassword reads the DBA password from path: the file's content
// without at most one trailing line feed. An empty value, one that holds a
// NUL, a carriage return, a line feed, another ASCII control byte or DEL,
// or one longer than
// maxDBAPasswordBytes is refused. No error carries the content.
func ReadDBAPassword(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the manager's own mounted Secret path
	if err != nil {
		return "", fmt.Errorf("open the DBA password file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// One byte more than the longest value and its line feed tells a value
	// that is too long without reading all of it.
	data, err := io.ReadAll(io.LimitReader(f, maxDBAPasswordBytes+2))
	if err != nil {
		return "", fmt.Errorf("read the DBA password file %s: %w", path, err)
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	switch {
	case len(data) == 0:
		return "", fmt.Errorf("the DBA password file %s is empty", path)
	case len(data) > maxDBAPasswordBytes:
		return "", fmt.Errorf("the DBA password in %s is longer than %d bytes", path, maxDBAPasswordBytes)
	case bytes.IndexByte(data, 0) >= 0:
		return "", fmt.Errorf("the DBA password in %s holds a NUL byte", path)
	case bytes.IndexByte(data, '\r') >= 0:
		return "", fmt.Errorf("the DBA password in %s holds a carriage return", path)
	case bytes.IndexByte(data, '\n') >= 0:
		return "", fmt.Errorf("the DBA password in %s holds a line feed other than one at its end", path)
	case HasDBAPasswordControlByte(data):
		return "", fmt.Errorf("the DBA password in %s holds an ASCII control byte", path)
	}
	return string(data), nil
}

// emptyLoginArgs is a csql login as dba with no password, in standalone mode
// on a stopped database. csql reads a password only from -p or a terminal, so
// with no -p and stdin from the null device the password is empty.
func emptyLoginArgs(database string) []string {
	return []string{"-u", "dba", "-S", database, "-c", "SELECT 1"}
}

// passwordRefused is what csql prints when a login's password is wrong
// (docs/poc/RESULTS.md, POC-61).
const passwordRefused = "Incorrect or missing password"

// emptyLoginRefused reports whether a dba login with the empty password is
// refused for its password. A login that neither connects nor is refused for
// its password (a database in use, a missing csql) is an error: it says
// nothing about the password.
func emptyLoginRefused(ctx context.Context, cli CLI, database string) (bool, error) {
	out, err := cli.Run(ctx, "csql", emptyLoginArgs(database)...)
	if strings.Contains(out, passwordRefused) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("a dba login with the empty password neither connected nor was refused for its password: %w: %s",
			err, strings.TrimSpace(out))
	}
	return false, nil
}

// dbaPasswordStepTimeout bounds the DBA password step that runs after a
// restore: each of its csql runs opens the stopped database in standalone
// mode, which takes seconds, not the hours a restore may take.
const dbaPasswordStepTimeout = 5 * time.Minute

// csqlPasswordPrompt is how csql asks for a password on a terminal (POC-61).
const csqlPasswordPrompt = "Enter Password"

// errNoTerminal is returned when the CLI cannot run a command on a terminal.
var errNoTerminal = errors.New("the command runner cannot run a command on a terminal")

// errPasswordNotRequested says that a database has a DBA password other than
// the requested one. It names neither value.
var errPasswordNotRequested = errors.New("the database's DBA password is not the requested one: " +
	"a database keeps the password it was created or backed up with (a restored database keeps its " +
	"source's), and the Instance Manager does not change a password it does not know")

// dbaPasswordAccepted reports whether a csql login as dba on the stopped
// database, with password typed at csql's prompt on a pseudo-terminal, is
// accepted (true) or refused for its password (false). csql reads a password
// only from -p, its argv, or at that prompt (POC-61, Q1), so the password is
// on no argv and in no environment. A login that did not prompt, or that
// failed for another reason (a database in use, a missing csql, the context
// ended), is an error: it says nothing about the password. No error carries
// what csql printed, since a terminal may print back what was typed.
func dbaPasswordAccepted(ctx context.Context, cli TerminalCLI, database, password string) (bool, error) {
	return loginAccepted(ctx, cli, emptyLoginArgs(database), password)
}

// clientLoginArgs is a csql login as dba in client mode, through the running
// server of database on this member. Client mode checks the password as
// standalone mode does (docs/poc/RESULTS.md, POC-61, Q5).
func clientLoginArgs(database string) []string {
	return []string{"-u", "dba", "-c", "SELECT 1", database + "@localhost"}
}

// runningDBAPasswordAccepted is dbaPasswordAccepted for a database whose
// server runs: the login goes through that server in client mode, and changes
// nothing.
func runningDBAPasswordAccepted(ctx context.Context, cli TerminalCLI, database, password string) (bool, error) {
	return loginAccepted(ctx, cli, clientLoginArgs(database), password)
}

// loginAccepted runs csql with args on a pseudo-terminal, types password at
// its prompt, and tells an accepted login from one refused for its password,
// as dbaPasswordAccepted describes.
func loginAccepted(ctx context.Context, cli TerminalCLI, args []string, password string) (bool, error) {
	out, prompted, err := cli.RunOnTerminal(ctx, csqlPasswordPrompt, password, "csql", args...)
	switch {
	case !prompted:
		return false, fmt.Errorf("csql did not prompt for the DBA password on a terminal (%s)", exitOf(err))
	case strings.Contains(out, passwordRefused):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("a dba login on a terminal neither connected nor was refused for its password (%s); "+
			"its output is not shown because it may hold what was typed", exitOf(err))
	}
	return true, nil
}

// exitOf describes how a command ended without anything it printed.
func exitOf(err error) string {
	if err == nil {
		return "exit code 0"
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Sprintf("exit code %d", exit.ExitCode())
	}
	return err.Error()
}

// confirmRequestedPassword returns nil when a dba login with password,
// typed at csql's terminal prompt, is accepted, and an error otherwise; it
// changes nothing.
func confirmRequestedPassword(ctx context.Context, cli CLI, logger *slog.Logger, database, password string) error {
	terminal, ok := cli.(TerminalCLI)
	if !ok {
		return fmt.Errorf("check that the DBA password is the requested one: %w", errNoTerminal)
	}
	accepted, err := dbaPasswordAccepted(ctx, terminal, database, password)
	if err != nil {
		return fmt.Errorf("check that the DBA password is the requested one: %w", err)
	}
	if !accepted {
		return errPasswordNotRequested
	}
	logger.Info("DBA password is already the requested one; it was not changed", "event", eventDBAPasswordAlreadySet)
	return nil
}

// confirmDBAPassword checks that the stopped database's DBA password is
// password, and never sets one. It is for a slave seeded from its master's
// backup: the slave carries the master's password, and a password set on the
// master reaches the slaves by replication (POC-61, Q3), so a slave is never
// given one of its own. No password, or another one, is an error.
func confirmDBAPassword(ctx context.Context, cli CLI, logger *slog.Logger, database, password string) error {
	if !databaseNamePattern.MatchString(database) {
		return fmt.Errorf("database name %q is not a plain identifier", database)
	}
	refused, err := emptyLoginRefused(ctx, cli, database)
	if err != nil {
		return fmt.Errorf("check the DBA password: %w", err)
	}
	if !refused {
		return errors.New("the database has no DBA password, but one is requested; " +
			"a seeded slave is not given one of its own: its master's password reaches it by replication")
	}
	return confirmRequestedPassword(ctx, cli, logger, database, password)
}

// ApplyDBAPassword sets password as the DBA password of database, which must
// be stopped, unless it already has one. The value is fed to csql on stdin as
// `ALTER USER dba PASSWORD '...'`, never on an argv or in an environment
// variable, and neither csql's output of that run, nor the value, is logged
// or returned in an error.
//
// A dba login with the empty password that is refused means a password is
// set: a database whose step already ran, or a restored one, which keeps its
// source's password (POC-61, Q4). Nothing is changed then, and the step
// succeeds only when a login with password, typed at csql's prompt on a
// terminal, is accepted; another password is an error, since CUBRID does not
// change a password without the old one. Otherwise the
// statement runs, and it counts as applied only when csql printed no ERROR
// line, since csql exits 0 when a statement fed on stdin fails, and a dba
// login with the empty password is then refused (POC-61, Q1).
func ApplyDBAPassword(ctx context.Context, cli CLI, logger *slog.Logger, database, password string) error {
	// Reject before issuing even the first csql command: direct callers must
	// not bypass ReadDBAPassword's terminal-safety rule. The value is never
	// included in an error or log.
	if HasDBAPasswordControlByte([]byte(password)) {
		return errors.New("the DBA password holds an ASCII control byte")
	}
	if !databaseNamePattern.MatchString(database) {
		return fmt.Errorf("database name %q is not a plain identifier", database)
	}
	stdinCLI, ok := cli.(StdinCLI)
	if !ok {
		return fmt.Errorf("set the DBA password: %w", errNoStdin)
	}
	refused, err := emptyLoginRefused(ctx, cli, database)
	if err != nil {
		return fmt.Errorf("check the DBA password before setting it: %w", err)
	}
	if refused {
		return confirmRequestedPassword(ctx, cli, logger, database, password)
	}
	statement := "ALTER USER dba PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "';\n"
	out, err := stdinCLI.RunWithStdin(ctx, statement, "csql", "-u", "dba", "-S", database)
	if err != nil {
		return fmt.Errorf("csql failed while setting the DBA password: %w", err)
	}
	if strings.Contains(out, "ERROR") {
		return errors.New("csql reported an error while setting the DBA password; " +
			"its output is not shown because it may hold the statement")
	}
	refused, err = emptyLoginRefused(ctx, cli, database)
	if err != nil {
		return fmt.Errorf("check the DBA password after setting it: %w", err)
	}
	if !refused {
		return errors.New("csql set the DBA password without an error, " +
			"but a dba login with the empty password is still accepted")
	}
	logger.Info("Set the DBA password", "event", eventDBAPasswordApplied)
	return nil
}
