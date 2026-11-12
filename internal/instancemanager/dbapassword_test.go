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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testPassword is distinctive, so that a leak of it is found by a plain
// substring search wherever it lands.
const testPassword = "Leak-Canary-7f3a9c"

const (
	callEmptyLogin = "csql -u dba -S appdb -c SELECT 1"
	callSetPass    = "csql -u dba -S appdb"
	outRefused     = "ERROR: Incorrect or missing password.\n"
)

// passwordCLI stands in for csql on a stopped database (docs/poc/RESULTS.md,
// POC-61): a login with the empty password is accepted until a password is
// set, then refused with "Incorrect or missing password" and exit 1; an
// ALTER USER fed on stdin exits 0 whether or not it applied.
type passwordCLI struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	stdins []string
	// set is whether the DBA user has a password.
	set bool
	// alterOut is what the ALTER run prints; an output with ERROR leaves the
	// password unset, as a failed statement does.
	alterOut string
	// alterIgnored makes the ALTER print success without setting anything.
	alterIgnored bool
	// loginOut and loginErr, when loginOut is set, are what every empty
	// login prints, whatever the password.
	loginOut string
	loginErr error
}

func (c *passwordCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if c.loginOut != "" {
		return c.loginOut, c.loginErr
	}
	if c.set {
		return outRefused, exitStatus1(c.t)
	}
	return "=== <Result of SELECT Command in Line 1> ===\n\n            1\n=============\n            1\n\n1 row selected.\n", nil
}

func (c *passwordCLI) RunWithStdin(_ context.Context, stdin, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	c.stdins = append(c.stdins, stdin)
	out := c.alterOut
	if out == "" {
		out = "Execute OK. (0.001 sec) Committed. (0.000 sec)\n"
	}
	if !c.alterIgnored && !strings.Contains(out, "ERROR") {
		c.set = true
	}
	return out, nil
}

func (c *passwordCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// applyForTest runs ApplyDBAPassword and returns its log and its error.
func applyForTest(t *testing.T, cli CLI, password string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := ApplyDBAPassword(context.Background(), cli, testLogger(&buf, slog.LevelDebug), dbName, password)
	return buf.String(), err
}

// The statement goes to csql on stdin. No argv carries the password: argv is
// readable by every process of the host (POC-61, Q1).
func TestApplyDBAPassword_FeedsTheStatementOnStdinOnly(t *testing.T) {
	cli := &passwordCLI{t: t}
	logs, err := applyForTest(t, cli, testPassword)
	if err != nil {
		t.Fatalf("ApplyDBAPassword: %v", err)
	}
	want := []string{callEmptyLogin, callSetPass, callEmptyLogin}
	if got := cli.recorded(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %q\nwant       %q", got, want)
	}
	if len(cli.stdins) != 1 || cli.stdins[0] != "ALTER USER dba PASSWORD '"+testPassword+"';\n" {
		t.Errorf("stdin = %q, want the ALTER USER statement once", cli.stdins)
	}
	for _, call := range cli.recorded() {
		if strings.Contains(call, testPassword) {
			t.Errorf("an argv carries the password: %q", call)
		}
	}
	if strings.Contains(logs, testPassword) {
		t.Errorf("the log carries the password:\n%s", logs)
	}
	if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordApplied); len(lines) != 1 {
		t.Errorf("want one %s line, got %v", eventDBAPasswordApplied, lines)
	}
}

// A single quote ends an SQL string literal; it is doubled, so the value is
// set as it was given and cannot end the statement early.
func TestApplyDBAPassword_DoublesSingleQuotes(t *testing.T) {
	cli := &passwordCLI{t: t}
	if _, err := applyForTest(t, cli, "it's'; DROP"); err != nil {
		t.Fatalf("ApplyDBAPassword: %v", err)
	}
	if want := "ALTER USER dba PASSWORD 'it''s''; DROP';\n"; len(cli.stdins) != 1 || cli.stdins[0] != want {
		t.Errorf("stdin = %q, want %q", cli.stdins, want)
	}
}

// csql exits 0 when a statement fed on stdin fails (POC-61, Q1): an ERROR line
// in its output is a failure whatever the exit code. Its output is never
// logged nor put in the error, since it may echo the statement.
func TestApplyDBAPassword_AnErrorLineIsAFailure(t *testing.T) {
	cli := &passwordCLI{t: t, alterOut: "In line 1, column 1,\nERROR: ALTER USER dba PASSWORD '" + testPassword + "' failed\n"}
	logs, err := applyForTest(t, cli, testPassword)
	if err == nil {
		t.Fatal("ApplyDBAPassword succeeded although csql printed an ERROR line")
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(logs, testPassword) {
		t.Errorf("csql's output reached the error or the log:\nerr=%v\n%s", err, logs)
	}
}

// csql reported nothing wrong, but an empty-password login is still
// accepted: the password is not set, and the step failed.
func TestApplyDBAPassword_AnEmptyLoginStillAcceptedIsAFailure(t *testing.T) {
	cli := &passwordCLI{t: t, alterIgnored: true}
	_, err := applyForTest(t, cli, testPassword)
	if err == nil || !strings.Contains(err.Error(), "still accepted") {
		t.Fatalf("err = %v, want a failure saying the empty login is still accepted", err)
	}
	if got := cli.recorded(); len(got) != 3 || got[2] != callEmptyLogin {
		t.Errorf("commands = %q, want the login checked again after the ALTER", got)
	}
}

// A database whose empty-password login is already refused has a password:
// a restart after the step, or a restored database, is left as it is.
func TestApplyDBAPassword_SkipsADatabaseThatHasAPassword(t *testing.T) {
	cli := &passwordCLI{t: t, set: true}
	logs, err := applyForTest(t, cli, testPassword)
	if err != nil {
		t.Fatalf("ApplyDBAPassword: %v", err)
	}
	if got := cli.recorded(); len(got) != 1 || got[0] != callEmptyLogin {
		t.Errorf("commands = %q, want only the empty-login check", got)
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
	if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordAlreadySet); len(lines) != 1 {
		t.Errorf("want one %s line, got %v", eventDBAPasswordAlreadySet, lines)
	}
}

// A login check that neither connected nor was refused for the password (the
// database is in use, csql is missing) says nothing about the password:
// nothing is applied and the step fails.
func TestApplyDBAPassword_AnUnansweredCheckAppliesNothing(t *testing.T) {
	cli := &passwordCLI{t: t, loginOut: "ERROR: Unable to mount disk volume.\n", loginErr: exitStatus1(t)}
	_, err := applyForTest(t, cli, testPassword)
	if err == nil {
		t.Fatal("ApplyDBAPassword succeeded on a check that did not answer")
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
}

// A CLI that cannot feed stdin applies nothing rather than fall back to argv.
func TestApplyDBAPassword_RefusesACLIWithoutStdin(t *testing.T) {
	cli := &argvOnlyCLI{}
	_, err := applyForTest(t, cli, testPassword)
	if err == nil {
		t.Fatal("ApplyDBAPassword succeeded without a way to feed stdin")
	}
	if len(cli.calls) != 0 {
		t.Errorf("commands ran: %q", cli.calls)
	}
}

// argvOnlyCLI is a CLI that cannot feed stdin.
type argvOnlyCLI struct{ calls []string }

func (c *argvOnlyCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	return "", nil
}

// The file holds the value and at most one trailing newline. A value that is
// empty, holds NUL, CR or LF, or is longer than the bound is refused, and no
// error carries the content of the file.
func TestReadDBAPassword(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", maxDBAPasswordBytes)
	tests := []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{name: "plain", content: testPassword, want: testPassword},
		{name: "one trailing newline", content: testPassword + "\n", want: testPassword},
		{name: "at the bound", content: long + "\n", want: long},
		{name: "empty file", content: "", wantErr: "is empty"},
		{name: "only a newline", content: "\n", wantErr: "is empty"},
		{name: "two trailing newlines", content: testPassword + "\n\n", wantErr: "line feed"},
		{name: "inner newline", content: testPassword + "\nsecond", wantErr: "line feed"},
		{name: "carriage return", content: testPassword + "\r\n", wantErr: "carriage return"},
		{name: "NUL", content: testPassword + "\x00", wantErr: "NUL"},
		{name: "over the bound", content: long + "x" + testPassword, wantErr: "longer than"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadDBAPassword(path)
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("ReadDBAPassword = %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), testPassword) || (tc.content != "" && strings.Contains(err.Error(), "xxxx")) {
				t.Errorf("the error carries the file's content: %v", err)
			}
			if got != "" {
				t.Errorf("a refused value was returned: %q", got)
			}
		})
	}
	if _, err := ReadDBAPassword(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was read")
	}
}

// fakeCSQL stands in for csql in a real child process: it records its argv,
// its environment and its stdin, and behaves as passwordCLI does.
const fakeCSQL = `#!/bin/bash
printf '%s\n' "$*" >> "${CAPTURE}/argv"
env >> "${CAPTURE}/env"
if [ "${@: -2:1}" = "-c" ]; then
  if [ -e "${CAPTURE}/set" ]; then echo "ERROR: Incorrect or missing password."; exit 1; fi
  echo "1 row selected."; exit 0
fi
cat >> "${CAPTURE}/stdin"
: > "${CAPTURE}/set"
echo "Execute OK."
`

// The value is found on csql's stdin and nowhere else: not on any argv, in
// the environment of any child, in the log or in an error, through the real
// command runner and its command log.
func TestApplyDBAPassword_LeaksTheValueNowhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in csql is a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	bin, capture := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "csql"), []byte(fakeCSQL), 0o700); err != nil { // #nosec G306 -- an executable test stand-in
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE", capture)
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DBAPasswordFileEnv, file)

	var buf bytes.Buffer
	// The log registers no secret, so nothing is redacted that was written.
	logger := NewLogger(&buf, LogSettings{Level: slog.LevelDebug, Database: dbName})
	cli := LoggingCLI{CLI: ExecCLI{Logger: logger}, Logger: logger}
	password, err := ReadDBAPassword(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyDBAPassword(context.Background(), cli, logger, dbName, password); err != nil {
		t.Fatalf("ApplyDBAPassword: %v", err)
	}
	// A second run finds the password set and changes nothing; its error, if
	// any, is checked too.
	err = ApplyDBAPassword(context.Background(), cli, logger, dbName, password)
	if err != nil && strings.Contains(err.Error(), testPassword) {
		t.Errorf("the error carries the password: %v", err)
	}

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(capture, name)) // #nosec G304 -- the test's own capture directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if stdin := read("stdin"); !strings.Contains(stdin, "PASSWORD '"+testPassword+"'") {
		t.Errorf("csql's stdin = %q, want the statement with the password", stdin)
	}
	for _, where := range []struct{ name, text string }{
		{"argv", read("argv")}, {"environment", read("env")}, {"log", buf.String()},
	} {
		if strings.Contains(where.text, testPassword) {
			t.Errorf("the password is in the %s:\n%s", where.name, where.text)
		}
	}
	if strings.Count(read("argv"), "\n") != 4 {
		t.Errorf("csql ran %d times, want 4 (check, set, check, check):\n%s",
			strings.Count(read("argv"), "\n"), read("argv"))
	}
	if err != nil {
		t.Errorf("the second run failed: %v", err)
	}
}

// ExecCLI feeds the given text on the command's stdin.
func TestExecCLI_RunWithStdinFeedsStdin(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat is not available")
	}
	out, err := ExecCLI{}.RunWithStdin(context.Background(), "fed\n", "cat")
	if err != nil || out != "fed\n" {
		t.Errorf("RunWithStdin = %q, %v; want the stdin back", out, err)
	}
}

// LoggingCLI logs a failed command run with stdin without its output: the
// output of csql may echo the statement it was fed.
func TestLoggingCLI_RunWithStdinLogsNoOutput(t *testing.T) {
	var buf bytes.Buffer
	cli := LoggingCLI{CLI: &passwordCLI{t: t, alterOut: "ERROR: " + testPassword}, Logger: testLogger(&buf, slog.LevelDebug)}
	out, _ := cli.RunWithStdin(context.Background(), "x", "csql", "-u", "dba")
	if !strings.Contains(out, testPassword) {
		t.Fatalf("output = %q, want it returned to the caller", out)
	}
	if strings.Contains(buf.String(), testPassword) {
		t.Errorf("the log carries the command's output:\n%s", buf.String())
	}
	if lines := withEvent(logLines(t, &buf), eventCommand); len(lines) != 1 {
		t.Errorf("want one command line, got %v", lines)
	}
	failing := LoggingCLI{CLI: stdinFailCLI{}, Logger: testLogger(&buf, slog.LevelDebug)}
	buf.Reset()
	if _, err := failing.RunWithStdin(context.Background(), "x", "csql"); err == nil {
		t.Fatal("want the failure returned")
	}
	if strings.Contains(buf.String(), testPassword) {
		t.Errorf("the log of a failed run carries its output:\n%s", buf.String())
	}
}

type stdinFailCLI struct{}

func (stdinFailCLI) Run(context.Context, string, ...string) (string, error) {
	return "", errors.New("unused")
}

func (stdinFailCLI) RunWithStdin(context.Context, string, string, ...string) (string, error) {
	return "echoed " + testPassword, errors.New("exit status 1")
}
