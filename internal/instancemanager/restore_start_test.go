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

// stepCLI records every command and fails the ones whose first two words are
// in failing.
type stepCLI struct {
	mu      sync.Mutex
	calls   []string
	failing map[string]bool
	// output is what a command whose first two words are the key prints.
	output map[string]string
}

func (c *stepCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if len(args) > 1 && c.failing[args[0]+" "+args[1]] {
		return "++ cubrid " + args[0] + " " + args[1] + ": fail", errors.New("exit status 1")
	}
	if len(args) > 1 {
		if out, ok := c.output[args[0]+" "+args[1]]; ok {
			return out, nil
		}
	}
	return "ok", nil
}

func (c *stepCLI) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// runRestoreOperation posts a restore to a server built around cli and returns
// the operation's terminal state.
// restoreMember says what kind of member the restore runs on.
type restoreMember struct {
	standalone bool
	haConf     string
}

// lastRestoreTarget is the database root of the latest runRestoreOperation.
var lastRestoreTarget string

func runRestoreOperation(t *testing.T, cli CLI, member restoreMember) Operation {
	t.Helper()
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	lastRestoreTarget = req.TargetDir
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithRestoreRoots(rootsFor(req))
	if member.standalone {
		srv = srv.WithStandaloneDatabase(dbName)
	}
	if member.haConf != "" {
		srv = srv.WithHAConfig(HAConfig{ConfPath: member.haConf})
	}
	h := srv.Handler()

	body, _ := json.Marshal(req)
	post := httptest.NewRequest(http.MethodPost, "/v1/restore/prepare", strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", "restore-1")
	post.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/restore/prepare = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	return pollUntilTerminal(t, h, op.ID)
}

const (
	// stepServerStart is how stepCLI names a server start.
	stepServerStart     = "server start"
	stepHeartbeatStatus = "heartbeat status"
	callServerStart     = "cubrid " + stepServerStart + " " + dbName
	callHeartbeatStart  = "cubrid heartbeat start"
)

// In a recovery bootstrap the entrypoint starts no database and runs only
// once, so the restore has to leave a standalone server running (#178).
func TestServer_Restore_StartsTheStandaloneServerBeforeCompleting(t *testing.T) {
	cli := &stepCLI{}
	final := runRestoreOperation(t, cli, restoreMember{standalone: true})
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := cli.recorded()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "cubrid restoredb ") || calls[1] != callServerStart {
		t.Errorf("calls = %q, want restoredb then %q", calls, callServerStart)
	}
	if _, err := os.Stat(filepath.Join(lastRestoreTarget, dbName, ownershipMarker)); !os.IsNotExist(err) {
		t.Errorf("the marker is still there after the restore completed (err=%v)", err)
	}
}

// A restore whose server does not start is not a completed restore.
func TestServer_Restore_FailedServerStartFailsTheOperation(t *testing.T) {
	cli := &stepCLI{failing: map[string]bool{stepServerStart: true}}
	final := runRestoreOperation(t, cli, restoreMember{standalone: true})
	if final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if !strings.Contains(final.FailureReason, "server start failed") {
		t.Errorf("failure reason = %q", final.FailureReason)
	}
}

// A member that is neither standalone nor configured for HA starts nothing.
func TestServer_Restore_StartsNothingWithoutHAConfiguration(t *testing.T) {
	cli := &stepCLI{}
	final := runRestoreOperation(t, cli, restoreMember{})
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	if calls := cli.recorded(); len(calls) != 1 || !strings.HasPrefix(calls[0], "cubrid restoredb ") {
		t.Errorf("calls = %q, want only restoredb", calls)
	}
}

// A seeded HA member registers the database under the member list and joins
// HA with one heartbeat start (docs/poc/RESULTS.md, POC-13).
func TestServer_Restore_SeededHAMemberJoinsHA(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}
	member := restoreMember{haConf: conf}
	final := runRestoreOperation(t, cli, member)
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := cli.recorded()
	want := []string{"cubrid restoredb ", "cubrid heartbeat status", callHeartbeatStart}
	if len(calls) != len(want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	for i := range want {
		if !strings.HasPrefix(calls[i], want[i]) {
			t.Errorf("call %d = %q, want prefix %q", i, calls[i], want[i])
		}
	}
	if entry := databasesTxtEntry(t, lastRestoreTarget, dbName); len(entry) < 3 || entry[2] != "demo-0:demo-1:demo-2" {
		t.Errorf("databases.txt entry = %q, want the member list as host", entry)
	}
}

// Heartbeat that already runs is not started a second time.
func TestServer_Restore_DoesNotRestartRunningHeartbeat(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &stepCLI{output: map[string]string{stepHeartbeatStatus: slaveOut}}
	runRestoreOperation(t, cli, restoreMember{haConf: conf})
	for _, call := range cli.recorded() {
		if call == callHeartbeatStart {
			t.Errorf("heartbeat was started although it reported a role: %q", cli.recorded())
		}
	}
}

// Heartbeat that reports the local node in a transition is running, and is
// not started a second time (docs/poc/RESULTS.md, POC-3 and POC-7).
func TestServer_Restore_DoesNotStartHeartbeatInTransition(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(conf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &stepCLI{output: map[string]string{stepHeartbeatStatus: transitionOut}}
	final := runRestoreOperation(t, cli, restoreMember{haConf: conf})
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	for _, call := range cli.recorded() {
		if call == callHeartbeatStart {
			t.Errorf("heartbeat was started although it reported a transition: %q", cli.recorded())
		}
	}
}
