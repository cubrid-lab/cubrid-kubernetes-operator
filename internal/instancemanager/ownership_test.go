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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const otherDatabaseLine = "otherdb\t\t/data/otherdb\tlocalhost\t/data/otherdb\tfile:/data/otherdb/lob\n"

// leaveInterrupted lays out what an operation interrupted in the middle of
// createdb or restoredb leaves: a recorded operation in the given state, and a
// database directory marked with its ID that holds a partial volume.
func leaveInterrupted(t *testing.T, store *OperationStore, databases string, kind OperationKind, state OperationState) string {
	t.Helper()
	op, _, err := store.FindOrCreate(kind, "interrupted", "hash", dbName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(op.ID, func(o *Operation) { o.State = state }); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(databases, dbName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{ownershipMarker: op.ID + "\n", dbName + "_vinf": "partial"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A bootstrap interrupted during createdb (the manager restarted, so the
// operation is recorded as Failed) is taken up by the next attempt: what the
// interrupted one left is removed and the database is created (#107).
func TestHABootstrap_ResumesAfterAnInterruptedCreatedb(t *testing.T) {
	f := newHAFixture(t)
	dir := leaveInterrupted(t, f.store, f.databases, OpHABootstrap, OpFailed)
	// The interrupted createdb had already registered the database.
	entry := dbName + "\t\t" + dir + "\thosts\n"
	if err := os.WriteFile(filepath.Join(f.databases, databasesTxt), []byte(otherDatabaseLine+entry), 0o600); err != nil {
		t.Fatal(err)
	}

	final := f.run("boot-2")
	if final.State != OpCompleted {
		t.Fatalf("final state = %s (%s), want Completed", final.State, final.FailureReason)
	}
	calls := f.cli.recorded()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "cubrid createdb ") || calls[1] != callHeartbeatStart {
		t.Errorf("calls = %q, want createdb then heartbeat start", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); !os.IsNotExist(err) {
		t.Errorf("the partial volume of the interrupted attempt is still there (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ownershipMarker)); !os.IsNotExist(err) {
		t.Errorf("the marker is still there after a successful createdb (err=%v)", err)
	}
}

// A marker that cannot be tied to a failed operation of this manager protects
// the directory: nothing is removed and nothing is created over it.
func TestHABootstrap_DoesNotReclaimWhatItCannotProveItOwns(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		want   string
	}{
		{"an operation this manager does not know", "op-00000000000000000000000000000000\n", "does not know"},
		{"an unreadable marker", "../../etc\n", "unreadable ownership marker"},
		// The value the image's entrypoint writes around its own createdb.
		{"the entrypoint's createdb", "entrypoint-createdb\n", "unreadable ownership marker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHAFixture(t)
			dir := filepath.Join(f.databases, dbName)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{ownershipMarker: tc.marker, dbName + "_vinf": "data"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			final := f.run("boot-1")
			if final.State != OpFailed || !strings.Contains(final.FailureReason, tc.want) {
				t.Fatalf("final = %s (%s), want Failed mentioning %q", final.State, final.FailureReason, tc.want)
			}
			if calls := f.cli.recorded(); len(calls) != 0 {
				t.Errorf("commands ran: %q", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
				t.Errorf("the existing file was removed: %v", err)
			}
		})
	}
}

// reclaimIncomplete for a restore: the interrupted restore's directory and its
// databases.txt line go, every other line stays.
func TestReclaimIncomplete_RemovesOnlyTheInterruptedDatabase(t *testing.T) {
	databases := t.TempDir()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := leaveInterrupted(t, store, databases, OpRestore, OpFailed)
	entry := dbName + "\t\t" + dir + "\thost\n"
	file := filepath.Join(databases, databasesTxt)
	if err := os.WriteFile(file, []byte(otherDatabaseLine+entry), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})

	if err := s.reclaimIncomplete(dbName); err != nil {
		t.Fatalf("reclaimIncomplete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the interrupted database directory is still there (err=%v)", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != otherDatabaseLine {
		t.Errorf("databases.txt = %q, want only the other database's line", data)
	}
}

// A completed operation whose marker was left behind has whole data: the
// marker is cleared and nothing is removed. No marker means no claim at all.
func TestReclaimIncomplete_KeepsCompleteAndUnmarkedData(t *testing.T) {
	t.Run("completed operation", func(t *testing.T) {
		databases := t.TempDir()
		store, err := NewOperationStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dir := leaveInterrupted(t, store, databases, OpRestore, OpCompleted)
		s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})
		if err := s.reclaimIncomplete(dbName); err != nil {
			t.Fatalf("reclaimIncomplete: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
			t.Errorf("complete data was removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ownershipMarker)); !os.IsNotExist(err) {
			t.Errorf("the marker of a completed operation is still there (err=%v)", err)
		}
	})
	t.Run("no marker", func(t *testing.T) {
		databases := t.TempDir()
		store, err := NewOperationStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(databases, dbName)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dbName+"_vinf"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})
		if err := s.reclaimIncomplete(dbName); err != nil {
			t.Fatalf("reclaimIncomplete: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
			t.Errorf("unmarked data was removed: %v", err)
		}
	})
}

// A restore marks its target while restoredb runs and leaves the mark after
// it has succeeded: the operation clears it once it has completed (#267).
func TestRestore_MarksItsTargetWhileRestoredbRuns(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	marker := filepath.Join(req.TargetDir, dbName, ownershipMarker)
	var during string
	cli := &restoreCLI{}
	cli.onRun = func() {
		data, _ := os.ReadFile(marker)
		during = strings.TrimSpace(string(data))
	}
	roots := rootsFor(req)
	roots.Owner = "op-0123456789abcdef0123456789abcdef"
	if _, err := Restore(t.Context(), cli, store, roots, req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if during != roots.Owner {
		t.Errorf("marker while restoredb ran = %q, want %q", during, roots.Owner)
	}
	if data, _ := os.ReadFile(marker); strings.TrimSpace(string(data)) != roots.Owner {
		t.Errorf("marker after restoredb = %q, want it kept as %q", data, roots.Owner)
	}
}

// A removal that stops partway, as a crash during reclaimIncomplete would, must
// not leave a registered directory without its marker: the next attempt has
// to be able to finish the reclaim instead of refusing an unowned database
// (#298).
func TestReclaimIncomplete_FinishesAfterARemovalStoppedPartway(t *testing.T) {
	databases := t.TempDir()
	store, err := NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := leaveInterrupted(t, store, databases, OpRestore, OpFailed)
	file := filepath.Join(databases, databasesTxt)
	if err := os.WriteFile(file, []byte(otherDatabaseLine+dbName+"\t\t"+dir+"\thost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})

	// The removal takes the marker first and stops there.
	real := removeTree
	t.Cleanup(func() { removeTree = real })
	removeTree = func(root *os.Root, name string) error {
		if _, err := root.Stat(name); os.IsNotExist(err) {
			return nil
		}
		if err := root.Remove(filepath.Join(name, ownershipMarker)); err != nil {
			t.Errorf("remove the marker inside %s: %v", name, err)
		}
		return errors.New("stopped partway")
	}
	if err := s.reclaimIncomplete(dbName); err == nil {
		t.Fatal("reclaimIncomplete succeeded although its removal stopped")
	}

	// The next attempt, after a restart, finishes the reclaim.
	removeTree = real
	if err := s.reclaimIncomplete(dbName); err != nil {
		t.Fatalf("reclaimIncomplete after the stopped removal: %v", err)
	}
	if hasExistingDB(databases, dbName) {
		t.Errorf("%s is still there: the next restore or bootstrap refuses it", dir)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != otherDatabaseLine {
		t.Errorf("databases.txt = %q, want only the other database's line", data)
	}
	entries, err := os.ReadDir(databases)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != databasesTxt {
			t.Errorf("%s was left in the database root", e.Name())
		}
	}
}

// Each state a crash inside reclaimIncomplete can leave is finished by the
// next attempt: a marked directory whose registration is already gone, and a
// directory already moved aside, whole or partly removed (#298).
func TestReclaimIncomplete_FinishesFromEachCrashState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, databases, dir string)
	}{
		{"unregistered, still marked", func(*testing.T, string, string) {}},
		{"moved aside whole", func(t *testing.T, databases, dir string) {
			if err := os.Rename(dir, filepath.Join(databases, reclaimDir(dbName))); err != nil {
				t.Fatal(err)
			}
		}},
		{"moved aside, marker removed", func(t *testing.T, databases, dir string) {
			tomb := filepath.Join(databases, reclaimDir(dbName))
			if err := os.Rename(dir, tomb); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(tomb, ownershipMarker)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databases := t.TempDir()
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := leaveInterrupted(t, store, databases, OpRestore, OpFailed)
			file := filepath.Join(databases, databasesTxt)
			if err := os.WriteFile(file, []byte(otherDatabaseLine), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.leave(t, databases, dir)
			s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})

			if err := s.reclaimIncomplete(dbName); err != nil {
				t.Fatalf("reclaimIncomplete: %v", err)
			}
			entries, err := os.ReadDir(databases)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != databasesTxt {
					t.Errorf("%s was left in the database root", e.Name())
				}
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != otherDatabaseLine {
				t.Errorf("databases.txt = %q, want only the other database's line", data)
			}
		})
	}
}

// A reclaim that refuses, or finds nothing to reclaim, changes no
// registration: databases.txt is only edited once every guard has passed.
func TestReclaimIncomplete_KeepsTheRegistrationWhenItRemovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   OperationState
		marker  func(id string) string
		restore bool
		refused bool
	}{
		{name: "completed operation", state: OpCompleted, marker: func(id string) string { return id + "\n" }},
		{name: "no marker", marker: nil},
		{name: "restored data", state: OpFailed, marker: func(id string) string { return id + "\n" }, restore: true, refused: true},
		{name: "running operation", state: OpRestoring, marker: func(id string) string { return id + "\n" }, refused: true},
		{name: "unknown operation", state: OpFailed, marker: func(string) string { return "op-" + strings.Repeat("0", 32) + "\n" }, refused: true},
		{name: "unreadable marker", state: OpFailed, marker: func(string) string { return "not an id\n" }, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databases := t.TempDir()
			store, err := NewOperationStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			state := tc.state
			if state == "" {
				state = OpFailed
			}
			dir := leaveInterrupted(t, store, databases, OpRestore, state)
			marker := filepath.Join(dir, ownershipMarker)
			id := strings.TrimSpace(readFile(t, marker))
			if tc.restore {
				if _, err := store.Update(id, func(op *Operation) { op.Restored = &OperationArtifact{Database: dbName} }); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker == nil {
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte(tc.marker(id)), 0o600); err != nil {
				t.Fatal(err)
			}
			registered := otherDatabaseLine + dbName + "\t\t" + dir + "\thost\n"
			file := filepath.Join(databases, databasesTxt)
			if err := os.WriteFile(file, []byte(registered), 0o600); err != nil {
				t.Fatal(err)
			}
			s := NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})

			err = s.reclaimIncomplete(dbName)
			if tc.refused != (err != nil) {
				t.Fatalf("reclaimIncomplete error = %v, want refused=%v", err, tc.refused)
			}
			if got := readFile(t, file); got != registered {
				t.Errorf("databases.txt = %q, want it unchanged", got)
			}
			if _, err := os.Stat(filepath.Join(dir, dbName+"_vinf")); err != nil {
				t.Errorf("the data was removed: %v", err)
			}
		})
	}
}
