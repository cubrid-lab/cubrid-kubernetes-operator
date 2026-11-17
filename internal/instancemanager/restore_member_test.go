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
	"slices"
	"strings"
	"testing"
)

// The member a restore in these tests names, and another member's name.
const (
	namedMember = "demo-2"
	otherMember = "demo-0"
)

// postRestore sends req to a manager whose own member name is member, and
// returns the answer, the commands it ran, the operation store's directory
// and the manager. The manager is a standalone member of the database, so an
// admitted restore can complete without an HA configuration.
func postRestore(t *testing.T, member string, req RestoreRequest, objects *countingStore) (*httptest.ResponseRecorder, *stepCLI, string, *Server) {
	t.Helper()
	storeDir := t.TempDir()
	store, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { awaitTerminalOperations(t, store) })
	roots := rootsFor(req)
	roots.Member = member
	cli := &stepCLI{}
	srv := NewServer(cli, "tok").WithOperationStore(store).WithObjectStore(objects).WithRestoreRoots(roots).
		WithStandaloneDatabase(dbName)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, pathRestorePrepare, strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer tok")
	post.Header.Set("Idempotency-Key", "named-member")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	return rr, cli, storeDir, srv
}

// A restore that names another member than the one it reached is refused
// with 409 WrongMember before the manager records an operation, reads the
// artifact, creates staging or touches the database root: the restore would
// otherwise overwrite storage nobody named (#117). A manager that cannot
// read its own name cannot tell, and refuses too.
func TestServer_Restore_RefusesAnotherMemberBeforeAnyWork(t *testing.T) {
	for _, c := range []struct {
		name, member string
	}{
		{"another member", otherMember},
		{"a manager that does not know its own name", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake, bucket, prefix := stageUploadedArtifact(t)
			objects := &countingStore{fakeObjectStore: fake}
			req := baseRestoreRequest(t, bucket, prefix)
			req.ExpectedMember = namedMember
			rr, cli, storeDir, _ := postRestore(t, c.member, req, objects)

			if rr.Code != http.StatusConflict {
				t.Fatalf("restore naming %s on member %q = %d (%s), want 409 before any work", namedMember,
					c.member, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			var answer ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			if answer.Code != CodeWrongMember || !strings.Contains(answer.Error, namedMember) {
				t.Errorf("answer = %+v, want code %s and the named member in the message", answer, CodeWrongMember)
			}
			if calls := cli.recorded(); len(calls) != 0 {
				t.Errorf("refused restore ran commands: %v", calls)
			}
			if objects.gets != 0 {
				t.Errorf("refused restore read the artifact %d times", objects.gets)
			}
			if entries, err := os.ReadDir(storeDir); err != nil || len(entries) != 0 {
				t.Errorf("refused restore changed the operation store: %v, %v", entries, err)
			}
			if _, err := os.Stat(req.StagingDir); !os.IsNotExist(err) {
				t.Errorf("refused restore created staging: %v", err)
			}
			if entries, err := os.ReadDir(req.TargetDir); err != nil || len(entries) != 0 {
				t.Errorf("refused restore changed the database root: %v, %v", entries, err)
			}
		})
	}
}

// A restore that names the member it reached is admitted as before, and so
// is one that names none, as a recovery restore into a new cluster does.
func TestServer_Restore_AdmitsItsOwnMemberOrNone(t *testing.T) {
	for _, named := range []string{namedMember, ""} {
		fake, bucket, prefix := stageUploadedArtifact(t)
		objects := &countingStore{fakeObjectStore: fake}
		req := baseRestoreRequest(t, bucket, prefix)
		req.ExpectedMember = named
		rr, _, _, srv := postRestore(t, namedMember, req, objects)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("restore naming %q on %s = %d (%s), want 202", named, namedMember, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		var op Operation
		if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		if final := pollUntilSettled(t, srv, srv.Handler(), op.ID); final.State != OpCompleted {
			t.Errorf("restore naming %q on %s ended %s (%s), want Completed", named, namedMember, final.State, final.FailureReason)
		}
	}
}

// Restore applies the same check for a caller that does not come through the
// handler, before it reads or writes anything.
func TestRestore_RefusesAnotherMember(t *testing.T) {
	fake, bucket, prefix := stageUploadedArtifact(t)
	objects := &countingStore{fakeObjectStore: fake}
	req := baseRestoreRequest(t, bucket, prefix)
	req.ExpectedMember = namedMember
	roots := rootsFor(req)
	roots.Member = otherMember
	cli := &stepCLI{}
	_, err := Restore(context.Background(), cli, objects, roots, req)
	if !errors.Is(err, ErrWrongMember) {
		t.Fatalf("Restore naming %s on %s = %v, want ErrWrongMember", namedMember, otherMember, err)
	}
	if calls := cli.recorded(); len(calls) != 0 || objects.gets != 0 {
		t.Errorf("refused restore ran %v and read the artifact %d times", calls, objects.gets)
	}
}

// The name a request carries must be a host name: it is compared with the
// member's own, never used as a path or an argument, but a value that cannot
// be a member's name is a fault of the request.
func TestValidateRestoreRequest_RefusesAMemberThatIsNoHostName(t *testing.T) {
	req := baseRestoreRequest(t, "bucket", "prefix")
	req.ExpectedMember = "../demo-0"
	if err := validateRestoreRequest(req); err == nil || !strings.Contains(err.Error(), "expectedMember") {
		t.Errorf("validateRestoreRequest(%q) = %v, want a refusal naming expectedMember", req.ExpectedMember, err)
	}
}

// GET /v1/role reports the check, so that the Operator names the member only
// to a manager that enforces it (ADR-0013).
func TestCapabilities_ReportTheMemberCheck(t *testing.T) {
	if !slices.Contains(Capabilities(), CapabilityExpectedMember) {
		t.Errorf("capabilities = %v, want %q among them", Capabilities(), CapabilityExpectedMember)
	}
}
