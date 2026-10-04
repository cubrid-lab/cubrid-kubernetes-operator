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
}

func (c *stepCLI) Run(_ context.Context, name string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	if len(args) > 1 && c.failing[args[0]+" "+args[1]] {
		return "++ cubrid " + args[0] + " " + args[1] + ": fail", errors.New("exit status 1")
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
func runRestoreOperation(t *testing.T, cli CLI, standalone bool) Operation {
	t.Helper()
	objects, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithRestoreRoots(rootsFor(req))
	if standalone {
		srv = srv.WithStandaloneDatabase(dbName)
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

const callServerStart = "cubrid server start " + dbName

// In a recovery bootstrap the entrypoint starts no database and runs only
// once, so the restore has to leave a standalone server running (#178).
func TestServer_Restore_StartsTheStandaloneServerBeforeCompleting(t *testing.T) {
	cli := &stepCLI{}
	final := runRestoreOperation(t, cli, true)
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := cli.recorded()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "cubrid restoredb ") || calls[1] != callServerStart {
		t.Errorf("calls = %q, want restoredb then %q", calls, callServerStart)
	}
}

// A restore whose server does not start is not a completed restore.
func TestServer_Restore_FailedServerStartFailsTheOperation(t *testing.T) {
	cli := &stepCLI{failing: map[string]bool{"server start": true}}
	final := runRestoreOperation(t, cli, true)
	if final.State != OpFailed {
		t.Fatalf("final state = %s, want Failed", final.State)
	}
	if !strings.Contains(final.FailureReason, "server start failed") {
		t.Errorf("failure reason = %q", final.FailureReason)
	}
}

// An HA member is started by the HA bootstrap, not by the restore.
func TestServer_Restore_DoesNotStartAnHAMember(t *testing.T) {
	cli := &stepCLI{}
	final := runRestoreOperation(t, cli, false)
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	for _, call := range cli.recorded() {
		if strings.Contains(call, "server start") {
			t.Errorf("an HA member's restore started a server: %q", cli.recorded())
		}
	}
}
