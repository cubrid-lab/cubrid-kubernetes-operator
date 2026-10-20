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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func skipRootPersistence(t *testing.T) {
	t.Helper()
	// Check on the test goroutine so no operation callback calls SkipNow.
	if os.Geteuid() == 0 {
		t.Skip("a directory mode does not keep root from writing")
	}
}

// unwritable makes the store's directory refuse new files, as a full or
// read-only volume would, and returns the way back. The records stay readable.
// Only call this wrapper on the test goroutine; workers use the helper below.
func unwritable(t *testing.T, dir string) (restore func()) {
	t.Helper()
	skipRootPersistence(t)
	restore = unwritableDuringOperation(t, dir)
	if restore == nil {
		t.Fatal("could not inject the operation-store permission failure")
	}
	return restore
}

// unwritableDuringOperation reports setup errors without ending a worker
// goroutine. Callers join the operation before checking its returned repair.
func unwritableDuringOperation(t *testing.T, dir string) (restore func()) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- a directory of the test
		t.Error(err)
		return nil
	}
	restore = func() {
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory of the test
			t.Error(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// The end of an operation that could not be written is not lost and not
// passed off as recorded: while it is unwritten the operation cannot be read
// as if it were still running, and the first read after the store works
// again writes it (#299).
func TestOperationStore_Settle_KeepsAnEndThatCouldNotBeWritten(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	store.SetLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	op, _, err := store.FindOrCreate(OpBackup, "k", "h", "appdb")
	if err != nil {
		t.Fatal(err)
	}

	restore := unwritable(t, dir)
	completed := func(o *Operation) { o.State = OpCompleted }
	if err := store.Settle(op.ID, completed); err == nil {
		t.Fatal("Settle reported no error although the record could not be written")
	}
	if !strings.Contains(logged.String(), `"event":"operation_store_write_failed"`) ||
		!strings.Contains(logged.String(), op.ID) {
		t.Errorf("the failed write was not logged with its event and operation: %s", logged.String())
	}
	if got, err := store.Get(op.ID); err == nil {
		t.Errorf("Get = %s without an error, although how the operation ended is not on record", got.State)
	}
	// A request for the same database is not started beside it.
	if _, _, err := store.FindOrCreate(OpBackup, "other-key", "h", "appdb"); err == nil {
		t.Error("another operation on the database was admitted while this one's end is unwritten")
	}
	// The retries of those reads do not log another error each.
	if n := strings.Count(logged.String(), `"level":"ERROR"`); n != 1 {
		t.Errorf("failed writes logged as errors = %d, want only the first: %s", n, logged.String())
	}

	restore()
	got, err := store.Get(op.ID)
	if err != nil || got.State != OpCompleted {
		t.Fatalf("after the store works again Get = %+v, %v; want Completed", got, err)
	}
	// It is on disk: a manager that starts on the same store reads it.
	reopened, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, existed, err := reopened.FindOrCreate(OpBackup, "k", "h", "appdb"); err != nil || !existed || again.State != OpCompleted {
		t.Errorf("after a restart the same key = %+v (existed %v), %v; want the Completed operation", again, existed, err)
	}
}

// An end that was never written is not Completed after a restart: the
// manager that starts fails the operation, and the same key still names it.
func TestOperationStore_Settle_UnwrittenEndIsNotCompletedAfterARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.FindOrCreate(OpBackup, "k", "h", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	restore := unwritable(t, dir)
	_ = store.Settle(op.ID, func(o *Operation) { o.State = OpCompleted })
	restore()

	restarted, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, existed, err := restarted.FindOrCreate(OpBackup, "k", "h", "appdb")
	if err != nil || !existed || again.ID != op.ID {
		t.Fatalf("the same key after a restart = %+v (existed %v), %v; want the same operation", again, existed, err)
	}
	if again.State != OpFailed {
		t.Errorf("state after a restart = %s, want Failed: its end was never on record", again.State)
	}
}

// A backup whose next state cannot be recorded goes no further: nothing is
// uploaded, and the operation ends Failed with that reason once the store can
// be written again (#299).
func TestServer_Backup_StopsWhenAStateCannotBeRecorded(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	objects := newFakeStore()
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	cli := newHeldCLI("backupdb")
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).
		WithBackupStagingRoot(filepath.Dir(staging))
	h := srv.Handler()

	body := `{"database":"appdb","destination":"` + staging + `","upload":{"bucket":"cubrid-backups","prefix":"prod/uid-1","clusterUID":"cuid","cubridVersion":"11.4.6","sourceInstance":"appdb-1","sourceRole":"slave"}}`
	rr := postBackup(t, h, "key-1", body)
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cli.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backupdb was not run")
	}

	// backupdb ends well, and the state Uploading cannot be recorded.
	restore := unwritable(t, dir)
	close(cli.release)
	srv.ops.Wait()
	if len(objects.objects) != 0 {
		t.Errorf("objects were uploaded although the state could not be recorded: %d", len(objects.objects))
	}
	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/operations/"+op.ID, nil)
		req.Header.Set("Authorization", "Bearer tok")
		req.RemoteAddr = testRemoteAddr
		got := httptest.NewRecorder()
		h.ServeHTTP(got, req)
		return got
	}
	if got := poll(); got.Code == http.StatusOK {
		t.Errorf("the operation was answered as %s while its end is unwritten", got.Body.String())
	}

	restore()
	got := poll()
	var ended Operation
	if err := json.Unmarshal(got.Body.Bytes(), &ended); err != nil {
		t.Fatalf("poll = %d %s", got.Code, got.Body.String())
	}
	if ended.State != OpFailed || !strings.Contains(ended.FailureReason, "record") {
		t.Errorf("operation = %s (%q), want Failed because its state could not be recorded", ended.State, ended.FailureReason)
	}
}

// An HA bootstrap that cannot record that it is about to start heartbeat
// does not start it.
func TestHABootstrap_StartsNothingItCannotRecord(t *testing.T) {
	f := newHAFixture(t)
	held := newHeldCLI("createdb")
	held.failing = map[string]bool{stepHeartbeatStatus: true} // heartbeat is not running
	f.srv.cli = held
	if rr := f.post("bootstrap-1"); rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/ha/bootstrap = %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("createdb was not run")
	}
	restore := unwritable(t, f.store.dir)
	close(held.release)
	f.srv.ops.Wait()
	if calls := held.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Errorf("heartbeat was started although the state Starting could not be recorded: %q", calls)
	}
	restore()
	ops, err := f.store.FindByKey("bootstrap-1")
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations = %d, %v", len(ops), err)
	}
	ended, err := f.store.Get(ops[0].ID)
	if err != nil || ended.State != OpFailed || !strings.Contains(ended.FailureReason, "record") {
		t.Errorf("operation = %+v, %v; want Failed because its state could not be recorded", ended, err)
	}
}

// onCreated is a log handler that runs once, right after the store wrote a
// new operation's first record: the moment between the record and the next
// write of the same request.
type onCreated struct {
	slog.Handler
	once func()
}

func (h *onCreated) Handle(ctx context.Context, r slog.Record) error {
	created := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "previousState" && a.Value.String() == "" {
			created = true
		}
		return true
	})
	if created && h.once != nil {
		h.once()
		h.once = nil
	}
	return h.Handler.Handle(ctx, r)
}

// A backup whose staging claim cannot be recorded, and whose failure cannot
// be recorded either, is not answered 202 with a Pending operation that never
// runs. Its end is written once the store works again, and the same key then
// names the Failed operation.
func TestServer_Backup_UnrecordedStagingClaimIsNotAnsweredAsPending(t *testing.T) {
	skipRootPersistence(t)
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var restore func()
	store.SetLogger(slog.New(&onCreated{
		Handler: slog.NewJSONHandler(&bytes.Buffer{}, nil),
		once:    func() { restore = unwritableDuringOperation(t, dir) },
	}))
	cli := newHeldCLI("backupdb")
	staging := t.TempDir()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(staging)
	h := srv.Handler()

	body := `{"database":"appdb","destination":"` + filepath.Join(staging, "bk") + `","upload":{"bucket":"cubrid-backups","prefix":"prod/uid-1","clusterUID":"cuid","cubridVersion":"11.4.6","sourceInstance":"appdb-1","sourceRole":"slave"}}`
	rr := postBackup(t, h, "key-1", body)
	if rr.Code == http.StatusAccepted {
		t.Errorf("POST /v1/backup = 202 %s although neither its staging nor its failure is on record", rr.Body.String())
	}
	if restore == nil {
		t.Fatal("the store did not record the operation before its staging claim")
	}
	srv.ops.Wait()
	if calls := cli.recorded(); count(calls, "cubrid backupdb") != 0 {
		t.Errorf("backupdb ran for an operation whose staging is not on record: %q", calls)
	}

	// A repeat while the store is still broken is not answered as Pending
	// either: the operation it names never runs.
	if rr := postBackup(t, h, "key-1", body); rr.Code == http.StatusAccepted {
		t.Errorf("repeat of the same key = 202 %s while the operation's end is unwritten", rr.Body.String())
	}

	restore()
	again := postBackup(t, h, "key-1", body)
	var op Operation
	if err := json.Unmarshal(again.Body.Bytes(), &op); err != nil {
		t.Fatalf("repeat = %d %s", again.Code, again.Body.String())
	}
	if op.State != OpFailed || !strings.Contains(op.FailureReason, "staging") {
		t.Errorf("repeat of the same key = %s (%q), want the Failed operation", op.State, op.FailureReason)
	}
}

// FindByKey does not hand out a record that still says an operation runs
// whose end could not be written yet; it does once that end is on disk.
func TestOperationStore_FindByKey_DoesNotReturnAnUnwrittenEnd(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.FindOrCreate(OpBackup, "k", "h", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	restore := unwritable(t, dir)
	_ = store.Settle(op.ID, func(o *Operation) { o.State = OpCompleted })
	if got, err := store.FindByKey("k"); err == nil {
		t.Errorf("FindByKey = %d operations without an error, although how the operation ended is not on record", len(got))
	}
	restore()
	got, err := store.FindByKey("k")
	if err != nil || len(got) != 1 || got[0].State != OpCompleted {
		t.Fatalf("after the store works again FindByKey = %v, %v; want the Completed operation", got, err)
	}
}

// afterCallCLI runs after once, when the command call has run.
type afterCallCLI struct {
	*volumeCLI
	call  string
	after func()
}

func (c *afterCallCLI) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := c.volumeCLI.Run(ctx, name, args...)
	if name+" "+strings.Join(args, " ") == c.call && c.after != nil {
		c.after()
		c.after = nil
	}
	return out, err
}

// A restore whose Completed cannot be written keeps its ownership marker, is
// not answered as running, and is Failed with its restored data on record
// after a restart: the end that was only in memory does not count (#299).
func TestServer_Restore_UnwrittenCompletedIsFailedAfterARestart(t *testing.T) {
	skipRootPersistence(t)
	f := newResumeFixture(t)
	var restore func()
	cli := &afterCallCLI{
		volumeCLI: &volumeCLI{databases: f.databases},
		call:      callServerStart,
		after:     func() { restore = unwritableDuringOperation(t, f.storeDir) },
	}
	f.start(cli)
	op := f.post("restore-1")
	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/operations/"+op.ID, nil)
		req.Header.Set("Authorization", "Bearer tok")
		req.RemoteAddr = testRemoteAddr
		got := httptest.NewRecorder()
		f.handler.ServeHTTP(got, req)
		return got
	}
	deadline := time.Now().Add(5 * time.Second)
	got := poll()
	for got.Code == http.StatusOK && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		got = poll()
	}
	f.server.ops.Wait()
	if restore == nil {
		t.Fatalf("the server was not started: %q", cli.recorded())
	}
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("poll = %d %s, want 500 while Completed is unwritten", got.Code, got.Body.String())
	}
	if marker := f.marker(); marker != op.ID {
		t.Errorf("ownership marker = %q, want %q: the restore is not on record as complete", marker, op.ID)
	}

	restore()
	f.start(&volumeCLI{databases: f.databases})
	ended, err := f.server.store.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.State != OpFailed || ended.Restored == nil {
		t.Errorf("after a restart = %s (restored %v), want Failed with the restored data on record", ended.State, ended.Restored)
	}
}

// afterStateRecord runs after a successful state write. It only changes the
// test directory's permissions: the store still holds its mutex here.
type afterStateRecord struct {
	slog.Handler
	state OperationState
	once  func()
}

func (h *afterStateRecord) Handle(ctx context.Context, r slog.Record) error {
	var event, state string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "event":
			event = a.Value.String()
		case "state":
			state = a.Value.String()
		}
		return true
	})
	if event == eventOperationState && state == string(h.state) && h.once != nil {
		h.once()
		h.once = nil
	}
	return h.Handler.Handle(ctx, r)
}

func persistenceRequest(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Idempotency-Key", key)
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

type failureMetadata struct {
	restored  *OperationArtifact
	owner     string
	reason    string
	forbidden OperationState
}

func checkFailureMetadata(t *testing.T, srv *Server, stage string, record *Operation, want failureMetadata) {
	t.Helper()
	if !reflect.DeepEqual(record.Restored, want.restored) {
		t.Errorf("%s restored record = %+v, want %+v", stage, record.Restored, want.restored)
	}
	if want.owner != "" {
		marker := strings.TrimSpace(readFile(t, filepath.Join(srv.restoreRoots.Target, record.Database, ownershipMarker)))
		if marker != want.owner {
			t.Errorf("%s ownership marker = %q, want %q", stage, marker, want.owner)
		}
	}
}

// checkTransitionFailure checks the disk and HTTP boundaries while writes
// remain broken, then settles the failure and replays the request through a
// fresh manager/store. The caller checks command, object and filesystem side
// effects after every replay has finished.
func checkTransitionFailure(t *testing.T, srv *Server, op Operation, previous, attempted OperationState,
	logged *bytes.Buffer, repair func(), replay func(http.Handler) *httptest.ResponseRecorder,
	metadata ...failureMetadata,
) {
	t.Helper()
	want := failureMetadata{reason: "could not record the state " + string(attempted)}
	if len(metadata) == 1 {
		want = metadata[0]
	}

	srv.ops.Wait()
	if repair == nil {
		t.Fatal("the intended persistence-failure boundary was not reached")
	}
	data, err := os.ReadFile(filepath.Join(srv.store.dir, op.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk Operation
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.ID != op.ID || disk.State != previous || disk.FailureReason != "" {
		t.Fatalf("disk record = %+v, want the preceding durable %s", disk, previous)
	}
	checkFailureMetadata(t, srv, "preceding disk", &disk, want)
	found := false
	for line := range strings.SplitSeq(strings.TrimSpace(logged.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if want.forbidden != "" && record["operationID"] == op.ID && record["state"] == string(want.forbidden) {
			t.Errorf("operation advanced to %s after its preceding record failed: %s", want.forbidden, line)
		}
		if record["event"] == eventStoreWriteFailed && record["operationID"] == op.ID &&
			record["state"] == string(attempted) && record["level"] == "ERROR" {
			found = true
		}
	}
	if !found {
		t.Errorf("no write-failure error names %s and %s: %s", op.ID, attempted, logged.String())
	}
	h := srv.Handler()
	for _, path := range []string{"/v1/operations/" + op.ID, "/v1/operations?idempotencyKey=" + op.IdempotencyKey} {
		if rr := persistenceRequest(h, http.MethodGet, path, "", ""); rr.Code != http.StatusInternalServerError {
			t.Errorf("GET %s = %d %s, want 500 while the end is unwritten", path, rr.Code, rr.Body.String())
		}
	}
	if rr := replay(h); rr.Code != http.StatusInternalServerError {
		t.Errorf("same-key replay = %d %s while the end is unwritten, want 500", rr.Code, rr.Body.String())
	}
	srv.ops.Wait()
	repair()
	ended, err := srv.store.Get(op.ID)
	if err != nil || ended.State != OpFailed || !strings.Contains(ended.FailureReason, want.reason) {
		t.Fatalf("settled operation = %+v, %v; want Failed with reason containing %q", ended, err, want.reason)
	}
	checkFailureMetadata(t, srv, "settled", ended, want)
	reopened, err := NewOperationStore(srv.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(srv.cli, "tok").WithOperationStore(reopened).WithObjectStore(srv.objects).
		WithBackupStagingRoot(srv.backupStagingRoot).WithRestoreRoots(srv.restoreRoots).
		WithStandaloneDatabase(srv.standaloneDB).WithHAConfig(srv.ha)
	restarted.heartbeatProcesses, restarted.heartbeatSettle = srv.heartbeatProcesses, srv.heartbeatSettle
	rr := replay(restarted.Handler())
	restarted.ops.Wait()
	if rr.Code != http.StatusAccepted {
		t.Fatalf("same-key replay after restart = %d %s, want 202 with the existing failure", rr.Code, rr.Body.String())
	}
	var again Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.ID != op.ID || again.State != OpFailed || again.FailureReason != ended.FailureReason {
		t.Errorf("same-key replay after restart = %+v, want the same durable failure %+v", again, ended)
	}
	checkFailureMetadata(t, srv, "replayed after restart", &again, want)
}

func TestServer_Backup_StopsBeforeUnrecordedRunningBackup(t *testing.T) {
	skipRootPersistence(t)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	store.SetLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	cli, objects := &stepCLI{}, newFakeStore()
	root := t.TempDir()
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
	upload := *testUpload
	req := BackupRequest{Database: dbName, Destination: filepath.Join(root, "backup"), Upload: &upload}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	// The staging claim must succeed before injection; failing it would test
	// an earlier boundary and never exercise RunningBackup.
	op, staging, err := srv.recordBackup("backup-key", HashRequest(body), req)
	if err != nil || staging == "" {
		t.Fatalf("recordBackup = %q, %v", staging, err)
	}
	claimed, err := store.Get(op.ID)
	if err != nil || claimed.StagingDir != staging {
		t.Fatalf("staging claim was not persisted: %+v, %v", claimed, err)
	}
	repair := unwritable(t, store.dir)
	if err := srv.admit(); err != nil {
		t.Fatal(err)
	}
	req.Destination = staging
	srv.runBackup(op.ID, req)
	checkTransitionFailure(t, srv, *op, OpPending, OpRunningBackup, &logged, repair, func(h http.Handler) *httptest.ResponseRecorder {
		return postBackup(t, h, "backup-key", string(body))
	})
	if calls := cli.recorded(); len(calls) != 0 || len(objects.objects) != 0 {
		t.Errorf("unrecorded RunningBackup ran commands %v or uploaded %d objects", calls, len(objects.objects))
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("unrecorded RunningBackup left its staging directory: %v", err)
	}
}

func TestServer_Restore_StopsBeforeUnrecordedTransitions(t *testing.T) {
	skipRootPersistence(t)
	for _, tc := range []struct{ previous, attempted OperationState }{
		{OpPending, OpDownloading}, {OpDownloading, OpRestoring},
	} {
		t.Run(string(tc.attempted), func(t *testing.T) {
			f := newResumeFixture(t)
			objects := &countingStore{fakeObjectStore: f.objects.(*fakeObjectStore)}
			f.objects = objects
			cli := &stepCLI{}
			f.start(cli)
			var logged bytes.Buffer
			var repair func()
			f.server.store.SetLogger(slog.New(&afterStateRecord{Handler: slog.NewJSONHandler(&logged, nil), state: tc.previous,
				once: func() { repair = unwritableDuringOperation(t, f.storeDir) }}))
			op := f.post("restore-key")
			body, err := json.Marshal(f.req)
			if err != nil {
				t.Fatal(err)
			}
			// Join before reading the injection callback's result.
			f.server.ops.Wait()
			checkTransitionFailure(t, f.server, op, tc.previous, tc.attempted, &logged, repair, func(h http.Handler) *httptest.ResponseRecorder {
				return persistenceRequest(h, http.MethodPost, "/v1/restore/prepare", "restore-key", string(body))
			})
			if calls := cli.recorded(); len(calls) != 0 || objects.gets != 0 {
				t.Errorf("unrecorded %s ran commands %v or read %d objects", tc.attempted, calls, objects.gets)
			}
			for _, path := range []string{filepath.Join(f.databases, dbName), filepath.Join(f.databases, databasesTxt), f.req.StagingDir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("unrecorded %s changed %s: %v", tc.attempted, path, err)
				}
			}
		})
	}
}

func TestHABootstrap_StopsBeforeUnrecordedCreating(t *testing.T) {
	skipRootPersistence(t)
	f := newHAFixture(t)
	var logged bytes.Buffer
	var repair func()
	f.store.SetLogger(slog.New(&onCreated{Handler: slog.NewJSONHandler(&logged, nil),
		once: func() { repair = unwritableDuringOperation(t, f.store.dir) }}))
	rr := f.post("bootstrap-key")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("bootstrap = %d %s", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	f.srv.ops.Wait()
	body, err := json.Marshal(HABootstrapRequest{Database: dbName})
	if err != nil {
		t.Fatal(err)
	}
	checkTransitionFailure(t, f.srv, op, OpPending, OpCreating, &logged, repair, func(h http.Handler) *httptest.ResponseRecorder {
		return persistenceRequest(h, http.MethodPost, "/v1/ha/bootstrap", "bootstrap-key", string(body))
	})
	if calls := f.cli.recorded(); len(calls) != 0 {
		t.Errorf("unrecorded Creating ran commands: %v", calls)
	}
	if _, err := os.Stat(f.databases); !os.IsNotExist(err) {
		t.Errorf("unrecorded Creating changed the target: %v", err)
	}
}

// The restored-artifact record is a separate boundary from the Starting state:
// restoredb has finished, but no start or subsequent transition is authorized.
func TestServer_Restore_StopsBeforeUnrecordedRestoredArtifact(t *testing.T) {
	skipRootPersistence(t)
	f := newResumeFixture(t)
	objects := &countingStore{fakeObjectStore: f.objects.(*fakeObjectStore)}
	f.objects = objects
	var repair func()
	cli := &volumeCLI{databases: f.databases,
		afterRestore: func() { repair = unwritableDuringOperation(t, f.storeDir) }}
	f.start(cli)
	var logged bytes.Buffer
	f.server.store.SetLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	op := f.post("restore-artifact")
	f.server.ops.Wait()
	reads := objects.gets
	if calls := cli.recorded(); count(calls, "cubrid restoredb ") != 1 ||
		count(calls, callServerStart) != 0 || count(calls, callHeartbeatStart) != 0 {
		t.Errorf("unrecorded restored artifact ran commands: %v", calls)
	}
	body, err := json.Marshal(f.req)
	if err != nil {
		t.Fatal(err)
	}
	checkTransitionFailure(t, f.server, op, OpRestoring, OpRestoring, &logged, repair,
		func(h http.Handler) *httptest.ResponseRecorder {
			return persistenceRequest(h, http.MethodPost, "/v1/restore/prepare", "restore-artifact", string(body))
		}, failureMetadata{owner: op.ID, reason: "record the restored database:", forbidden: OpStarting})
	if calls := cli.recorded(); count(calls, "cubrid restoredb ") != 1 ||
		count(calls, callServerStart) != 0 || count(calls, callHeartbeatStart) != 0 || objects.gets != reads {
		t.Errorf("same-key replay changed restore work: commands %v, reads %d (before %d)", calls, objects.gets, reads)
	}
	f.wantRestoredDataKept()
}

// HA status is read only after the restored-artifact write succeeded. Injecting
// here exercises Starting itself, rather than the earlier metadata boundary.
func TestServer_Restore_StopsBeforeUnrecordedStarting(t *testing.T) {
	skipRootPersistence(t)
	f := newResumeFixture(t)
	f.haConf = filepath.Join(t.TempDir(), "cubrid_ha.conf")
	if err := os.WriteFile(f.haConf, []byte(testHAConf), 0o600); err != nil {
		t.Fatal(err)
	}
	objects := &countingStore{fakeObjectStore: f.objects.(*fakeObjectStore)}
	f.objects = objects
	var repair func()
	var recorded *OperationArtifact
	cli := &afterCallCLI{
		volumeCLI: &volumeCLI{databases: f.databases,
			stepCLI: stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}},
		call: "cubrid " + stepHeartbeatStatus,
		after: func() {
			ops, err := f.server.store.FindByKey("restore-starting")
			if err != nil || len(ops) != 1 || ops[0].State != OpRestoring || ops[0].Restored == nil {
				t.Errorf("restored artifact was not durable before Starting injection: %v, %v", ops, err)
				return
			}
			artifact := *ops[0].Restored
			recorded = &artifact
			repair = unwritableDuringOperation(t, f.storeDir)
		},
	}
	f.start(cli)
	var logged bytes.Buffer
	f.server.store.SetLogger(slog.New(slog.NewJSONHandler(&logged, nil)))
	op := f.post("restore-starting")
	f.server.ops.Wait()
	if recorded == nil {
		t.Fatal("Starting injection did not follow a durable restored-artifact record")
	}
	reads := objects.gets
	if calls := cli.recorded(); count(calls, "cubrid restoredb ") != 1 ||
		count(calls, callServerStart) != 0 || count(calls, callHeartbeatStart) != 0 {
		t.Errorf("unrecorded Starting ran commands: %v", calls)
	}
	body, err := json.Marshal(f.req)
	if err != nil {
		t.Fatal(err)
	}
	checkTransitionFailure(t, f.server, op, OpRestoring, OpStarting, &logged, repair,
		func(h http.Handler) *httptest.ResponseRecorder {
			return persistenceRequest(h, http.MethodPost, "/v1/restore/prepare", "restore-starting", string(body))
		}, failureMetadata{restored: recorded, owner: op.ID, reason: "could not record the state Starting"})
	if calls := cli.recorded(); count(calls, "cubrid restoredb ") != 1 ||
		count(calls, callServerStart) != 0 || count(calls, callHeartbeatStart) != 0 || objects.gets != reads {
		t.Errorf("same-key replay changed restore work: commands %v, reads %d (before %d)", calls, objects.gets, reads)
	}
	f.wantRestoredDataKept()
}

// A takeover must record the previous artifact under its new operation before
// changing ownership or starting it; readable old data alone is not authority.
func TestServer_Restore_StopsBeforeUnrecordedAdoption(t *testing.T) {
	skipRootPersistence(t)
	f := newResumeFixture(t)
	f.start(&volumeCLI{databases: f.databases,
		stepCLI: stepCLI{failing: map[string]bool{stepServerStart: true}}})
	previous := f.run("restore-original")
	if previous.State != OpFailed || previous.Restored == nil || f.marker() != previous.ID {
		t.Fatalf("initial failed restore did not retain its artifact and ownership: %+v", previous)
	}
	volume := readFile(t, filepath.Join(f.databases, dbName, restoredVolume))
	registration := readFile(t, filepath.Join(f.databases, databasesTxt))
	objects := &countingStore{fakeObjectStore: f.objects.(*fakeObjectStore)}
	f.objects = objects
	cli := &volumeCLI{databases: f.databases}
	f.start(cli)
	var logged bytes.Buffer
	var repair func()
	f.server.store.SetLogger(slog.New(&afterStateRecord{Handler: slog.NewJSONHandler(&logged, nil), state: OpRestoring,
		once: func() { repair = unwritableDuringOperation(t, f.storeDir) }}))
	op := f.post("restore-adoption")
	f.server.ops.Wait()
	if calls := cli.recorded(); len(calls) != 0 || objects.gets != 0 || f.marker() != previous.ID {
		t.Errorf("unrecorded adoption changed ownership or work: calls %v, reads %d, owner %q (want %q)",
			calls, objects.gets, f.marker(), previous.ID)
	}
	body, err := json.Marshal(f.req)
	if err != nil {
		t.Fatal(err)
	}
	checkTransitionFailure(t, f.server, op, OpRestoring, OpRestoring, &logged, repair,
		func(h http.Handler) *httptest.ResponseRecorder {
			return persistenceRequest(h, http.MethodPost, "/v1/restore/prepare", "restore-adoption", string(body))
		}, failureMetadata{owner: previous.ID, reason: "restore failed:", forbidden: OpStarting})
	if calls := cli.recorded(); len(calls) != 0 || objects.gets != 0 || f.marker() != previous.ID {
		t.Errorf("same-key replay changed adoption work: commands %v, reads %d, owner %q", calls, objects.gets, f.marker())
	}
	if got := readFile(t, filepath.Join(f.databases, dbName, restoredVolume)); got != volume {
		t.Errorf("adoption changed restored volume: %q, want %q", got, volume)
	}
	if got := readFile(t, filepath.Join(f.databases, databasesTxt)); got != registration {
		t.Errorf("adoption changed database registration: %q, want %q", got, registration)
	}
	original, err := f.server.store.Get(previous.ID)
	if err != nil || !reflect.DeepEqual(original.Restored, previous.Restored) || original.State != previous.State {
		t.Errorf("previous restored record changed: %+v, %v; want %+v", original, err, previous)
	}
}
