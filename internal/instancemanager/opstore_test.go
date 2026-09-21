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
	"testing"
)

func newStore(t *testing.T) (*OperationStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewOperationStore(dir)
	if err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	return s, dir
}

func TestFindOrCreate_NewThenSameKeySameRequest(t *testing.T) {
	s, _ := newStore(t)

	op1, existed, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName)
	if err != nil || existed {
		t.Fatalf("first create: existed=%v err=%v", existed, err)
	}
	if op1.State != OpPending || !isValidOperationID(op1.ID) {
		t.Fatalf("op1 = %+v", op1)
	}

	op2, existed, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName)
	if err != nil || !existed {
		t.Fatalf("repeat: existed=%v err=%v", existed, err)
	}
	if op2.ID != op1.ID {
		t.Errorf("repeat returned a different op: %s != %s", op2.ID, op1.ID)
	}
}

func TestFindOrCreate_SameKeyDifferentRequestConflicts(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, _, err := s.FindOrCreate(OpBackup, "key-1", "hash-DIFFERENT", dbName)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestFindOrCreate_SingleActiveOpPerDatabase(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// A different key on the same database while the first is non-terminal.
	_, _, err := s.FindOrCreate(OpBackup, "key-2", "hash-b", dbName)
	if !errors.Is(err, ErrOperationInProgress) {
		t.Errorf("err = %v, want ErrOperationInProgress", err)
	}
}

func TestFindOrCreate_NewKeyAllowedAfterTerminal(t *testing.T) {
	s, _ := newStore(t)
	op, _, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(op.ID, func(o *Operation) { o.State = OpFailed }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, err := s.FindOrCreate(OpBackup, "key-2", "hash-b", dbName); err != nil {
		t.Errorf("new op after terminal should be allowed, got %v", err)
	}
}

func TestOperationStore_ReconcileOnStart_FailsInFlight(t *testing.T) {
	s, dir := newStore(t)
	op, _, err := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(op.ID, func(o *Operation) { o.State = OpRunningBackup }); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Reopen the store (simulates a manager restart).
	s2, err := NewOperationStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(op.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.State != OpFailed {
		t.Errorf("in-flight op after restart = %s, want Failed (never a false Completed)", got.State)
	}
	if got.FailureReason == "" {
		t.Error("expected a failure reason after restart reconciliation")
	}
}

func TestOperationStore_ReconcileOnStart_KeepsTerminal(t *testing.T) {
	s, dir := newStore(t)
	op, _, _ := s.FindOrCreate(OpBackup, "key-1", "hash-a", dbName)
	if _, err := s.Update(op.ID, func(o *Operation) { o.State = OpCompleted }); err != nil {
		t.Fatalf("update: %v", err)
	}
	s2, _ := NewOperationStore(dir)
	got, _ := s2.Get(op.ID)
	if got.State != OpCompleted {
		t.Errorf("terminal op after restart = %s, want Completed unchanged", got.State)
	}
}

func TestOperationStore_Get_RejectsUnsafeID(t *testing.T) {
	s, _ := newStore(t)
	for _, bad := range []string{"../etc/passwd", "op-xyz", "", "op-" + "g" + "0000000000000000000000000000000"} {
		if _, err := s.Get(bad); !errors.Is(err, ErrOperationNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrOperationNotFound", bad, err)
		}
	}
}

func TestNewOperationStore_CreatesDir(t *testing.T) {
	dir := t.TempDir() + "/nested/ops"
	if _, err := NewOperationStore(dir); err != nil {
		t.Fatalf("NewOperationStore: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("store dir not created: %v", err)
	}
}
