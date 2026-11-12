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
	// outAccepted is what csql prints for a login whose SELECT 1 ran.
	outAccepted = "1 row selected.\n"
	// msgNoPrompt is part of the error of a login on a terminal that did
	// not prompt.
	msgNoPrompt = "did not prompt"
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
	// current is the DBA password the database has when set is true.
	current string
	// typed is every line typed at a terminal prompt.
	typed []string
	// terminal, when set, answers every login on a terminal.
	terminal *terminalAnswer
}

// terminalAnswer is what a login on a terminal returns.
type terminalAnswer struct {
	out      string
	prompted bool
	err      error
}

// callTerminalLogin is how passwordCLI records a login on a terminal.
const callTerminalLogin = "terminal csql -u dba -S appdb -c SELECT 1"

// RunOnTerminal stands in for csql on a pseudo-terminal (POC-61): with a
// password set it prompts, and the login is accepted only with that
// password; a wrong one prints "Incorrect or missing password" and exits 1.
func (c *passwordCLI) RunOnTerminal(_ context.Context, _, input, name string, args ...string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "terminal "+name+" "+strings.Join(args, " "))
	c.typed = append(c.typed, input)
	if c.terminal != nil {
		return c.terminal.out, c.terminal.prompted, c.terminal.err
	}
	if c.set && input == c.current {
		return "\n" + outAccepted, true, nil
	}
	return "\n" + outRefused, true, exitStatus1(c.t)
}

// alterPassword is the password an ALTER USER statement fed on stdin sets.
func alterPassword(statement string) string {
	value := strings.TrimSuffix(strings.TrimPrefix(statement, "ALTER USER dba PASSWORD '"), "';\n")
	return strings.ReplaceAll(value, "''", "'")
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
		c.set, c.current = true, alterPassword(stdin)
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

// Direct callers of ApplyDBAPassword must not bypass file validation. The
// operation refuses terminal-control input before any SQL command or
// authentication call, so no half-configured database is created.
func TestApplyDBAPassword_RejectsTerminalControlBeforeSQL(t *testing.T) {
	for name, value := range map[string]string{
		"interrupt": testPassword + "\x03",
		"eof":       testPassword + "\x04",
		"erase":     testPassword + "\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			cli := &passwordCLI{t: t}
			logs, err := applyForTest(t, cli, value)
			if err == nil || !strings.Contains(err.Error(), "control byte") {
				t.Fatalf("ApplyDBAPassword err = %v; want control-byte refusal", err)
			}
			if len(cli.recorded()) != 0 || len(cli.stdins) != 0 {
				t.Fatalf("the invalid value reached csql: %q / %q", cli.recorded(), cli.stdins)
			}
			if strings.Contains(err.Error(), value) || strings.Contains(logs, value) {
				t.Fatal("invalid password leaked into error or log")
			}
		})
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
// a restart after the step, or a restored database. It is left as it is once
// a login with the requested password, typed at csql's prompt on a terminal,
// is accepted.
func TestApplyDBAPassword_ConfirmsAPasswordAlreadySetToTheRequestedOne(t *testing.T) {
	cli := &passwordCLI{t: t, set: true, current: testPassword}
	logs, err := applyForTest(t, cli, testPassword)
	if err != nil {
		t.Fatalf("ApplyDBAPassword: %v", err)
	}
	want := []string{callEmptyLogin, callTerminalLogin}
	if got := cli.recorded(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %q\nwant       %q", got, want)
	}
	if len(cli.typed) != 1 || cli.typed[0] != testPassword {
		t.Errorf("typed = %q, want the requested password once", cli.typed)
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
	if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordAlreadySet); len(lines) != 1 {
		t.Errorf("want one %s line, got %v", eventDBAPasswordAlreadySet, lines)
	}
	if strings.Contains(logs, testPassword) {
		t.Errorf("the log carries the password:\n%s", logs)
	}
}

// A database that has a password other than the requested one, such as a
// database restored from a backup of a database with another password, is
// not given the requested one: CUBRID does not change a password without the
// old one (POC-61, Q4). The step fails and says why, never with either value.
func TestApplyDBAPassword_RefusesAPasswordThatIsNotTheRequestedOne(t *testing.T) {
	const other = otherSourcePassword
	cli := &passwordCLI{t: t, set: true, current: other}
	logs, err := applyForTest(t, cli, testPassword)
	if err == nil {
		t.Fatal("ApplyDBAPassword succeeded on a database whose password is not the requested one")
	}
	if !strings.Contains(err.Error(), "not the requested") {
		t.Errorf("err = %v, want it to say the password is not the requested one", err)
	}
	for _, value := range []string{testPassword, other} {
		if strings.Contains(err.Error(), value) || strings.Contains(logs, value) {
			t.Errorf("a password reached the error or the log:\nerr=%v\n%s", err, logs)
		}
	}
	if len(cli.stdins) != 0 {
		t.Errorf("an ALTER was fed: %q", cli.stdins)
	}
	if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordAlreadySet); len(lines) != 0 {
		t.Errorf("a refused password was logged as already set: %v", lines)
	}
}

// A login on a terminal that neither connected nor was refused for its
// password says nothing about the password: the step fails, and does not
// report the password as set.
func TestApplyDBAPassword_AnUnansweredTerminalLoginIsAFailure(t *testing.T) {
	for name, answer := range map[string]terminalAnswer{
		"no prompt":       {out: "", prompted: false, err: context.DeadlineExceeded},
		"database in use": {out: "\nERROR: Unable to mount disk volume.\n", prompted: true, err: errors.New("exit status 1")},
	} {
		t.Run(name, func(t *testing.T) {
			cli := &passwordCLI{t: t, set: true, current: testPassword, terminal: &answer}
			logs, err := applyForTest(t, cli, testPassword)
			if err == nil {
				t.Fatal("ApplyDBAPassword succeeded on a login that did not answer")
			}
			if lines := withEvent(logLines(t, bytes.NewBufferString(logs)), eventDBAPasswordAlreadySet); len(lines) != 0 {
				t.Errorf("an unanswered login was logged as already set: %v", lines)
			}
		})
	}
}

// A CLI that cannot run csql on a terminal cannot confirm a password that is
// already set; the step fails instead of passing over it.
func TestApplyDBAPassword_RefusesACLIWithoutATerminal(t *testing.T) {
	inner := &passwordCLI{t: t, set: true, current: testPassword}
	_, err := applyForTest(t, stdinOnly{inner}, testPassword)
	if err == nil {
		t.Fatal("ApplyDBAPassword succeeded without a way to confirm the password")
	}
}

// stdinOnly hides RunOnTerminal from the CLI it wraps.
type stdinOnly struct{ StdinCLI }

// dbaPasswordAccepted reads csql's answer on the terminal: accepted after
// the prompt with exit 0, refused when csql says the password is wrong, and
// an error for anything else. No error carries what csql printed, since a
// terminal may echo what was typed.
func TestDBAPasswordAccepted(t *testing.T) {
	tests := []struct {
		name     string
		answer   terminalAnswer
		accepted bool
		wantErr  string
	}{
		{name: "accepted", answer: terminalAnswer{out: "\n" + outAccepted, prompted: true}, accepted: true},
		{name: "refused", answer: terminalAnswer{out: "\n" + outRefused, prompted: true, err: errors.New("exit status 1")}},
		{name: "no prompt, refused", answer: terminalAnswer{out: outRefused, err: errors.New("exit status 1")}, wantErr: msgNoPrompt},
		{name: "no prompt, ended", answer: terminalAnswer{err: context.DeadlineExceeded}, wantErr: msgNoPrompt},
		{name: "no prompt, exit 0", answer: terminalAnswer{out: outAccepted}, wantErr: msgNoPrompt},
		{name: "other failure", answer: terminalAnswer{out: "\nERROR: Unable to mount " + testPassword + "\n", prompted: true,
			err: errors.New("exit status 1")}, wantErr: "neither"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cli := &passwordCLI{t: t, terminal: &tc.answer}
			accepted, err := dbaPasswordAccepted(context.Background(), cli, dbName, testPassword)
			if tc.wantErr == "" {
				if err != nil || accepted != tc.accepted {
					t.Fatalf("dbaPasswordAccepted = %v, %v; want %v", accepted, err, tc.accepted)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("dbaPasswordAccepted = %v, %v; want an error naming %q", accepted, err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), "mount")) {
				t.Errorf("the error carries csql's output: %v", err)
			}
			if got := cli.recorded(); len(got) != 1 || got[0] != callTerminalLogin {
				t.Errorf("commands = %q, want one login on a terminal", got)
			}
			for _, call := range cli.recorded() {
				if strings.Contains(call, testPassword) {
					t.Errorf("an argv carries the password: %q", call)
				}
			}
		})
	}
}

// A slave seeded from its master's backup carries the master's password,
// which replicates and is never set on the slave (POC-61, Q3). Its password
// is only confirmed: the requested one passes, and no password or another
// one fails without any statement fed.
func TestConfirmDBAPassword(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     bool
		current string
		ok      bool
	}{
		{name: "requested", set: true, current: testPassword, ok: true},
		{name: "another", set: true, current: otherSourcePassword},
		{name: "none", set: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := &passwordCLI{t: t, set: tc.set, current: tc.current}
			var buf bytes.Buffer
			err := confirmDBAPassword(context.Background(), cli, testLogger(&buf, slog.LevelDebug), dbName, testPassword)
			if (err == nil) != tc.ok {
				t.Fatalf("confirmDBAPassword = %v, want ok=%v", err, tc.ok)
			}
			if len(cli.stdins) != 0 {
				t.Errorf("a statement was fed on a seeded slave: %q", cli.stdins)
			}
			if err != nil && strings.Contains(err.Error(), testPassword) || strings.Contains(buf.String(), testPassword) {
				t.Errorf("the password reached the error or the log: %v\n%s", err, buf.String())
			}
		})
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
// empty, holds NUL/CR/LF or terminal control bytes, or exceeds the bound is refused, and no
// error carries the content of the file.
func TestReadDBAPassword(t *testing.T) {
	const wantTerminalControlByte = "control byte"
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
		{name: "tab", content: testPassword + "\t", wantErr: wantTerminalControlByte},
		{name: "terminal interrupt", content: testPassword + "\x03", wantErr: wantTerminalControlByte},
		{name: "terminal EOF", content: testPassword + "\x04", wantErr: wantTerminalControlByte},
		{name: "terminal flow control", content: testPassword + "\x13", wantErr: wantTerminalControlByte},
		{name: "terminal erase", content: testPassword + "\x7f", wantErr: wantTerminalControlByte},
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
// its environment and its stdin, and behaves as passwordCLI does. On a
// terminal it prompts as csql does, reads one line, and accepts the login
// only when that line is the password set (POC-61).
const fakeCSQL = `#!/bin/bash
printf '%s\n' "$*" >> "${CAPTURE}/argv"
env >> "${CAPTURE}/env"
if [ "${@: -2:1}" = "-c" ]; then
  if [ -t 0 ]; then
    printf 'Enter Password : '
    IFS= read -r typed
    echo
    if [ -e "${CAPTURE}/set" ] && [ "${typed}" = "$(cat "${CAPTURE}/set")" ]; then echo "1 row selected."; exit 0; fi
    echo "ERROR: Incorrect or missing password."; exit 1
  fi
  if [ -e "${CAPTURE}/set" ]; then echo "ERROR: Incorrect or missing password."; exit 1; fi
  echo "1 row selected."; exit 0
fi
statement=$(cat)
printf '%s\n' "${statement}" >> "${CAPTURE}/stdin"
value=${statement#"ALTER USER dba PASSWORD '"}
printf '%s' "${value%"';"}" > "${CAPTURE}/set"
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
	// A second run finds the password set, confirms it by a login on a
	// terminal and changes nothing; its error, if any, is checked too.
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
	if strings.Count(read("argv"), "\n") != 5 {
		t.Errorf("csql ran %d times, want 5 (check, set, check, check, login on a terminal):\n%s",
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

// LoggingCLI logs a login on a terminal as a command, without its output,
// which may hold what was typed.
func TestLoggingCLI_RunOnTerminalLogsNoOutput(t *testing.T) {
	var buf bytes.Buffer
	inner := &passwordCLI{t: t, terminal: &terminalAnswer{out: "echoed " + testPassword, prompted: true,
		err: errors.New("exit status 1")}}
	cli := LoggingCLI{CLI: inner, Logger: testLogger(&buf, slog.LevelDebug)}
	out, prompted, err := cli.RunOnTerminal(context.Background(), csqlPasswordPrompt, testPassword, "csql",
		emptyLoginArgs(dbName)...)
	if !prompted || err == nil || !strings.Contains(out, testPassword) {
		t.Fatalf("RunOnTerminal = %q, %v, %v; want the inner answer returned to the caller", out, prompted, err)
	}
	if strings.Contains(buf.String(), testPassword) {
		t.Errorf("the log carries the output or the input:\n%s", buf.String())
	}
	if lines := withEvent(logLines(t, &buf), eventCommand); len(lines) != 1 {
		t.Errorf("want one command line, got %v", lines)
	}
	if _, _, err := (LoggingCLI{CLI: &argvOnlyCLI{}, Logger: testLogger(&buf, slog.LevelDebug)}).RunOnTerminal(
		context.Background(), csqlPasswordPrompt, testPassword, "csql"); err == nil {
		t.Error("a CLI without a terminal was reported as having run the command")
	}
}
