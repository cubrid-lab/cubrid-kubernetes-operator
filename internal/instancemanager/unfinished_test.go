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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roleAnswer asks s for its role and decodes the answer.
func roleAnswer(t *testing.T, s *Server) HAStatus {
	t.Helper()
	rr := doReq(t, s.Handler(), "/v1/role", "tok", testRemoteAddr)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/role = %d: %s", rr.Code, rr.Body.String())
	}
	var st HAStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// writeMarker leaves a database directory with a volume and the given marker.
func writeMarker(t *testing.T, databases, marker string) string {
	t.Helper()
	dir := filepath.Join(databases, dbName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{ownershipMarker: marker, dbName + "_vinf": "data"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The role answer reports a database that carries the ownership marker, and
// says whether the next operation of this manager takes it up or a person
// has to. The role is reported as it is, and nothing is removed or cleared.
func TestServer_Role_ReportsAnUnfinishedDatabase(t *testing.T) {
	tests := []struct {
		name string
		// state of the recorded operation the marker names; empty writes
		// marker as it is.
		state    OperationState
		restored bool
		marker   string
		want     UnfinishedState
		reason   string
	}{
		{name: "a failed operation", state: OpFailed, want: UnfinishedWaiting, reason: "next"},
		{name: "a completed operation", state: OpCompleted, want: UnfinishedWaiting, reason: "next"},
		{name: "a failed restore with restored data", state: OpFailed, restored: true, want: UnfinishedRefused,
			reason: "restored"},
		{name: "an operation this manager does not know", marker: "op-00000000000000000000000000000000\n",
			want: UnfinishedRefused, reason: "does not know"},
		{name: "an unreadable marker", marker: "../../etc\n", want: UnfinishedRefused, reason: "unreadable"},
		{name: "the entrypoint's createdb", marker: "entrypoint-createdb\n", want: UnfinishedRefused,
			reason: "entrypoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			databases := t.TempDir()
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var dir, id string
			if tc.state != "" {
				dir = leaveInterrupted(t, store, databases, OpRestore, tc.state)
				data, err := os.ReadFile(filepath.Join(dir, ownershipMarker))
				if err != nil {
					t.Fatal(err)
				}
				id = strings.TrimSpace(string(data))
				if tc.restored {
					if _, err := store.Update(id, func(o *Operation) { o.Restored = &OperationArtifact{Database: dbName} }); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				dir = writeMarker(t, databases, tc.marker)
				if op := strings.TrimSpace(tc.marker); isValidOperationID(op) {
					id = op
				}
			}
			s := NewServer(fakeCLI{out: masterOut}, "tok").WithOperationStore(store).
				WithRestoreRoots(RestoreRoots{Target: databases}).WithReplication(dbName, databases)

			st := roleAnswer(t, s)
			if st.Role != RoleMaster {
				t.Errorf("role = %q, want master: the marker hides no role", st.Role)
			}
			u := st.Unfinished
			if u == nil {
				t.Fatalf("unfinished = nil, want %s", tc.want)
			}
			if u.Database != dbName || u.State != tc.want || u.Operation != id || !strings.Contains(u.Reason, tc.reason) {
				t.Errorf("unfinished = %+v, want database %s, state %s, operation %q and a reason mentioning %q",
					u, dbName, tc.want, id, tc.reason)
			}
			marker, err := os.ReadFile(filepath.Join(dir, ownershipMarker))
			if err != nil {
				t.Fatalf("the marker is gone after a role answer: %v", err)
			}
			if tc.marker != "" && string(marker) != tc.marker {
				t.Errorf("marker = %q, want it left as %q", marker, tc.marker)
			}
			if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
				t.Errorf("the volume is gone after a role answer: %v", err)
			}
		})
	}
}

// A standalone member reports its database as an HA member does.
func TestServer_Role_ReportsTheUnfinishedDatabaseOfAStandaloneMember(t *testing.T) {
	databases := t.TempDir()
	writeMarker(t, databases, "entrypoint-createdb\n")
	s := NewServer(fakeCLI{}, "tok").WithRestoreRoots(RestoreRoots{Target: databases}).WithStandaloneDatabase(dbName)
	if u := roleAnswer(t, s).Unfinished; u == nil || u.State != UnfinishedRefused || u.Database != dbName {
		t.Errorf("unfinished = %+v, want %s refused", u, dbName)
	}
}

// A database without the marker, a missing database root and a member that
// was not told its database report nothing.
func TestServer_Role_ReportsNoUnfinishedDatabaseWithoutAMarker(t *testing.T) {
	databases := t.TempDir()
	dir := filepath.Join(databases, dbName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dbName+"_vinf"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	marked := t.TempDir()
	writeMarker(t, marked, "entrypoint-createdb\n")
	for name, s := range map[string]*Server{
		"no marker": NewServer(fakeCLI{out: masterOut}, "tok").
			WithRestoreRoots(RestoreRoots{Target: databases}).WithReplication(dbName, databases),
		"no database root": NewServer(fakeCLI{out: masterOut}, "tok").
			WithRestoreRoots(RestoreRoots{Target: filepath.Join(databases, "missing")}).WithReplication(dbName, databases),
		"no database": NewServer(fakeCLI{out: masterOut}, "tok").WithRestoreRoots(RestoreRoots{Target: marked}),
	} {
		if u := roleAnswer(t, s).Unfinished; u != nil {
			t.Errorf("%s: unfinished = %+v, want none", name, u)
		}
	}
}

// A role answer only observes: the record of the operation a marker names is
// not written, neither to fail an operation the start could not read and
// that has not ended, nor to write an end that could not be written before.
// The answer judges both as the next read of the record will: failed.
func TestServer_Role_UnfinishedReportWritesNoOperationRecord(t *testing.T) {
	tests := map[string]struct {
		state OperationState
		// leave turns the record into the case to report, and returns the
		// store the role answer reads.
		leave func(t *testing.T, storeDir string, store *OperationStore, id string) *OperationStore
	}{
		"a record the start could not read": {
			state: OpCreating,
			leave: func(t *testing.T, storeDir string, _ *OperationStore, id string) *OperationStore {
				repair := overwrite("{")(t, filepath.Join(storeDir, id+".json"))
				reopened, err := NewOperationStore(storeDir)
				if err != nil {
					t.Fatal(err)
				}
				repair()
				return reopened
			},
		},
		"an end that could not be written": {
			state: OpRestoring,
			leave: func(t *testing.T, storeDir string, store *OperationStore, id string) *OperationStore {
				restore := unwritable(t, storeDir)
				if err := store.Settle(id, func(o *Operation) { o.State = OpFailed }); err == nil {
					t.Fatal("Settle reported no error although the store cannot be written")
				}
				restore()
				return store
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			databases, storeDir := t.TempDir(), t.TempDir()
			store, err := NewOperationStore(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			dir := leaveInterrupted(t, store, databases, OpRestore, tc.state)
			data, err := os.ReadFile(filepath.Join(dir, ownershipMarker))
			if err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSpace(string(data))
			store = tc.leave(t, storeDir, store, id)
			record := filepath.Join(storeDir, id+".json")
			before, err := os.ReadFile(record) // #nosec G304 -- a file of the test
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer(fakeCLI{out: masterOut}, "tok").WithOperationStore(store).
				WithRestoreRoots(RestoreRoots{Target: databases}).WithReplication(dbName, databases)

			u := roleAnswer(t, s).Unfinished
			if u == nil || u.State != UnfinishedWaiting || u.Operation != id || !strings.Contains(u.Reason, "failed") {
				t.Errorf("unfinished = %+v, want operation %s waiting as failed", u, id)
			}
			after, err := os.ReadFile(record) // #nosec G304 -- a file of the test
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("the role answer wrote the operation record:\nbefore %s\nafter  %s", before, after)
			}
			if got, err := store.Get(id); err != nil || got.State != OpFailed {
				t.Errorf("Get after the role answer = %+v, %v; want the record failed by the store's own read", got, err)
			}
		})
	}
}

// An operation that runs now leaves its marker until it completes: that is
// its work in progress, which the bootstrap or restore status reports, not a
// database left unfinished.
func TestServer_Role_ReportsNoUnfinishedDatabaseForARunningOperation(t *testing.T) {
	databases := t.TempDir()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaveInterrupted(t, store, databases, OpRestore, OpCreating)
	s := NewServer(fakeCLI{out: masterOut}, "tok").WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: databases}).WithReplication(dbName, databases)
	if u := roleAnswer(t, s).Unfinished; u != nil {
		t.Errorf("unfinished = %+v, want none for an operation that runs", u)
	}
}

// A database root that cannot be opened for another reason than its absence
// is not taken for a database without the marker: it cannot be judged now.
func TestServer_Role_ReportsADatabaseRootThatCannotBeOpened(t *testing.T) {
	databases := t.TempDir()
	notADirectory := filepath.Join(databases, "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(fakeCLI{out: masterOut}, "tok").
		WithRestoreRoots(RestoreRoots{Target: notADirectory}).WithReplication(dbName, databases)
	u := roleAnswer(t, s).Unfinished
	if u == nil || u.State != UnfinishedRefused || !strings.Contains(u.Reason, "cannot be judged now") {
		t.Errorf("unfinished = %+v, want refused as cannot be judged now", u)
	}
}

// A record that cannot be read now is not said to need a person: the
// report says that the database cannot be judged now, and why.
func TestServer_Role_ReportsARecordThatCannotBeReadNow(t *testing.T) {
	databases, storeDir := t.TempDir(), t.TempDir()
	store, err := NewOperationStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := leaveInterrupted(t, store, databases, OpRestore, OpFailed)
	data, err := os.ReadFile(filepath.Join(dir, ownershipMarker))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(data))
	t.Cleanup(overwrite("{")(t, filepath.Join(storeDir, id+".json")))
	s := NewServer(fakeCLI{out: masterOut}, "tok").WithOperationStore(store).
		WithRestoreRoots(RestoreRoots{Target: databases}).WithReplication(dbName, databases)
	u := roleAnswer(t, s).Unfinished
	if u == nil || u.State != UnfinishedRefused || u.Operation != id || !strings.Contains(u.Reason, "cannot be judged now") {
		t.Errorf("unfinished = %+v, want operation %s refused as cannot be judged now", u, id)
	}
}
