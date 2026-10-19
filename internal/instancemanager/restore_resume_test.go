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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restoredVolume is the file the test restoredb leaves in the database
// directory, standing in for the restored volumes.
const restoredVolume = dbName + "_vinf"

// volumeCLI is a stepCLI whose restoredb leaves a volume in the database
// directory and then runs afterRestore.
type volumeCLI struct {
	stepCLI
	databases    string
	afterRestore func()
}

func (c *volumeCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := c.stepCLI.Run(ctx, name, args...)
	if err == nil && len(args) > 0 && args[0] == "restoredb" {
		if werr := os.WriteFile(filepath.Join(c.databases, dbName, restoredVolume), []byte("restored"), 0o600); werr != nil {
			return "", werr
		}
		if c.afterRestore != nil {
			c.afterRestore()
		}
	}
	return out, err
}

// resumeFixture is a standalone member, or an HA member when haConf is set,
// whose manager can be restarted on the same operation store and database
// root.
type resumeFixture struct {
	t      *testing.T
	haConf string
	// processes is the process table the manager reads; nil is one without
	// any CUBRID process.
	processes func() (map[string]bool, error)
	objects   ObjectStore
	req       RestoreRequest
	storeDir  string
	databases string
	server    *Server
	handler   http.Handler
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	return &resumeFixture{t: t, objects: objects, req: req, storeDir: t.TempDir(), databases: req.TargetDir}
}

// start starts (or restarts) the manager with cli, on the fixture's store.
func (f *resumeFixture) start(cli CLI) {
	f.t.Helper()
	store, err := NewOperationStore(f.storeDir)
	if err != nil {
		f.t.Fatalf("NewOperationStore: %v", err)
	}
	f.server = NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(f.objects).
		WithRestoreRoots(rootsFor(f.req))
	if f.haConf != "" {
		f.server = f.server.WithHAConfig(HAConfig{ConfPath: f.haConf})
	} else {
		f.server = f.server.WithStandaloneDatabase(dbName)
	}
	f.server.heartbeatProcesses, f.server.heartbeatSettle = processTable(), 0
	if f.processes != nil {
		f.server.heartbeatProcesses = f.processes
	}
	f.handler = f.server.Handler()
}

// post submits the fixture's restore request under key and returns the
// operation it started.
func (f *resumeFixture) post(key string) Operation {
	f.t.Helper()
	body, _ := json.Marshal(f.req)
	post := httptest.NewRequest(http.MethodPost, "/v1/restore/prepare", strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", key)
	post.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, post)
	if rr.Code != http.StatusAccepted {
		f.t.Fatalf("/v1/restore/prepare = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		f.t.Fatal(err)
	}
	return op
}

func (f *resumeFixture) run(key string) Operation {
	f.t.Helper()
	return pollUntilSettled(f.t, f.server, f.handler, f.post(key).ID)
}

func (f *resumeFixture) marker() string {
	data, _ := os.ReadFile(filepath.Join(f.databases, dbName, ownershipMarker))
	return strings.TrimSpace(string(data))
}

func (f *resumeFixture) wantRestoredDataKept() {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.databases, dbName, restoredVolume)); err != nil {
		f.t.Errorf("the restored data was not kept: %v", err)
	}
	if entry := databasesTxtEntry(f.t, f.databases, dbName); len(entry) == 0 {
		f.t.Errorf("the restored database is no longer registered")
	}
}

func count(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// A restore whose server does not start keeps its ownership of the restored
// database, and the next attempt starts that database instead of restoring
// over it or refusing it as someone else's data (#267).
func TestServer_Restore_RetryStartsTheDataAFailedStartLeft(t *testing.T) {
	f := newResumeFixture(t)
	first := &volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases}
	f.start(first)
	failed := f.run("restore-1")
	if failed.State != OpFailed {
		t.Fatalf("first attempt = %s, want Failed", failed.State)
	}
	if got := f.marker(); got != failed.ID {
		t.Fatalf("marker after a failed start = %q, want the operation %q", got, failed.ID)
	}
	f.wantRestoredDataKept()

	// The manager restarts and the operator retries under the next key.
	second := &volumeCLI{databases: f.databases}
	f.start(second)
	final := f.run("restore-2")
	if final.State != OpCompleted {
		t.Fatalf("retry = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := second.recorded()
	if len(calls) != 1 || calls[0] != callServerStart {
		t.Errorf("retry calls = %q, want only %q (no second restoredb)", calls, callServerStart)
	}
	if final.Artifact == nil || final.Artifact.Database != dbName || final.Artifact.ManifestURI == "" {
		t.Errorf("artifact of the completed retry = %+v", final.Artifact)
	}
	f.wantRestoredDataKept()
	if got := f.marker(); got != "" {
		t.Errorf("marker after the completed retry = %q, want none", got)
	}
}

// Restored data that may already have been started is never removed: a retry
// that asks for a different restore is refused and the data stays (#267).
func TestServer_Restore_DoesNotRemoveRestoredDataForADifferentRequest(t *testing.T) {
	f := newResumeFixture(t)
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases})
	failed := f.run("restore-1")
	if failed.State != OpFailed {
		t.Fatalf("first attempt = %s, want Failed", failed.State)
	}

	f.req.ExpectedCubridVersion = ""
	second := &volumeCLI{databases: f.databases}
	f.start(second)
	final := f.run("restore-other")
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "restored") {
		t.Fatalf("different restore = %s (%s), want Failed naming the restored data", final.State, final.FailureReason)
	}
	if calls := second.recorded(); len(calls) != 0 {
		t.Errorf("commands ran over the restored data: %q", calls)
	}
	f.wantRestoredDataKept()
	if got := f.marker(); got != failed.ID {
		t.Errorf("marker = %q, want it kept as %q", got, failed.ID)
	}
}

// An HA bootstrap never removes the data a restore has restored.
func TestReclaimIncomplete_KeepsRestoredData(t *testing.T) {
	databases := t.TempDir()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := leaveInterrupted(t, store, databases, OpRestore, OpFailed)
	id := strings.TrimSpace(readFile(t, filepath.Join(dir, ownershipMarker)))
	if _, err := store.Update(id, func(op *Operation) { op.Restored = &OperationArtifact{Database: dbName} }); err != nil {
		t.Fatal(err)
	}
	s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})
	if err := s.reclaimIncomplete(dbName); err == nil {
		t.Fatal("reclaimIncomplete removed restored data without an error")
	}
	if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
		t.Errorf("restored data was removed: %v", err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// When the store cannot record that restoredb succeeded, the database is not
// started: it is not yet known to be restored, so the next attempt may remove
// it and restore again (#267).
func TestServer_Restore_DoesNotStartWhatItCouldNotRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	f := newResumeFixture(t)
	first := &volumeCLI{databases: f.databases}
	first.afterRestore = func() {
		if err := os.Chmod(f.storeDir, 0o500); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(f.storeDir, 0o750) })
	f.start(first)
	op := f.post("restore-1")
	// The operation cannot reach a terminal state with a read-only store; wait
	// until the restore has run and the attempt has had time to go on.
	deadline := time.Now().Add(5 * time.Second)
	for count(first.recorded(), "cubrid restoredb ") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := first.recorded(); count(calls, callServerStart) != 0 {
		t.Fatalf("the server was started although the restore was not recorded: %q", calls)
	}

	// The manager restarts with a writable store: the attempt is Failed.
	if err := os.Chmod(f.storeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	second := &volumeCLI{databases: f.databases}
	f.start(second)
	if got := f.marker(); got != op.ID {
		t.Fatalf("marker = %q, want the unrecorded operation %q", got, op.ID)
	}
	final := f.run("restore-2")
	if final.State != OpCompleted {
		t.Fatalf("retry = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := second.recorded()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "cubrid restoredb ") || calls[1] != callServerStart {
		t.Errorf("retry calls = %q, want restoredb then %q", calls, callServerStart)
	}
	if count(first.recorded(), callServerStart) != 0 {
		t.Errorf("the first attempt started the server: %q", first.recorded())
	}
}

// A manager that stopped inside a takeover, after the new operation recorded
// the restored data but before it rewrote the marker, leaves the data owned
// by the earlier operation; the next attempt still takes it over (#267).
func TestServer_Restore_RetryAfterAStopInsideTheTakeover(t *testing.T) {
	f := newResumeFixture(t)
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases})
	failed := f.run("restore-1")
	if failed.State != OpFailed || failed.Restored == nil {
		t.Fatalf("first attempt = %s (restored %v), want Failed with the data recorded", failed.State, failed.Restored)
	}
	// The interrupted takeover: recorded, marker not rewritten, then Failed
	// by the restart.
	store, err := NewOperationStore(f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	taking, _, err := store.FindOrCreate(OpRestore, "restore-2", failed.RequestHash, dbName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(taking.ID, func(op *Operation) {
		op.State = OpStarting
		op.Restored = failed.Restored
	}); err != nil {
		t.Fatal(err)
	}

	retry := &volumeCLI{databases: f.databases}
	f.start(retry)
	final := f.run("restore-3")
	if final.State != OpCompleted {
		t.Fatalf("retry = %s (%s), want Completed", final.State, final.FailureReason)
	}
	if calls := retry.recorded(); len(calls) != 1 || calls[0] != callServerStart {
		t.Errorf("retry calls = %q, want only %q", calls, callServerStart)
	}
	f.wantRestoredDataKept()
	if got := f.marker(); got != "" {
		t.Errorf("marker after the completed retry = %q, want none", got)
	}
}

// A completed restore whose marker was left has a database the entrypoint
// does not start: the manager starts it when it starts, then clears the
// marker (#267).
func TestResumeCompletedRestores_StartsTheDatabaseAndClearsTheMarker(t *testing.T) {
	f := newResumeFixture(t)
	f.start(&volumeCLI{databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	marker := filepath.Join(f.databases, dbName, ownershipMarker)
	if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := &volumeCLI{databases: f.databases}
	f.start(restarted)
	if err := f.server.ResumeCompletedRestores(t.Context()); err != nil {
		t.Fatalf("ResumeCompletedRestores: %v", err)
	}
	if calls := restarted.recorded(); len(calls) != 1 || calls[0] != callServerStart {
		t.Errorf("calls = %q, want only %q", calls, callServerStart)
	}
	if got := f.marker(); got != "" {
		t.Errorf("marker = %q, want it cleared", got)
	}
	f.wantRestoredDataKept()
}

// A configuration mismatch after restart must not clear the ownership of a
// completed restore whose database the member cannot start.
func TestResumeCompletedRestores_KeepsAnotherStandaloneDatabase(t *testing.T) {
	f := newResumeFixture(t)
	f.start(&volumeCLI{databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	marker := filepath.Join(f.databases, dbName, ownershipMarker)
	if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := &volumeCLI{databases: f.databases}
	f.start(restarted)
	f.server.WithStandaloneDatabase("otherdb")
	if err := f.server.ResumeCompletedRestores(t.Context()); err == nil || !strings.Contains(err.Error(), "otherdb") {
		t.Errorf("resume = %v, want an error naming the configured database mismatch", err)
	}
	if calls := restarted.recorded(); len(calls) != 0 {
		t.Errorf("resume ran commands for another database: %v", calls)
	}
	if got := f.marker(); got != done.ID {
		t.Errorf("marker = %q, want it kept as %q", got, done.ID)
	}
	f.wantRestoredDataKept()
}

// A start that fails keeps the marker, so the next start of the manager tries
// again; the marker of an operation that did not complete is left alone.
func TestResumeCompletedRestores_LeavesWhatItCannotFinish(t *testing.T) {
	t.Run("start fails", func(t *testing.T) {
		f := newResumeFixture(t)
		f.start(&volumeCLI{databases: f.databases})
		done := f.run("restore-1")
		marker := filepath.Join(f.databases, dbName, ownershipMarker)
		if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases})
		if err := f.server.ResumeCompletedRestores(t.Context()); err == nil {
			t.Error("ResumeCompletedRestores reported no error for a failed start")
		}
		if got := f.marker(); got != done.ID {
			t.Errorf("marker = %q, want it kept as %q", got, done.ID)
		}
	})
	t.Run("failed operation", func(t *testing.T) {
		f := newResumeFixture(t)
		f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}, databases: f.databases})
		failed := f.run("restore-1")
		restarted := &volumeCLI{databases: f.databases}
		f.start(restarted)
		if err := f.server.ResumeCompletedRestores(t.Context()); err != nil {
			t.Fatalf("ResumeCompletedRestores: %v", err)
		}
		if calls := restarted.recorded(); len(calls) != 0 {
			t.Errorf("commands ran for a failed restore: %q", calls)
		}
		if got := f.marker(); got != failed.ID {
			t.Errorf("marker = %q, want it kept as %q", got, failed.ID)
		}
	})
}

// On an HA member, a heartbeat status that did not answer starts nothing when
// the manager resumes a completed restore: the error is returned and the
// marker kept, so the next start of the manager tries again.
func TestResumeCompletedRestores_DoesNotStartHeartbeatWhenStatusDidNotAnswer(t *testing.T) {
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	marker := filepath.Join(f.databases, dbName, ownershipMarker)
	if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := &volumeCLI{stepCLI: stepCLI{errs: map[string]error{stepHeartbeatStatus: context.DeadlineExceeded}}, databases: f.databases}
	f.start(restarted)
	if err := f.server.ResumeCompletedRestores(t.Context()); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("ResumeCompletedRestores = %v, want an error because heartbeat status did not answer", err)
	}
	if calls := restarted.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started although its status did not answer: %q", calls)
	}
	if got := f.marker(); got != done.ID {
		t.Errorf("marker = %q, want it kept as %q", got, done.ID)
	}
	f.wantRestoredDataKept()
}

// stateUnknownOut is what `cubrid heartbeat status` printed, with exit 0,
// after `cubrid heartbeat stop` and after a failed `cubrid heartbeat start`:
// cub_master stays and HA is not active (docs/poc/RESULTS.md, POC-26, 2a, 4a
// and 4c).
const stateUnknownOut = `@ cubrid heartbeat status

 HA-Node Info (current h-0, state unknown)


 HA-Process Info (master 344, state unknown)

`

// The restore of an HA member whose heartbeat runs without a confirmed role
// does not complete: heartbeat is not started again, the operation fails
// with the reason, and the restored data and its marker are kept. A later
// attempt of the same request, once heartbeat reports a role, takes that data
// over and completes without running restoredb again (#454).
func TestServer_Restore_FailsWhileHeartbeatRunsWithoutAConfirmedRole(t *testing.T) {
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &volumeCLI{stepCLI: stepCLI{output: map[string]string{stepHeartbeatStatus: stateUnknownOut}}, databases: f.databases}
	f.start(cli)
	failed := f.run("restore-1")
	if failed.State != OpFailed || !strings.Contains(failed.FailureReason, "'unknown'") ||
		!strings.Contains(failed.FailureReason, "without a confirmed role") {
		t.Fatalf("restore = %s (%s), want Failed naming state 'unknown' and the missing role", failed.State, failed.FailureReason)
	}
	if calls := cli.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started although it reported state unknown: %q", calls)
	}
	if got := f.marker(); got != failed.ID {
		t.Errorf("marker = %q, want it kept as %q", got, failed.ID)
	}
	f.wantRestoredDataKept()

	retry := &volumeCLI{stepCLI: stepCLI{output: map[string]string{stepHeartbeatStatus: masterOut}}, databases: f.databases}
	f.start(retry)
	done := f.run("restore-2")
	if done.State != OpCompleted {
		t.Fatalf("retry = %s (%s), want Completed", done.State, done.FailureReason)
	}
	if calls := retry.recorded(); count(calls, "cubrid restoredb") != 0 || count(calls, callHeartbeatStart) != 0 {
		t.Errorf("retry calls = %q, want neither restoredb nor a heartbeat start", calls)
	}
	f.wantRestoredDataKept()
}

// On an HA member whose heartbeat runs without a confirmed role, the start of
// a completed restore's database does not count as done: the error is
// returned, the marker and the data are kept so that the next start of the
// manager tries again, and the manager serves all the same (#454).
func TestResumeCompletedRestores_FailsWhileHeartbeatRunsWithoutAConfirmedRole(t *testing.T) {
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	marker := filepath.Join(f.databases, dbName, ownershipMarker)
	if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := &volumeCLI{stepCLI: stepCLI{output: map[string]string{stepHeartbeatStatus: stateUnknownOut}}, databases: f.databases}
	f.start(restarted)
	err := f.server.ResumeCompletedRestores(t.Context())
	if err == nil || !strings.Contains(err.Error(), "'unknown'") || !strings.Contains(err.Error(), "without a confirmed role") {
		t.Errorf("ResumeCompletedRestores = %v, want an error naming state 'unknown' and the missing role", err)
	}
	if err := f.server.ResumeAndServe(t.Context(), func() error { return nil }); err != nil {
		t.Errorf("ResumeAndServe = %v, want the manager to serve", err)
	}
	if calls := restarted.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started although it reported state unknown: %q", calls)
	}
	if got := f.marker(); got != done.ID {
		t.Errorf("marker = %q, want it kept as %q", got, done.ID)
	}
	f.wantRestoredDataKept()
}

// A second round that finds heartbeat running without a confirmed role
// follows the same rule when the manager resumes a completed restore: nothing
// is started, the error is returned and the marker kept (#454).
func TestResumeCompletedRestores_SecondRoundWithoutAConfirmedRoleStartsNothing(t *testing.T) {
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
	done := f.run("restore-1")
	if done.State != OpCompleted {
		t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
	}
	if err := os.WriteFile(filepath.Join(f.databases, dbName, ownershipMarker), []byte(done.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cli := &sequenceCLI{t: t, answers: []statusAnswer{answerB, {stateUnknownOut, false}}}
	f.start(cli)
	err := f.server.ResumeCompletedRestores(t.Context())
	if err == nil || !strings.Contains(err.Error(), "'unknown'") || !strings.Contains(err.Error(), "without a confirmed role") {
		t.Errorf("ResumeCompletedRestores = %v, want an error naming state 'unknown' and the missing role", err)
	}
	cli.mu.Lock()
	calls := append([]string(nil), cli.calls...)
	cli.mu.Unlock()
	if count(calls, callHeartbeatStart) != 0 || len(cli.statusCalls()) != 2 {
		t.Errorf("calls = %q, want two statuses and no heartbeat start", calls)
	}
	if got := f.marker(); got != done.ID {
		t.Errorf("marker = %q, want it kept as %q", got, done.ID)
	}
	f.wantRestoredDataKept()
}

// A member that is not standalone joins HA after a restore, and what it joins
// is in its HA configuration. Without a usable one the manager cannot start
// the database of a completed restore: it starts nothing, returns the error
// and keeps the marker, so the next start of the manager tries again (#348).
func TestResumeCompletedRestores_NeedsTheHAConfigurationOfAnHAMember(t *testing.T) {
	for name, breakConf := range map[string]func(path string) error{
		"a missing file":              os.Remove,
		"a file without ha_node_list": func(path string) error { return os.WriteFile(path, []byte("ha_mode=on\n"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newResumeFixture(t)
			f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
			if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
				t.Fatal(err)
			}
			f.start(&volumeCLI{stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}, databases: f.databases})
			done := f.run("restore-1")
			if done.State != OpCompleted {
				t.Fatalf("restore = %s (%s), want Completed", done.State, done.FailureReason)
			}
			marker := filepath.Join(f.databases, dbName, ownershipMarker)
			if err := os.WriteFile(marker, []byte(done.ID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := breakConf(f.haConf); err != nil {
				t.Fatal(err)
			}

			restarted := &volumeCLI{databases: f.databases}
			f.start(restarted)
			err := f.server.ResumeCompletedRestores(t.Context())
			if err == nil || !strings.Contains(err.Error(), "HA configuration") {
				t.Errorf("ResumeCompletedRestores = %v, want an error about the HA configuration", err)
			}
			if calls := restarted.recorded(); len(calls) != 0 {
				t.Errorf("commands ran without a usable HA configuration: %q", calls)
			}
			if got := f.marker(); got != done.ID {
				t.Errorf("marker = %q, want it kept as %q", got, done.ID)
			}
			f.wantRestoredDataKept()
		})
	}
}

// The marker the image's entrypoint writes around a standalone member's own
// createdb names no operation of this manager. A restore into that target,
// the adoption of earlier restored data and the start of completed restores
// all leave the directory and the marker as they are, and run no command.
func TestRestore_LeavesTheEntrypointsUnfinishedCreatedbAlone(t *testing.T) {
	const entrypointMarker = "entrypoint-createdb"
	f := newResumeFixture(t)
	dir := filepath.Join(f.databases, dbName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(dir, dbName+"_lgat")
	const left = "left by createdb"
	for name, content := range map[string]string{ownershipMarker: entrypointMarker + "\n", filepath.Base(volume): left} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantUntouched := func() {
		t.Helper()
		if got := f.marker(); got != entrypointMarker {
			t.Errorf("marker = %q, want it kept as %q", got, entrypointMarker)
		}
		if data, err := os.ReadFile(volume); err != nil || string(data) != left {
			t.Errorf("the files createdb left were changed (err=%v): %q", err, data)
		}
	}

	cli := &volumeCLI{databases: f.databases}
	f.start(cli)
	if err := f.server.ResumeCompletedRestores(t.Context()); err != nil {
		t.Fatalf("ResumeCompletedRestores: %v", err)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("commands ran at the start of the manager: %q", calls)
	}
	wantUntouched()

	op := f.run("restore-1")
	if op.State != OpFailed || !strings.Contains(op.FailureReason, "unreadable ownership marker") {
		t.Fatalf("restore = %s (%s), want Failed for an unreadable ownership marker", op.State, op.FailureReason)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("commands ran for a restore over the entrypoint's createdb: %q", calls)
	}
	restored, err := f.server.adoptRestored(op.ID, dbName)
	if err != nil || restored != nil {
		t.Errorf("adoptRestored = %v, %v; want nothing taken over", restored, err)
	}
	wantUntouched()
}
