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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testHAConf = "[common]\nha_node_list=cubrid@demo-0:demo-1:demo-2\nha_db_list=appdb\nha_port_id=59901\n"

// haCLI stands in for the CUBRID CLI of an HA member: createdb registers the
// database as the real command does, and heartbeat status reports a role once
// heartbeat was started.
type haCLI struct {
	mu        sync.Mutex
	calls     []string
	databases string
	failing   string // first word after "cubrid" of the command that fails
	running   bool   // heartbeat already running
	// status and statusErr, when either is set, are what heartbeat status
	// answers before heartbeat was started.
	status    string
	statusErr error
	// exit is the error of a command that ran to its end and exited 1.
	exit error
	// dbaPasswordSet is whether the database's DBA user has a password: an
	// empty csql login is then refused (docs/poc/RESULTS.md, POC-61).
	dbaPasswordSet bool
	// alterFails makes the ALTER USER fed on stdin print an ERROR line and
	// set nothing, with exit 0 as csql does.
	alterFails bool
	stdins     []string
}

func (c *haCLI) RunWithStdin(_ context.Context, stdin, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	c.stdins = append(c.stdins, stdin)
	if c.alterFails {
		return "ERROR: the statement failed\n", nil
	}
	c.dbaPasswordSet = true
	return "Execute OK.\n", nil
}

func (c *haCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	command := name + " " + strings.Join(args, " ")
	if len(args) > 1 && args[0] == "heartbeat" && args[1] == "status" {
		if c.running {
			return masterOut, nil
		}
		if c.status != "" || c.statusErr != nil {
			return c.status, c.statusErr
		}
		// What heartbeat status printed while heartbeat was never started
		// (docs/poc/RESULTS.md, POC-26, shape B).
		return noNodeOut, c.exit

	}
	c.calls = append(c.calls, command)
	if len(args) > 0 && args[0] == c.failing {
		return "++ cubrid " + args[0] + ": fail", errors.New("exit status 1")
	}
	switch {
	case name == "csql":
		if c.dbaPasswordSet {
			return outRefused, c.exit
		}
		return "1 row selected.\n", nil
	case len(args) > 0 && args[0] == "createdb":
		c.dbaPasswordSet = false
		line := dbName + "\t\t" + filepath.Join(c.databases, dbName) + "\thosts\n"
		_ = os.WriteFile(filepath.Join(c.databases, databasesTxt), []byte(line), 0o600)
	case len(args) > 1 && args[0] == "heartbeat" && args[1] == "start":
		c.running = true
	}
	return "ok", nil
}

func (c *haCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

type haFixture struct {
	t         *testing.T
	cli       *haCLI
	store     *OperationStore
	databases string
	srv       *Server
	handler   http.Handler
}

func newHAFixture(t *testing.T) *haFixture {
	t.Helper()
	base := t.TempDir()
	databases := filepath.Join(base, "databases")
	conf := filepath.Join(base, "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewOperationStore(filepath.Join(base, "operations"))
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := &haCLI{databases: databases, exit: exitStatus1(t)}
	srv := NewServer(cli, "tok").
		WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: databases, Staging: filepath.Join(base, "staging")}).
		WithHAConfig(HAConfig{ConfPath: conf, VolumeSize: "64M", LogVolumeSize: "32M"})
	srv.heartbeatProcesses, srv.heartbeatSettle = processTable(), 0
	return &haFixture{t: t, cli: cli, store: store, databases: databases, srv: srv, handler: srv.Handler()}
}

func (f *haFixture) post(key string) *httptest.ResponseRecorder {
	f.t.Helper()
	body, _ := json.Marshal(HABootstrapRequest{Database: dbName})
	req := httptest.NewRequest(http.MethodPost, "/v1/ha/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", key)
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, req)
	return rr
}

func (f *haFixture) run(key string) Operation {
	f.t.Helper()
	rr := f.post(key)
	if rr.Code != http.StatusAccepted {
		f.t.Fatalf("/v1/ha/bootstrap = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		f.t.Fatal(err)
	}
	return pollUntilTerminal(f.t, f.handler, op.ID)
}

// The first database is created with the HA member list as its host and its
// volumes below the database root, then heartbeat is started once
// (docs/poc/RESULTS.md, POC-13).
func TestHABootstrap_CreatesTheDatabaseThenStartsHeartbeat(t *testing.T) {
	f := newHAFixture(t)
	final := f.run("boot-1")
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	want := []string{
		"cubrid createdb --db-volume-size=64M --log-volume-size=32M --server-name=demo-0:demo-1:demo-2 -F " +
			filepath.Join(f.databases, dbName) + " " + dbName + " en_US",
		callHeartbeatStart,
	}
	if got := f.cli.recorded(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %q\nwant       %q", got, want)
	}
}

// withDBAPassword gives the manager a DBA password file holding value.
func (f *haFixture) withDBAPassword(value string) {
	f.t.Helper()
	file := filepath.Join(f.t.TempDir(), "dba-password")
	if err := os.WriteFile(file, []byte(value+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	ha := f.srv.ha
	ha.DBAPasswordFile = file
	f.srv.WithHAConfig(ha)
}

// wantNoPasswordInOperations checks that no operation record carries value.
func (f *haFixture) wantNoPasswordInOperations(value string) {
	f.t.Helper()
	dir := filepath.Join(filepath.Dir(f.databases), "operations")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- the test's own operation store
		if err != nil {
			return err
		}
		if strings.Contains(string(data), value) {
			f.t.Errorf("operation record %s carries the password", path)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// With a DBA password file, the bootstrap member sets the password right
// after its createdb and before heartbeat starts, while the database still
// carries the operation's marker; a slave receives it by replication
// (docs/poc/RESULTS.md, POC-61, Q3).
func TestHABootstrap_SetsTheDBAPasswordAfterCreatedbBeforeHeartbeatStart(t *testing.T) {
	f := newHAFixture(t)
	f.withDBAPassword(testPassword)
	final := f.run("boot-1")
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	got := f.cli.recorded()
	want := []string{"cubrid createdb", callEmptyLogin, callSetPass, callEmptyLogin, callHeartbeatStart}
	if len(got) != len(want) {
		t.Fatalf("commands = %q\nwant       %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) || (want[i] == callSetPass && got[i] != callSetPass) {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
		if strings.Contains(got[i], testPassword) {
			t.Errorf("an argv carries the password: %q", got[i])
		}
	}
	if len(f.cli.stdins) != 1 || !strings.Contains(f.cli.stdins[0], "'"+testPassword+"'") {
		t.Errorf("stdin = %q, want the statement once", f.cli.stdins)
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName, ".im-operation")); !os.IsNotExist(err) {
		t.Errorf("the ownership marker is still there after a complete bootstrap (err=%v)", err)
	}
	f.wantNoPasswordInOperations(testPassword)
}

// A failed password step fails the bootstrap: heartbeat is not started, and
// the marker stays, so the entrypoint never starts that database and the next
// attempt removes it and creates it again (reclaimIncomplete).
func TestHABootstrap_AFailedPasswordStepStartsNothing(t *testing.T) {
	f := newHAFixture(t)
	f.withDBAPassword(testPassword)
	f.cli.alterFails = true
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "DBA password") {
		t.Fatalf("final = %s (%s), want Failed naming the DBA password", final.State, final.FailureReason)
	}
	if strings.Contains(final.FailureReason, testPassword) {
		t.Errorf("the failure reason carries the password: %q", final.FailureReason)
	}
	for _, call := range f.cli.recorded() {
		if strings.Contains(call, "heartbeat start") {
			t.Errorf("heartbeat was started without the password: %q", f.cli.recorded())
		}
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName, ".im-operation")); err != nil {
		t.Errorf("the ownership marker is gone after a failed password step: %v", err)
	}

	f.cli.alterFails = false
	before := len(f.cli.recorded())
	if again := f.run("boot-2"); again.State != OpCompleted {
		t.Fatalf("retry: state = %s (%s), want Completed", again.State, again.FailureReason)
	}
	got := f.cli.recorded()[before:]
	if len(got) != 5 || !strings.HasPrefix(got[0], "cubrid createdb") || got[2] != callSetPass || got[4] != callHeartbeatStart {
		t.Errorf("retry commands = %q, want createdb, the password step, heartbeat start", got)
	}
	f.wantNoPasswordInOperations(testPassword)
}

// A retry after the password step, here after a failed heartbeat start, finds
// the database registered and creates nothing; the password is not set twice.
func TestHABootstrap_ARetryAfterThePasswordStepDoesNotSetItAgain(t *testing.T) {
	f := newHAFixture(t)
	f.withDBAPassword(testPassword)
	f.cli.failing = heartbeatCmd
	if final := f.run("boot-1"); final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed at heartbeat start", final.State)
	}
	f.cli.failing = ""
	if again := f.run("boot-2"); again.State != OpCompleted {
		t.Fatalf("retry: state = %s (%s), want Completed", again.State, again.FailureReason)
	}
	if len(f.cli.stdins) != 1 {
		t.Errorf("the password was fed %d times, want once", len(f.cli.stdins))
	}
}

// A password file that cannot be used fails the bootstrap before createdb:
// nothing is created that would then wait for a password.
func TestHABootstrap_AnUnusablePasswordFileCreatesNothing(t *testing.T) {
	f := newHAFixture(t)
	f.withDBAPassword("")
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "empty") {
		t.Fatalf("final = %s (%s), want Failed for an empty password", final.State, final.FailureReason)
	}
	if got := f.cli.recorded(); len(got) != 0 {
		t.Errorf("commands ran: %q", got)
	}
}

// A manager given no log volume size, as by an operator that sets none,
// leaves the size to the image's cubrid.conf rather than passing an empty
// flag (#666).
func TestHABootstrap_LeavesTheLogVolumeSizeToTheImageWhenNoneIsSet(t *testing.T) {
	f := newHAFixture(t)
	f.srv.WithHAConfig(HAConfig{ConfPath: f.srv.ha.ConfPath, VolumeSize: f.srv.ha.VolumeSize})
	if final := f.run("boot-1"); final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	got := f.cli.recorded()
	if len(got) == 0 || !strings.HasPrefix(got[0], "cubrid createdb ") {
		t.Fatalf("commands = %q, want createdb first", got)
	}
	if strings.Contains(got[0], "--log-volume-size") {
		t.Errorf("createdb = %q, want no --log-volume-size", got[0])
	}
}

// Asking again, with the same key or a new one, creates nothing and does not
// issue a second heartbeat start.
func TestHABootstrap_IsNotRepeated(t *testing.T) {
	f := newHAFixture(t)
	f.run("boot-1")
	before := len(f.cli.recorded())

	if again := f.run("boot-1"); again.State != OpCompleted {
		t.Fatalf("same key: state = %s", again.State)
	}
	if other := f.run("boot-2"); other.State != OpCompleted {
		t.Fatalf("new key: state = %s (%s)", other.State, other.FailureReason)
	}
	if got := f.cli.recorded(); len(got) != before {
		t.Errorf("a repeated bootstrap ran more commands: %q", got[before:])
	}
}

// A directory without a registration is an interrupted createdb or foreign
// data: the bootstrap stops instead of creating over it or removing it.
func TestHABootstrap_RefusesAnUnregisteredDatabaseDirectory(t *testing.T) {
	f := newHAFixture(t)
	leftover := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(leftover, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, dbName+"_vinf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "refusing") {
		t.Fatalf("final = %s (%s), want Failed with a refusal", final.State, final.FailureReason)
	}
	if got := f.cli.recorded(); len(got) != 0 {
		t.Errorf("commands ran against a directory that was already there: %q", got)
	}
	if _, err := os.Stat(filepath.Join(leftover, dbName+"_vinf")); err != nil {
		t.Errorf("the existing file was touched: %v", err)
	}
}

// A failed createdb leaves no directory behind, so a retry is not refused,
// and heartbeat is not started on a member without a database.
func TestHABootstrap_FailedCreatedbStartsNothing(t *testing.T) {
	f := newHAFixture(t)
	f.cli.failing = "createdb"
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "createdb failed") {
		t.Fatalf("final = %s (%s)", final.State, final.FailureReason)
	}
	for _, call := range f.cli.recorded() {
		if strings.Contains(call, "heartbeat start") {
			t.Errorf("heartbeat was started without a database: %q", f.cli.recorded())
		}
	}
	if _, err := os.Stat(filepath.Join(f.databases, dbName)); !os.IsNotExist(err) {
		t.Errorf("the directory of the failed createdb is still there (err=%v)", err)
	}
}

func TestHABootstrap_RefusesAStandaloneMember(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &haCLI{databases: t.TempDir()}
	f := &haFixture{t: t, cli: cli, handler: NewServer(cli, "tok").WithOperationStore(store).
		WithStandaloneDatabase(dbName).Handler()}
	if rr := f.post("boot-1"); rr.Code != http.StatusConflict {
		t.Errorf("/v1/ha/bootstrap on a standalone member = %d, want 409", rr.Code)
	}
	if got := cli.recorded(); len(got) != 0 {
		t.Errorf("commands ran: %q", got)
	}
}

func TestHAHosts(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, "conf")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got, err := haHosts(write(testHAConf)); err != nil || got != "demo-0:demo-1:demo-2" {
		t.Errorf("haHosts = %q, %v", got, err)
	}
	for _, bad := range []string{
		"", "[common]\nha_db_list=appdb\n", "ha_node_list=cubrid@\n",
		"ha_node_list=cubrid@a;rm\n", "#ha_node_list=cubrid@a:b\n",
	} {
		if got, err := haHosts(write(bad)); err == nil {
			t.Errorf("haHosts(%q) = %q, want an error", bad, got)
		}
	}
	if _, err := haHosts(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file must be an error")
	}
}

// Heartbeat that reports the local node in a transition, or in any other
// state that is not a confirmed role, is running: the bootstrap does not start
// it again, which would flip the activation off (docs/poc/RESULTS.md, POC-3
// and POC-7), and does not report Completed either.
func TestHABootstrap_DoesNotStartHeartbeatInTransition(t *testing.T) {
	for _, state := range []string{"to-be-master", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f := newHAFixture(t)
			f.cli.status = strings.Replace(transitionOut, "state to-be-master", "state "+state, 1)
			final := f.run("boot-1")
			if final.State != OpFailed || !strings.Contains(final.FailureReason, "'"+state+"'") {
				t.Fatalf("final = %s (%s), want Failed naming state %q", final.State, final.FailureReason, state)
			}
			for _, call := range f.cli.recorded() {
				if call == callHeartbeatStart {
					t.Errorf("heartbeat was started although it reported state %s: %q", state, f.cli.recorded())
				}
			}
		})
	}
}

// Heartbeat that already reports a confirmed role completes the bootstrap
// without a second start.
func TestHABootstrap_CompletesOnRunningHeartbeat(t *testing.T) {
	f := newHAFixture(t)
	f.cli.status = masterOut
	final := f.run("boot-1")
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	for _, call := range f.cli.recorded() {
		if call == callHeartbeatStart {
			t.Errorf("heartbeat was started although it reported a transition: %q", f.cli.recorded())
		}
	}
}

// A heartbeat status that did not answer says nothing about heartbeat: the
// bootstrap starts nothing and fails, so that the operator asks again.
func TestHABootstrap_DoesNotStartHeartbeatWhenStatusDidNotAnswer(t *testing.T) {
	f := newHAFixture(t)
	f.cli.statusErr = context.DeadlineExceeded
	final := f.run("boot-1")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "heartbeat status") {
		t.Fatalf("final = %s (%s), want Failed naming heartbeat status", final.State, final.FailureReason)
	}
	for _, call := range f.cli.recorded() {
		if call == callHeartbeatStart {
			t.Errorf("heartbeat was started although its status did not answer: %q", f.cli.recorded())
		}
	}
}
