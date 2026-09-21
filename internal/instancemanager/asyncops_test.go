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
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return NewServer(cli, "tok").WithOperationStore(store).Handler()
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
	rr := postBackup(t, h, "", `{"database":"appdb","destination":"/tmp/bk"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key = %d, want 400", rr.Code)
	}
}

func TestServer_Backup_AsyncReturnsOperation(t *testing.T) {
	h := newAsyncServer(t, fakeCLI{out: "ok"})
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"/tmp/bk"}`)
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
	body := `{"database":"appdb","destination":"/tmp/bk"}`
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
	postBackup(t, h, "key-1", `{"database":"appdb","destination":"/tmp/bk"}`)
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"/tmp/OTHER"}`)
	if rr.Code != http.StatusConflict {
		t.Errorf("same key diff body = %d, want 409", rr.Code)
	}
}

func TestServer_Backup_NoFalseCompletion(t *testing.T) {
	// A successful backupdb must NOT become Completed without upload+manifest.
	h := newAsyncServer(t, fakeCLI{out: "Backup Volume Label: Level: 0"})
	rr := postBackup(t, h, "key-1", `{"database":"appdb","destination":"/tmp/bk"}`)
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
