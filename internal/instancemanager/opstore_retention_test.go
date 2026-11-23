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
	"testing"
	"time"
)

// endedAt records operation id as ended in state at the time at: the record
// on disk then says the operation ended that long ago.
func endedAt(t *testing.T, s *OperationStore, id string, state OperationState, at time.Time) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.readLocked(id)
	if err != nil {
		t.Fatal(err)
	}
	op.State = state
	op.UpdatedAt = at
	if err := s.writeLocked(op); err != nil {
		t.Fatal(err)
	}
}

// noReferences is a referenced function that names no operation.
func noReferences() (map[string]bool, error) { return map[string]bool{}, nil }

// recordExists reports whether the record of operation id is on disk.
func recordExists(t *testing.T, dir, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, id+".json"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// A completed operation is never run again within the horizon, also after a
// restart: the same key, sent to a store reopened from disk, is answered
// with the recorded operation, and a request under another key that prunes
// the store keeps it (#369).
func TestOperationStore_SameKeyWithinTheHorizonIsNotRunAgainAfterARestart(t *testing.T) {
	s, dir := newStore(t)
	s.SetReferenced(noReferences)
	op, _, err := s.FindOrCreate(OpBackup, "backup-key", sameBody, dbName)
	if err != nil {
		t.Fatal(err)
	}
	endedAt(t, s, op.ID, OpCompleted, time.Now().UTC().Add(-operationRetention+time.Hour))

	reopened, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetReferenced(noReferences)
	// Another request runs the pruning first.
	if _, _, err := reopened.FindOrCreate(OpBackup, "other-key", sameBody, "otherdb"); err != nil {
		t.Fatal(err)
	}
	again, existed, err := reopened.FindOrCreate(OpBackup, "backup-key", sameBody, dbName)
	if err != nil || !existed || again.ID != op.ID || again.State != OpCompleted {
		t.Fatalf("same key after a restart = %+v (existed %v), %v; want the completed operation %s", again, existed, err, op.ID)
	}
	found, err := reopened.FindByKey("backup-key")
	if err != nil || len(found) != 1 || found[0].ID != op.ID {
		t.Fatalf("FindByKey after a restart = %+v, %v; want operation %s", found, err, op.ID)
	}
}

// Only records whose operation ended more than the horizon ago are pruned.
// One that has not ended, one whose end could not be written, one within
// the horizon, one without a time and one from the future are kept.
func TestOperationStore_PrunesOnlyRecordsThatEndedBeyondTheHorizon(t *testing.T) {
	s, dir := newStore(t)
	s.SetReferenced(noReferences)
	now := time.Now().UTC()
	create := func(key, database string) *Operation {
		t.Helper()
		op, _, err := s.FindOrCreate(OpBackup, key, sameBody, database)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	// Every operation is created before any is aged: a creation prunes.
	expiredCompleted := create("expired-completed", "db1")
	expiredFailed := create("expired-failed", "db2")
	young := create("young", "db3")
	noTime := create("no-time", "db4")
	future := create("future", "db5")
	running := create("running", "db6")
	unwritten := create("unwritten", "db7")
	endedAt(t, s, expiredCompleted.ID, OpCompleted, now.Add(-operationRetention-time.Hour))
	endedAt(t, s, expiredFailed.ID, OpFailed, now.Add(-operationRetention-time.Hour))
	endedAt(t, s, young.ID, OpCompleted, now.Add(-operationRetention+time.Hour))
	endedAt(t, s, noTime.ID, OpCompleted, time.Time{})
	endedAt(t, s, future.ID, OpCompleted, now.Add(time.Hour))
	endedAt(t, s, running.ID, OpRunningBackup, now.Add(-operationRetention-time.Hour))
	endedAt(t, s, unwritten.ID, OpFailed, now.Add(-operationRetention-time.Hour))
	// Its record on disk ended long ago, but a later end is kept in memory.
	s.mu.Lock()
	s.unsettled = map[string]func(*Operation){unwritten.ID: func(o *Operation) { o.State = OpCompleted }}
	ops, err := s.listLocked()
	if err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.pruneLocked(ops, now)
	s.mu.Unlock()

	for _, c := range []struct {
		name string
		op   *Operation
		want bool
	}{
		{"completed beyond the horizon", expiredCompleted, false},
		{"failed beyond the horizon", expiredFailed, false},
		{"within the horizon", young, true},
		{"without a time", noTime, true},
		{"from the future", future, true},
		{"running", running, true},
		{"end not written", unwritten, true},
	} {
		if got := recordExists(t, dir, c.op.ID); got != c.want {
			t.Errorf("%s: record kept = %v, want %v", c.name, got, c.want)
		}
	}
}

// A record that an ownership marker names is never pruned: the database
// directory, a completed restore started again after a restart and a member
// rebuild waiting for its release are judged by it. Nothing is pruned while
// the markers cannot be read.
func TestOperationStore_KeepsRecordsThatAMarkerNames(t *testing.T) {
	base := t.TempDir()
	databases := filepath.Join(base, "databases")
	store, err := NewOperationStore(filepath.Join(base, "operations"))
	if err != nil {
		t.Fatal(err)
	}
	NewServer(fakeCLI{}, "tok").WithOperationStore(store).WithRestoreRoots(RestoreRoots{Target: databases})
	old := time.Now().UTC().Add(-operationRetention - time.Hour)
	// Every operation is created before any is aged: a creation prunes.
	marked, _, err := store.FindOrCreate(OpRestore, "marked", sameBody, dbName)
	if err != nil {
		t.Fatal(err)
	}
	unmarked, _, err := store.FindOrCreate(OpRestore, "unmarked", sameBody, "otherdb")
	if err != nil {
		t.Fatal(err)
	}
	endedAt(t, store, marked.ID, OpCompleted, old)
	endedAt(t, store, unmarked.ID, OpFailed, old)
	if err := os.MkdirAll(filepath.Join(databases, dbName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(databases, markerPath(dbName)), []byte(marked.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// databases.txt is a file beside the database directories, not one.
	if err := os.WriteFile(filepath.Join(databases, databasesTxt), []byte("#db\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A marker that cannot be read: it may name either record.
	if os.Geteuid() != 0 {
		blocked := filepath.Join(databases, "blocked")
		if err := os.MkdirAll(blocked, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blocked, ownershipMarker), []byte(unmarked.ID+"\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FindOrCreate(OpBackup, "trigger-1", sameBody, "db1"); err != nil {
			t.Fatal(err)
		}
		if !recordExists(t, store.dir, unmarked.ID) || !recordExists(t, store.dir, marked.ID) {
			t.Fatal("a record was pruned while a marker could not be read")
		}
		if err := os.RemoveAll(blocked); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := store.FindOrCreate(OpBackup, "trigger-2", sameBody, "db2"); err != nil {
		t.Fatal(err)
	}
	if !recordExists(t, store.dir, marked.ID) {
		t.Error("the record the marker names was pruned")
	}
	if recordExists(t, store.dir, unmarked.ID) {
		t.Error("the expired record no marker names was kept")
	}
	// The marked record still answers as before.
	op, err := store.Get(marked.ID)
	if err != nil || op.State != OpCompleted {
		t.Errorf("Get(marked) = %+v, %v", op, err)
	}
}

// A store without SetReferenced prunes nothing: it cannot tell which
// records a marker names.
func TestOperationStore_PrunesNothingWithoutReferences(t *testing.T) {
	s, dir := newStore(t)
	op, _, err := s.FindOrCreate(OpBackup, "old", sameBody, dbName)
	if err != nil {
		t.Fatal(err)
	}
	endedAt(t, s, op.ID, OpCompleted, time.Now().UTC().Add(-operationRetention-time.Hour))
	if _, _, err := s.FindOrCreate(OpBackup, "new", sameBody, "otherdb"); err != nil {
		t.Fatal(err)
	}
	if !recordExists(t, dir, op.ID) {
		t.Error("a store without references pruned a record")
	}
}

// A pruning stopped partway, as by a crash after the first removal, leaves
// every record whole or absent. The store reopened from disk reads every
// record that is left, still answers the keys within the horizon with their
// operations, and prunes the rest at its next request. A directory sync that
// fails after a removal fails no request.
func TestOperationStore_PruningStoppedPartwayLeavesAUsableStore(t *testing.T) {
	s, dir := newStore(t)
	s.SetReferenced(noReferences)
	old := time.Now().UTC().Add(-operationRetention - time.Hour)
	// Every operation is created before any is aged: a creation prunes.
	first, _, err := s.FindOrCreate(OpBackup, "first", sameBody, "db1")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.FindOrCreate(OpBackup, "second", sameBody, "db2")
	if err != nil {
		t.Fatal(err)
	}
	young, _, err := s.FindOrCreate(OpBackup, "young", sameBody, "db3")
	if err != nil {
		t.Fatal(err)
	}
	endedAt(t, s, first.ID, OpCompleted, old)
	endedAt(t, s, second.ID, OpCompleted, old)
	endedAt(t, s, young.ID, OpCompleted, time.Now().UTC())

	// The second removal does not happen: the pruning stops there.
	removals := 0
	removeOperationRecord = func(name string) error {
		removals++
		if removals > 1 {
			return errors.New("the manager stopped")
		}
		return os.Remove(name)
	}
	t.Cleanup(func() { removeOperationRecord = os.Remove })
	if _, _, err := s.FindOrCreate(OpBackup, "trigger-1", sameBody, "db4"); err != nil {
		t.Fatalf("a pruning that stopped failed the request: %v", err)
	}
	removeOperationRecord = os.Remove
	if left := []bool{recordExists(t, dir, first.ID), recordExists(t, dir, second.ID)}; left[0] == left[1] {
		t.Fatalf("records left after one removal = %v; want exactly one of the two expired records", left)
	}

	reopened, err := NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetReferenced(noReferences)
	if err := reopened.unreadableErrLocked(); err != nil {
		t.Fatalf("a record left by the stopped pruning cannot be read: %v", err)
	}
	again, existed, err := reopened.FindOrCreate(OpBackup, "young", sameBody, "db3")
	if err != nil || !existed || again.ID != young.ID {
		t.Fatalf("key within the horizon after the stopped pruning = %+v (existed %v), %v; want operation %s",
			again, existed, err, young.ID)
	}

	// The directory sync after a removal fails: the request is answered.
	syncOperationDir = func(string) error { return errors.New("sync failed") }
	t.Cleanup(func() { syncOperationDir = syncDirPath })
	reopened.mu.Lock()
	ops, err := reopened.listLocked()
	if err != nil {
		reopened.mu.Unlock()
		t.Fatal(err)
	}
	reopened.pruneLocked(ops, time.Now().UTC())
	reopened.mu.Unlock()
	syncOperationDir = syncDirPath
	if recordExists(t, dir, first.ID) || recordExists(t, dir, second.ID) {
		t.Error("the next pruning kept an expired record")
	}
	if !recordExists(t, dir, young.ID) {
		t.Error("the next pruning removed the record within the horizon")
	}
}

// A record that cannot be read is never pruned, and while it stays
// unreadable no operation is admitted, as before pruning existed (#427).
func TestOperationStore_PruningKeepsTheUnreadableRecordGuard(t *testing.T) {
	s, dir := newStore(t)
	s.SetReferenced(noReferences)
	op, _, err := s.FindOrCreate(OpBackup, "old", sameBody, dbName)
	if err != nil {
		t.Fatal(err)
	}
	endedAt(t, s, op.ID, OpCompleted, time.Now().UTC().Add(-operationRetention-time.Hour))
	repair := overwrite("{")(t, filepath.Join(dir, op.ID+".json"))
	if _, _, err := s.FindOrCreate(OpBackup, "new", sameBody, "otherdb"); !errors.Is(err, ErrOperationRecordUnreadable) {
		t.Fatalf("FindOrCreate beside an unreadable record = %v; want ErrOperationRecordUnreadable", err)
	}
	if !recordExists(t, dir, op.ID) {
		t.Fatal("the unreadable record was removed")
	}
	repair()
	again, existed, err := s.FindOrCreate(OpBackup, "old", sameBody, dbName)
	if err != nil || !existed || again.ID != op.ID {
		t.Fatalf("after the repair = %+v (existed %v), %v; want operation %s", again, existed, err, op.ID)
	}
}
