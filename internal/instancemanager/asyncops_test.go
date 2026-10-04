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
	"testing"
	"time"
)

const testRemoteAddr = "10.0.0.1:5000"

func newAsyncServer(t *testing.T, cli CLI) http.Handler {
	t.Helper()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	// Backup and restore answer 202 and finish in a goroutine that keeps
	// recording state transitions, each one a temp file created in the store
	// directory and renamed into place. If the test returns first, t.TempDir's
	// RemoveAll races that goroutine and fails with "directory not empty"
	// (#130). Cleanups run last-in first-out, so this one runs before the
	// TempDir removal registered above.
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	t.Cleanup(func() { _ = os.RemoveAll(testStagingRoot) })
	return NewServer(cli, "tok").WithOperationStore(store).WithBackupStagingRoot(testStagingRoot).Handler()
}

// awaitTerminalOperations blocks until every operation in store has reached a
// terminal state. The terminal transition is the last write an operation makes
// to the store, so once it is visible nothing else will create files there.
// An operation that never terminates fails the test with its ID and state
// rather than surfacing later as an opaque TempDir cleanup error.
func awaitTerminalOperations(t *testing.T, store *OperationStore) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		ops, err := store.listLocked()
		store.mu.Unlock()
		if err != nil {
			t.Errorf("list operations: %v", err)
			return
		}
		var running []string
		for _, op := range ops {
			if !op.State.IsTerminal() {
				running = append(running, op.ID+"="+string(op.State))
			}
		}
		if len(running) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("operations still running when the test ended: %v", running)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postBackup(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/backup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestServer_Backup_RequiresIdempotencyKey(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	rr := postBackup(t, h, "", `{"database":"appdb","destination":"`+testStagingRoot+`/bk"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key = %d, want 400", rr.Code)
	}
}

func TestServer_Backup_AsyncReturnsOperation(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"`+testStagingRoot+`/bk"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/backup = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !isValidOperationID(op.ID) {
		t.Errorf("operation id = %q, not a valid server-generated id", op.ID)
	}
}

func TestServer_Backup_SameKeySameBodyIsIdempotent(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	body := `{"database":"appdb","destination":"` + testStagingRoot + `/bk"}`
	first := postBackup(t, h, "key-1", body)
	second := postBackup(t, h, "key-1", body)
	if second.Code != http.StatusAccepted {
		t.Fatalf("repeat = %d, want 202", second.Code)
	}
	var a, b Operation
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Errorf("idempotent repeat returned a different op: %s != %s", a.ID, b.ID)
	}
}

func TestServer_Backup_SameKeyDifferentBodyConflicts(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	postBackup(t, h, "key-1", `{"database":"appdb","destination":"`+testStagingRoot+`/bk"}`)
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"`+testStagingRoot+`/OTHER"}`)
	if rr.Code != http.StatusConflict {
		t.Errorf("same key diff body = %d, want 409", rr.Code)
	}
}

func TestServer_Backup_NoFalseCompletion(t *testing.T) {
	// A successful backupdb must NOT become Completed without upload+manifest.
	h := newAsyncServer(t, fakeCLI{out: "Backup Volume Label: Level: 0"})
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"`+testStagingRoot+`/bk"}`)
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)

	final := pollUntilTerminal(t, h, op.ID)
	if final.State == OpCompleted {
		t.Errorf("bare backup reached Completed without upload+manifest: %+v", final)
	}
	if final.State != OpFailed {
		t.Errorf("final state = %s, want Failed (upload not implemented)", final.State)
	}
}

func TestServer_Backup_CompletesAfterUpload(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	objStore := newFakeStore()
	// Pre-stage a backup file at the destination, since the fake CLI does not
	// actually run backupdb.
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	h := NewServer(fakeCLI{out: "ok"}, "tok").
		WithOperationStore(store).
		WithObjectStore(objStore).
		WithBackupStagingRoot(filepath.Dir(staging)).
		Handler()

	body := `{"database":"appdb","destination":"` + staging + `","upload":{"bucket":"cubrid-backups","prefix":"prod/uid-1","clusterUID":"cuid","cubridVersion":"11.4.6","sourceInstance":"appdb-1","sourceRole":"slave"}}`
	rr := postBackup(t, h, "key-1", body)
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)

	final := pollUntilTerminal(t, h, op.ID)
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (reason %q), want Completed", final.State, final.FailureReason)
	}
	if final.Artifact == nil || final.Artifact.ManifestURI != "s3://cubrid-backups/prod/uid-1/manifest.json" {
		t.Errorf("artifact = %+v", final.Artifact)
	}
	if _, ok := objStore.objects["cubrid-backups/prod/uid-1/manifest.json"]; !ok {
		t.Error("manifest.json was not uploaded on completion")
	}
}

func TestServer_GetOperation_NotFound(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	req := httptest.NewRequest("GET", "/v1/operations/op-00000000000000000000000000000000", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown operation = %d, want 404", rr.Code)
	}
}

func TestServer_Convergence_RequiresParams(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: convergedApplyinfo})
	req := httptest.NewRequest("GET", "/v1/ha/convergence", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing params = %d, want 400", rr.Code)
	}
}

func TestServer_Convergence_ReportsFacts(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: convergedApplyinfo})
	req := httptest.NewRequest("GET", "/v1/ha/convergence?database=appdb&copiedLogPath=/x", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/ha/convergence = %d, want 200", rr.Code)
	}
	var c ApplyConvergence
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.Converged() {
		t.Errorf("convergence = %+v, want converged", c)
	}
}

func pollUntilTerminal(t *testing.T, h http.Handler, id string) Operation {
	t.Helper()
	for range 100 {
		req := httptest.NewRequest("GET", "/v1/operations/"+id, nil)
		req.Header.Set("Authorization", "Bearer tok")
		req.RemoteAddr = testRemoteAddr
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var op Operation
		if err := json.Unmarshal(rr.Body.Bytes(), &op); err == nil && op.State.IsTerminal() {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach a terminal state", id)
	return Operation{}
}

// testStagingRoot is the backup staging root of the handler tests. The
// manager creates request directories below it, so it has to be writable.
var testStagingRoot = filepath.Join(os.TempDir(), "im-test-staging")

// stagingCLI records whether the -D directory of backupdb existed when the
// command ran, and can fail the command.
type stagingCLI struct {
	err     error
	sawDir  bool
	ranWith string
}

func (c *stagingCLI) Run(_ context.Context, _ string, args ...string) (string, error) {
	for i, a := range args {
		if a == "-D" && i+1 < len(args) {
			c.ranWith = args[i+1]
			info, err := os.Stat(args[i+1])
			c.sawDir = err == nil && info.IsDir()
		}
	}
	return "ok", c.err
}

// backupdb exits 1 when its destination directory does not exist
// (docs/poc/RESULTS.md, POC-12), and nothing else creates it on the volume.
func TestServer_Backup_CreatesTheStagingDirectory(t *testing.T) {
	for _, async := range []bool{false, true} {
		// Neither the staging root nor the request's directory exists yet.
		root := filepath.Join(t.TempDir(), "backup-staging")
		staging := filepath.Join(root, "uid-1")
		cli := &stagingCLI{}
		srv := NewServer(cli, "tok").WithBackupStagingRoot(root)
		if async {
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewOperationStore: %v", err)
			}
			srv = srv.WithOperationStore(store)
		}
		h := srv.Handler()
		body, _ := json.Marshal(BackupRequest{Database: dbName, Destination: staging})
		rr := postBackup(t, h, "key-1", string(body))
		if async {
			var op Operation
			_ = json.Unmarshal(rr.Body.Bytes(), &op)
			pollUntilTerminal(t, h, op.ID)
		}
		if cli.ranWith != staging || !cli.sawDir {
			t.Errorf("async=%v: backupdb ran with -D %q, directory existed: %v", async, cli.ranWith, cli.sawDir)
		}
	}
}

// A backup that fails removes the directory it created.
func TestServer_Backup_FailureRemovesTheDirectoryItCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup-staging")
	staging := filepath.Join(root, "uid-1")
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	cli := &stagingCLI{err: errors.New("exit status 1")}
	h := NewServer(cli, "tok").WithOperationStore(store).WithBackupStagingRoot(root).Handler()
	body, _ := json.Marshal(BackupRequest{Database: dbName, Destination: staging})
	rr := postBackup(t, h, "key-1", string(body))
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)
	if final := pollUntilTerminal(t, h, op.ID); final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if !cli.sawDir {
		t.Error("the directory did not exist when backupdb ran")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging directory is still there after the failure (err=%v)", err)
	}
}

// failingCLI fails every command, like a backupdb that cannot run.
type failingCLI struct{ calls int }

func (f *failingCLI) Run(_ context.Context, _ string, _ ...string) (string, error) {
	f.calls++
	return "backupdb: no space left", errors.New("exit status 1")
}

func TestServer_Backup_RejectsDestinationOutsideStagingRoot(t *testing.T) {
	root := t.TempDir()
	for _, destination := range []string{
		"/etc",                         // another tree
		root,                           // the root itself
		filepath.Join(root, "a", "b"),  // deeper than one directory
		filepath.Join(root, "..", "x"), // escapes through ..
		filepath.Join(root, "..bad"),   // not a plain name
		"relative/path",
	} {
		store, err := NewOperationStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewOperationStore: %v", err)
		}
		cli := &failingCLI{}
		h := NewServer(cli, "tok").WithOperationStore(store).WithBackupStagingRoot(root).Handler()
		body, _ := json.Marshal(BackupRequest{Database: dbName, Destination: destination})
		rr := postBackup(t, h, "key-1", string(body))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("destination %q = %d, want 400 (body %s)", destination, rr.Code, rr.Body.String())
		}
		if cli.calls != 0 {
			t.Errorf("destination %q: %d command(s) ran before the check", destination, cli.calls)
		}
	}
}

func TestServer_Backup_RequiresConfiguredStagingRoot(t *testing.T) {
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	h := NewServer(fakeCLI{out: "ok"}, "tok").WithOperationStore(store).Handler()
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"`+testStagingRoot+`/bk"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("backup without a staging root = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestServer_Backup_FailureRemovesStaging(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "uid-1")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "partial_bk0v000"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	h := NewServer(&failingCLI{}, "tok").WithOperationStore(store).WithBackupStagingRoot(root).Handler()
	body, _ := json.Marshal(BackupRequest{Database: dbName, Destination: staging})
	rr := postBackup(t, h, "key-1", string(body))
	var op Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &op)

	final := pollUntilTerminal(t, h, op.ID)
	if final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging directory is still there after the failure (err=%v)", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the staging root itself must stay: %v", err)
	}
}
