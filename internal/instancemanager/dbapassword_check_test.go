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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// callClientLogin is how checkCLI records the login the check runs: in
// client mode, through the running server, on a terminal.
const callClientLogin = "terminal csql -u dba -c SELECT 1 appdb@localhost"

// checkCLI stands in for CUBRID on a member whose server runs: `cubrid server
// status` lists appdb while running is set, and a csql login on a terminal is
// accepted only with current, the database's DBA password, as POC-61 Q5 saw
// in client mode.
type checkCLI struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	typed   []string
	running bool
	// statusErr is what `cubrid server status` fails with, when set.
	statusErr error
	current   string
	// terminal, when set, answers every login on a terminal.
	terminal *terminalAnswer
	// entered, when set, is closed once a login on a terminal started, and
	// that login then waits until release is closed or its context ends.
	entered chan struct{}
	release chan struct{}
}

func (c *checkCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	if call != "cubrid server status" {
		return "", errors.New("unexpected command " + call)
	}
	if c.statusErr != nil {
		return "", c.statusErr
	}
	if c.running {
		return serverStatusRunning, nil
	}
	return "@ cubrid server status\n", nil
}

func (c *checkCLI) RunOnTerminal(ctx context.Context, _, input, name string, args ...string) (string, bool, error) {
	c.mu.Lock()
	c.calls = append(c.calls, "terminal "+name+" "+strings.Join(args, " "))
	c.typed = append(c.typed, input)
	entered, release := c.entered, c.release
	c.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", true, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal != nil {
		return c.terminal.out, c.terminal.prompted, c.terminal.err
	}
	if input == c.current {
		return "\n" + outAccepted, true, nil
	}
	return "\n" + outRefused, true, exitStatus1(c.t)
}

func (c *checkCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// checkServer is a standalone member of appdb whose DBA password file is
// file ("" sets none), logging into logged.
func checkServer(cli CLI, file string, logged *bytes.Buffer) *Server {
	logger := slog.New(slog.NewJSONHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewServer(LoggingCLI{CLI: cli, Logger: logger}, "tok").
		WithLogger(logger).
		WithStandaloneDatabase(dbName).
		WithHAConfig(HAConfig{DBAPasswordFile: file})
}

// askDBAPassword sends GET /v1/dba-password with the token and decodes the
// answer, which must be 200.
func askDBAPassword(t *testing.T, s *Server) (DBAPasswordCheck, string) {
	t.Helper()
	rr := sendV1(s.Handler(), http.MethodGet, "/v1/dba-password", "", "Bearer tok", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/dba-password = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	var got DBAPasswordCheck
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return got, rr.Body.String()
}

// wantNothingLeaked checks that neither the answer nor the log holds the
// password or anything csql printed.
func wantNothingLeaked(t *testing.T, answer string, logged *bytes.Buffer) {
	t.Helper()
	for _, leak := range []string{testPassword, "Incorrect or missing", "row selected", "Unable to mount"} {
		if strings.Contains(answer, leak) {
			t.Errorf("the answer holds %q: %s", leak, answer)
		}
		if strings.Contains(logged.String(), leak) {
			t.Errorf("the log holds %q:\n%s", leak, logged.String())
		}
	}
}

// While the server runs, a dba login with the mounted value, typed at csql's
// prompt in client mode, tells whether the value is the database's DBA
// password. The check reads only: it runs the server status and one login,
// and the value is typed on the terminal, never put on an argv.
func TestDBAPasswordCheck_ALoginThroughTheRunningServerTellsWhetherTheValueMatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		want    DBAPasswordState
	}{
		{name: "the database's password", current: testPassword, want: DBAPasswordMatches},
		{name: "a changed password", current: testPassword + "-changed", want: DBAPasswordDiffers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := &checkCLI{t: t, running: true, current: tc.current}
			var logged bytes.Buffer
			got, answer := askDBAPassword(t, checkServer(cli, passwordFile(t, testPassword), &logged))
			if got.State != tc.want {
				t.Fatalf("state = %q (%s), want %q", got.State, answer, tc.want)
			}
			if tc.want == DBAPasswordDiffers && got.Reason == "" {
				t.Errorf("a Differs answer says nothing about why")
			}
			if calls := cli.recorded(); !slices.Equal(calls, []string{"cubrid server status", callClientLogin}) {
				t.Errorf("ran %q, want the server status and one login in client mode", calls)
			}
			if !slices.Equal(cli.typed, []string{testPassword}) {
				t.Errorf("typed %d lines, want the password once", len(cli.typed))
			}
			for _, call := range cli.recorded() {
				if strings.Contains(call, testPassword) {
					t.Errorf("the password is on an argv: %q", call)
				}
			}
			wantNothingLeaked(t, answer, &logged)
		})
	}
}

// When the check cannot tell, it answers Unknown with a reason, never Differs:
// the operator must not report a rotation it did not see. A check that
// cannot be made runs no login.
func TestDBAPasswordCheck_AnswersUnknownWhenItCannotTell(t *testing.T) {
	timeout := errors.New("signal: killed")
	for _, tc := range []struct {
		name  string
		cli   *checkCLI
		file  func(t *testing.T) string
		ha    bool
		says  string
		login bool
	}{
		{name: "no password file is set", cli: &checkCLI{running: true, current: testPassword},
			file: func(*testing.T) string { return "" }, says: "no DBA password"},
		{name: "the file is missing", cli: &checkCLI{running: true, current: testPassword},
			file: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }, says: "open the DBA password file"},
		{name: "the file is empty", cli: &checkCLI{running: true, current: testPassword},
			file: func(t *testing.T) string { return passwordFile(t, "") }, says: "empty"},
		// A value with a terminal control byte would be acted on by the
		// terminal instead of reaching csql, so it is never typed (#833).
		{name: "the value holds a terminal control byte", cli: &checkCLI{running: true, current: testPassword},
			file: func(t *testing.T) string { return passwordFile(t, testPassword+"\x03") }, says: "control byte"},
		{name: "the server is not running", cli: &checkCLI{current: testPassword},
			file: func(t *testing.T) string { return passwordFile(t, testPassword) }, says: "not running"},
		{name: "the server status fails", cli: &checkCLI{statusErr: timeout, current: testPassword},
			file: func(t *testing.T) string { return passwordFile(t, testPassword) }, says: "server status failed"},
		{name: "the member is an HA member", ha: true, cli: &checkCLI{running: true, current: testPassword},
			file: func(t *testing.T) string { return passwordFile(t, testPassword) }, says: "HA member"},
		{name: "csql does not prompt", login: true,
			cli:  &checkCLI{running: true, terminal: &terminalAnswer{err: errors.New("exit status 0")}},
			file: func(t *testing.T) string { return passwordFile(t, testPassword) }, says: "did not prompt"},
		{name: "csql fails for another reason after the prompt", login: true,
			cli: &checkCLI{running: true, terminal: &terminalAnswer{
				out: "\nERROR: Unable to mount disk volume.\n", prompted: true, err: errors.New("exit status 1")}},
			file: func(t *testing.T) string { return passwordFile(t, testPassword) }, says: "neither connected nor was refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cli.t = t
			var logged bytes.Buffer
			s := checkServer(tc.cli, tc.file(t), &logged)
			if tc.ha {
				s.standaloneDB = ""
			}
			got, answer := askDBAPassword(t, s)
			if got.State != DBAPasswordUnknown {
				t.Fatalf("state = %q (%s), want Unknown", got.State, answer)
			}
			if !strings.Contains(got.Reason, tc.says) {
				t.Errorf("reason = %q, want it to say %q", got.Reason, tc.says)
			}
			if logged := slices.Contains(tc.cli.recorded(), callClientLogin); logged != tc.login {
				t.Errorf("a login ran: %v, want %v (%q)", logged, tc.login, tc.cli.recorded())
			}
			wantNothingLeaked(t, answer, &logged)
		})
	}
}

// A login that hangs cannot hold the member: the check ends after its bound
// and answers Unknown.
func TestDBAPasswordCheck_IsBounded(t *testing.T) {
	cli := &checkCLI{t: t, running: true, current: testPassword,
		entered: make(chan struct{}), release: make(chan struct{})}
	var logged bytes.Buffer
	s := checkServer(cli, passwordFile(t, testPassword), &logged)
	s.dbaCheckBound = 50 * time.Millisecond
	start := time.Now()
	got, answer := askDBAPassword(t, s)
	if got.State != DBAPasswordUnknown || !strings.Contains(got.Reason, "deadline") {
		t.Fatalf("answer = %s, want Unknown because the check ran out of time", answer)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check took %v, past its bound", took)
	}
}

// Only one check runs at a time: a second one that arrives meanwhile answers
// Unknown at once instead of starting another login.
func TestDBAPasswordCheck_RunsOneAtATime(t *testing.T) {
	cli := &checkCLI{t: t, running: true, current: testPassword,
		entered: make(chan struct{}), release: make(chan struct{})}
	var logged bytes.Buffer
	s := checkServer(cli, passwordFile(t, testPassword), &logged)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- sendV1(s.Handler(), http.MethodGet, "/v1/dba-password", "", "Bearer tok", testRemoteAddr)
	}()
	select {
	case <-cli.entered:
	case rr := <-first:
		t.Fatalf("the first check answered %d (%s) before its login started", rr.Code, rr.Body.String())
	}
	got, _ := askDBAPassword(t, s)
	if got.State != DBAPasswordUnknown || !strings.Contains(got.Reason, "another check") {
		t.Errorf("a second check while one runs = %+v, want Unknown because another check runs", got)
	}
	close(cli.release)
	if rr := <-first; !strings.Contains(rr.Body.String(), `"state":"Matches"`) {
		t.Errorf("the first check = %d %s, want Matches", rr.Code, rr.Body.String())
	}
	logins := 0
	for _, call := range cli.recorded() {
		if call == callClientLogin {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("%d logins ran, want 1", logins)
	}
}
