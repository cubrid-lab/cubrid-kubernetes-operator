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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldRestoreCLI holds the command whose words begin with hold until release
// is closed, whatever its context says, as a command that does not answer a
// cancellation would. Like a process that is never started, any other command
// whose context has already ended fails and is not recorded.
type heldRestoreCLI struct {
	stepCLI
	hold        string
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func newHeldRestoreCLI() *heldRestoreCLI { return newHeldCLI("restoredb") }

func newHeldCLI(hold string) *heldRestoreCLI {
	return &heldRestoreCLI{hold: hold, entered: make(chan struct{}), release: make(chan struct{})}
}

func (c *heldRestoreCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	if strings.HasPrefix(strings.Join(args, " "), c.hold) {
		c.enteredOnce.Do(func() { close(c.entered) })
		<-c.release
		return c.stepCLI.Run(ctx, name, args...)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.stepCLI.Run(ctx, name, args...)
}

// startHeldRestore starts a restore on a standalone member and returns once
// its restoredb is running. The restore would start the server next.
func startHeldRestore(t *testing.T, cli *heldRestoreCLI) (*Server, http.Handler, string) {
	t.Helper()
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithRestoreRoots(rootsFor(req)).WithStandaloneDatabase(dbName)
	h := srv.Handler()
	t.Cleanup(func() { awaitTerminalOperations(t, store) })

	body, _ := json.Marshal(req)
	rr := postOperation(h, "/v1/restore/prepare", string(body), "restore-1")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/restore/prepare = %d: %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cli.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("restoredb was not run")
	}
	return srv, h, op.ID
}

func postOperation(h http.Handler, path, body, key string) *httptest.ResponseRecorder {
	post := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", key)
	post.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	return rr
}

// wantNothingStartedAfterTheStop fails when a start of CUBRID was issued
// after the last stop of its server.
func wantNothingStartedAfterTheStop(t *testing.T, calls []string) {
	t.Helper()
	lastStop := -1
	for i, call := range calls {
		if call == stopServer {
			lastStop = i
		}
	}
	if lastStop < 0 {
		t.Fatalf("calls = %q, the server was never stopped", calls)
	}
	for _, call := range calls[lastStop:] {
		if call == callServerStart || call == callHeartbeatStart {
			t.Errorf("calls = %q: %q ran after the final stop", calls, call)
		}
	}
}

// A restore that is about to start its database when the member is stopped
// does not start it after the final shutdown, and its operation says why it
// did not complete. A stop that follows is safe and stops nothing twice
// (#273).
func TestServer_Stop_NoStartAfterTheFinalShutdown(t *testing.T) {
	cli := newHeldRestoreCLI()
	srv, h, id := startHeldRestore(t, cli)

	// restoredb finishes while the stop is under way.
	time.AfterFunc(50*time.Millisecond, func() { close(cli.release) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	final := pollUntilTerminal(t, h, id)
	calls := cli.recorded()
	wantNothingStartedAfterTheStop(t, calls)
	if final.State != OpFailed || !strings.Contains(final.FailureReason, "stopping") {
		t.Errorf("operation = %s (%q), want Failed because the member is stopping", final.State, final.FailureReason)
	}

	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if again := cli.recorded(); len(again) != len(calls) {
		t.Errorf("the second stop ran %q", again[len(calls):])
	}
}

// The stop waits for running operations only as long as it may. When they
// have not ended by then it says so, and none of them starts CUBRID later;
// the next stop runs the whole shutdown again.
func TestServer_Stop_WaitForOperationsIsBounded(t *testing.T) {
	cli := newHeldRestoreCLI()
	srv, h, id := startHeldRestore(t, cli)

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := srv.Stop(short, dbName); err == nil {
		t.Error("Stop reported success while an operation was still running")
	}
	// The wait left the shutdown part of the deadline.
	if !slices.Contains(cli.recorded(), stopServer) {
		t.Errorf("calls = %q, the stop whose wait ran out did not stop the server", cli.recorded())
	}
	close(cli.release)
	final := pollUntilTerminal(t, h, id)
	if final.State != OpFailed {
		t.Errorf("operation = %s, want Failed", final.State)
	}

	ctx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	calls := cli.recorded()
	wantNothingStartedAfterTheStop(t, calls)
	if slices.Contains(calls, callServerStart) {
		t.Errorf("calls = %q, the server was started while the member stopped", calls)
	}
}

// Once the member is stopping, no new mutating operation is accepted.
func TestServer_Stop_RejectsNewOperations(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &stepCLI{}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(testStagingRoot)
	if err := srv.Stop(context.Background(), dbName); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for path, body := range map[string]string{
		"/v1/backup":          `{"database":"` + dbName + `","destination":"` + testStagingRoot + `/b1"}`,
		"/v1/restore/prepare": `{"database":"` + dbName + `"}`,
		"/v1/ha/bootstrap":    `{"database":"` + dbName + `"}`,
	} {
		if rr := postOperation(h, path, body, "k1"); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s while stopping = %d (%s), want 503", path, rr.Code, rr.Body.String())
		}
	}
	if store.AnyInProgress() {
		t.Error("an operation was recorded while the member was stopping")
	}
}

// A start that is already running when the member is stopped is over before
// the server is stopped: the stop does not race it (#273).
func TestServer_Stop_WaitsForARunningStart(t *testing.T) {
	cli := newHeldCLI("server start")
	srv, h, id := startHeldRestore(t, cli)

	time.AfterFunc(50*time.Millisecond, func() { close(cli.release) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	pollUntilTerminal(t, h, id)
	calls := cli.recorded()
	start, stop := slices.Index(calls, callServerStart), slices.Index(calls, stopServer)
	if start < 0 || stop < start {
		t.Errorf("calls = %q, want the server stopped after its start returned", calls)
	}
	wantNothingStartedAfterTheStop(t, calls)
}

// A repeated request for an operation that exists is not counted as running,
// so the stop does not wait for it.
func TestServer_Stop_RepeatedRequestIsNotWaitedFor(t *testing.T) {
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&stepCLI{}, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithRestoreRoots(rootsFor(req)).WithStandaloneDatabase(dbName)
	h := srv.Handler()
	body, _ := json.Marshal(req)
	rr := postOperation(h, "/v1/restore/prepare", string(body), "restore-1")
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if final := pollUntilTerminal(t, h, op.ID); final.State != OpCompleted {
		t.Fatalf("operation = %s (%s), want Completed", final.State, final.FailureReason)
	}
	if rr := postOperation(h, "/v1/restore/prepare", string(body), "restore-1"); rr.Code != http.StatusAccepted {
		t.Fatalf("repeated request = %d: %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("Stop took %s, it waited for an operation that was not running", took)
	}
}

// cancelledRestoreCLI runs restoredb until its context ends, as a command
// that the stop's cancellation ends.
type cancelledRestoreCLI struct {
	stepCLI
	entered chan struct{}
}

func (c *cancelledRestoreCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "restoredb" {
		close(c.entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	return c.stepCLI.Run(ctx, name, args...)
}

// An operation whose command the stop cancelled says that the member is
// stopping.
func TestServer_Stop_CancelledOperationSaysTheMemberIsStopping(t *testing.T) {
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &cancelledRestoreCLI{entered: make(chan struct{})}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithRestoreRoots(rootsFor(req)).WithStandaloneDatabase(dbName)
	h := srv.Handler()
	body, _ := json.Marshal(req)
	var op Operation
	if err := json.Unmarshal(postOperation(h, "/v1/restore/prepare", string(body), "restore-1").Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	<-cli.entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx, dbName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	final := pollUntilTerminal(t, h, op.ID)
	if final.State != OpFailed || !strings.HasPrefix(final.FailureReason, errStopping.Error()) {
		t.Errorf("operation = %s (%q), want Failed because the member is stopping", final.State, final.FailureReason)
	}
}
