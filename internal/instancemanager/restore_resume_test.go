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

// resumeFixture is a standalone member whose manager can be restarted on the
// same operation store and database root.
type resumeFixture struct {
	t         *testing.T
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
		WithRestoreRoots(rootsFor(f.req)).WithStandaloneDatabase(dbName)
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
	return pollUntilTerminal(f.t, f.handler, f.post(key).ID)
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
