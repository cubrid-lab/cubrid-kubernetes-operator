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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker is the ownership marker the restore left on its database, "" for
// none.
func (r rebuildRestore) marker(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.req.TargetDir, r.req.Database, ownershipMarker))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// release posts POST /v1/rebuild/release for the restore's database with b.
func (r rebuildRestore) release(t *testing.T, b RebuildBinding) int {
	t.Helper()
	body, err := json.Marshal(RebuildReleaseRequest{Database: r.req.Database, Rebuild: b})
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, "/v1/rebuild/release", bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	r.h.ServeHTTP(rr, post)
	return rr.Code
}

// A restore for a member rebuild that completed keeps its ownership marker:
// the member is not started again after a restart until the Operator judged
// it caught up and released it with the rebuild's operation ID and token
// (ADR-0006; a member restored across a change of master can lack
// acknowledged writes, POC-57). Another token or rebuild is refused and
// changes nothing; the record keeps no token.
func TestServer_RebuildRestore_KeepsItsMarkerUntilReleased(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer()}})
	if r.final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", r.final.State, r.final.FailureReason)
	}
	if got := r.marker(t); got != r.final.ID {
		t.Fatalf("marker = %q, want it kept as %q until the release", got, r.final.ID)
	}
	if r.final.Rebuild == nil || r.final.Rebuild.OperationID != testRebuild.OperationID ||
		r.final.Rebuild.TokenSHA256 == "" {
		t.Fatalf("the record names the rebuild, got %+v", r.final.Rebuild)
	}
	if raw, _ := json.Marshal(r.final); strings.Contains(string(raw), testRebuild.Token) {
		t.Fatalf("the operation record holds the token: %s", raw)
	}

	other := testRebuild
	other.Token = "fedcba9876543210fedcba9876543210"
	if code := r.release(t, other); code != http.StatusConflict {
		t.Fatalf("a release with another token = %d, want 409", code)
	}
	another := testRebuild
	another.OperationID = "rebuild-op-2"
	if code := r.release(t, another); code != http.StatusConflict {
		t.Fatalf("a release of another rebuild = %d, want 409", code)
	}
	if got := r.marker(t); got != r.final.ID {
		t.Fatalf("a refused release changed the marker to %q", got)
	}
	if code := r.release(t, testRebuild); code != http.StatusOK {
		t.Fatalf("the release = %d, want 200", code)
	}
	if got := r.marker(t); got != "" {
		t.Fatalf("the released database still carries the marker %q", got)
	}
	if code := r.release(t, testRebuild); code != http.StatusOK {
		t.Fatalf("a repeated release = %d, want 200", code)
	}
}

// A member that is stopping, as the Operator stops the member of a failed
// rebuild, is never released.
func TestServer_RebuildRelease_RefusedOnceTheMemberIsStopping(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer()}})
	if r.final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", r.final.State, r.final.FailureReason)
	}
	_ = r.srv.Stop(t.Context(), r.req.Database)
	if code := r.release(t, testRebuild); code != http.StatusServiceUnavailable {
		t.Fatalf("a release of a stopping member = %d, want 503", code)
	}
	if got := r.marker(t); got != r.final.ID {
		t.Fatalf("the marker of a stopping member = %q, want it kept", got)
	}
}

// The source is asked again right before heartbeat is started. A source that
// is no longer the active master by then keeps the restored member from
// starting: it may lack writes the new master acknowledged (POC-57). The
// restore fails with RebuildSourceNotMaster and keeps the restored data and
// its marker.
func TestServer_RebuildRestore_RefusesTheStartWhenTheSourceChanged(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer(), then: slaveAnswer()}})
	if r.final.State != OpFailed || r.final.FailureCode != FailureRebuildSourceNotMaster {
		t.Fatalf("final = %s %s (%s), want Failed with %s", r.final.State, r.final.FailureCode,
			r.final.FailureReason, FailureRebuildSourceNotMaster)
	}
	if r.final.Restored == nil {
		t.Fatalf("the restore failed after restoredb, so it records the data as restored")
	}
	if calls := r.cli.recorded(); count(calls, callHeartbeatStart) != 0 {
		t.Fatalf("heartbeat was started although the source was no longer the master: %q", calls)
	}
	if got := r.marker(t); got != r.final.ID {
		t.Fatalf("marker = %q, want it kept", got)
	}
}

// After a restart of the member, a completed rebuild restore that was not
// released is not started: ResumeCompletedRestores leaves it and its marker,
// and the image's entrypoint starts no database that carries the marker.
func TestResumeCompletedRestores_DoesNotStartAnUnreleasedRebuild(t *testing.T) {
	r := runRebuildRestore(t, rebuildCase{request: &testRebuild, manifest: &testRebuild,
		source: &sourceIM{answer: activeMasterAnswer()}})
	if r.final.State != OpCompleted {
		t.Fatalf("final = %s (%s), want Completed", r.final.State, r.final.FailureReason)
	}
	cli := &stepCLI{failing: map[string]bool{stepHeartbeatStatus: true}}
	restarted := NewServer(cli, "tok").WithOperationStore(r.store).WithRestoreRoots(r.srv.restoreRoots).
		WithHAConfig(r.srv.ha)
	restarted.heartbeatProcesses, restarted.heartbeatSettle = processTable(), 0
	if err := restarted.ResumeCompletedRestores(t.Context()); err != nil {
		t.Fatalf("ResumeCompletedRestores: %v", err)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Fatalf("the restarted member ran %q for an unreleased rebuild", calls)
	}
	if got := r.marker(t); got != r.final.ID {
		t.Fatalf("marker = %q, want it kept", got)
	}
}

// A release while the restore of the rebuild still runs is refused and
// changes nothing: only a completed restore is released.
func TestServer_RebuildRelease_RefusedWhileTheRestoreRuns(t *testing.T) {
	target := t.TempDir()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.FindOrCreate(OpRestore, "rebuild-restore", "hash", dbName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(op.ID, func(o *Operation) {
		o.State = OpRestoring
		o.Rebuild = operationRebuild(&testRebuild)
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, dbName, ownershipMarker)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(op.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&stepCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: target})
	r := rebuildRestore{req: RestoreRequest{Database: dbName, TargetDir: target}, h: srv.Handler()}
	if code := r.release(t, testRebuild); code != http.StatusConflict {
		t.Fatalf("a release while the restore runs = %d, want 409", code)
	}
	if got := r.marker(t); got != op.ID {
		t.Fatalf("marker = %q, want it kept", got)
	}
}
