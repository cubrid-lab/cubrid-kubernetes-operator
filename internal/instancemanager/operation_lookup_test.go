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
	"net/url"
	"os"
	"testing"
)

// lookupOperations asks GET /v1/operations?idempotencyKey=key.
func lookupOperations(t *testing.T, h http.Handler, key, token string) (*httptest.ResponseRecorder, []Operation) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/operations?"+url.Values{"idempotencyKey": {key}}.Encode(), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = testRemoteAddr
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body struct {
		Operations []Operation `json:"operations"`
	}
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rr.Body.String(), err)
		}
	}
	return rr, body.Operations
}

func newLookupServer(t *testing.T, cli CLI) (http.Handler, *OperationStore) {
	t.Helper()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	t.Cleanup(func() { _ = os.RemoveAll(testStagingRoot) })
	h := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(newFakeStore()).
		WithBackupStagingRoot(testStagingRoot).Handler()
	return h, store
}

// The operator looks a backup up by its idempotency key when it cannot send
// the start again (#365). The lookup answers what the store holds and starts
// nothing: no command, no operation.
func TestServer_LookupByIdempotencyKey_StartsNothing(t *testing.T) {
	cli := &recordingCLI{}
	h, store := newLookupServer(t, cli)

	rr, ops := lookupOperations(t, h, "cubridbackup:ns:bk:uid-1", "tok")
	if rr.Code != http.StatusOK {
		t.Fatalf("lookup of an unknown key = %d (%s), want 200 with no operation", rr.Code, rr.Body.String())
	}
	if len(ops) != 0 {
		t.Errorf("lookup of an unknown key returned %v", ops)
	}
	if len(cli.calls) != 0 {
		t.Errorf("a lookup ran commands: %v", cli.calls)
	}
	store.mu.Lock()
	stored, err := store.listLocked()
	store.mu.Unlock()
	if err != nil || len(stored) != 0 {
		t.Errorf("a lookup recorded operations: %v (err %v)", stored, err)
	}
}

func TestServer_LookupByIdempotencyKey_ReturnsTheStartedOperation(t *testing.T) {
	cli := &recordingCLI{}
	h, _ := newLookupServer(t, cli)
	const key = "cubridbackup:ns:bk:uid-1"

	rr := postBackup(t, h, key, backupBody(testStagingRoot+"/bk"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("/v1/backup = %d (%s)", rr.Code, rr.Body.String())
	}
	var started Operation
	_ = json.Unmarshal(rr.Body.Bytes(), &started)
	pollUntilTerminal(t, h, started.ID)
	commands := len(cli.calls)

	rr, ops := lookupOperations(t, h, key, "tok")
	if rr.Code != http.StatusOK || len(ops) != 1 {
		t.Fatalf("lookup = %d %v, want the started operation", rr.Code, ops)
	}
	if ops[0].ID != started.ID || ops[0].Kind != OpBackup || ops[0].IdempotencyKey != key {
		t.Errorf("lookup returned %+v, want operation %s", ops[0], started.ID)
	}
	if len(cli.calls) != commands {
		t.Errorf("the lookup ran commands: %v", cli.calls[commands:])
	}

	// Another key is not this operation.
	if _, other := lookupOperations(t, h, key+"-other", "tok"); len(other) != 0 {
		t.Errorf("lookup of another key returned %v", other)
	}
}

func TestServer_LookupByIdempotencyKey_Refusals(t *testing.T) {
	h, _ := newLookupServer(t, &recordingCLI{})
	if rr, _ := lookupOperations(t, h, "k", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("lookup without a token = %d, want 401", rr.Code)
	}
	if rr, _ := lookupOperations(t, h, "", "tok"); rr.Code != http.StatusBadRequest {
		t.Errorf("lookup without a key = %d, want 400", rr.Code)
	}

	// A manager without a store cannot say that an operation does not
	// exist, so it does not answer as if none did.
	noStore := NewServer(&recordingCLI{}, "tok").Handler()
	if rr, _ := lookupOperations(t, noStore, "k", "tok"); rr.Code == http.StatusOK {
		t.Errorf("lookup on a manager without a store = 200, want an error")
	}
}
