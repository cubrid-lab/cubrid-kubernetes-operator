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
}

func (c *haCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	command := name + " " + strings.Join(args, " ")
	if len(args) > 1 && args[0] == "heartbeat" && args[1] == "status" {
		if c.running {
			return masterOut, nil
		}
		return "++ cubrid heartbeat status: fail", errors.New("exit status 1")
	}
	c.calls = append(c.calls, command)
	if len(args) > 0 && args[0] == c.failing {
		return "++ cubrid " + args[0] + ": fail", errors.New("exit status 1")
	}
	switch {
	case len(args) > 0 && args[0] == "createdb":
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
	databases string
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
	cli := &haCLI{databases: databases}
	h := NewServer(cli, "tok").
		WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: databases, Staging: filepath.Join(base, "staging")}).
		WithHAConfig(HAConfig{ConfPath: conf, VolumeSize: "64M"}).
		Handler()
	return &haFixture{t: t, cli: cli, databases: databases, handler: h}
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
		"cubrid createdb --db-volume-size=64M --server-name=demo-0:demo-1:demo-2 -F " +
			filepath.Join(f.databases, dbName) + " " + dbName + " en_US",
		callHeartbeatStart,
	}
	if got := f.cli.recorded(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %q\nwant       %q", got, want)
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
