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
	"strings"
	"testing"
	"time"
)

// unwritable makes the store's directory refuse new files, as a full or
// read-only volume would, and returns the way back. The records in it stay
// readable. It skips the test for a user the mode does not hold for.
func unwritable(t *testing.T, dir string) (restore func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("a directory mode does not keep root from writing")
	}
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- a directory of the test
		t.Fatal(err)
	}
	restore = func() {
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory of the test
			t.Fatal(err)
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
	ops, err := f.store.listLocked()
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
	dir := t.TempDir()
	store, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var restore func()
	store.SetLogger(slog.New(&onCreated{
		Handler: slog.NewJSONHandler(&bytes.Buffer{}, nil),
		once:    func() { restore = unwritable(t, dir) },
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
