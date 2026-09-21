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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrIdempotencyConflict is returned when an idempotency key is reused with a
// different request body (a same-key/different-request repeat must not start a
// second run; ADR-0003).
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// ErrOperationInProgress is returned when a mutating operation is requested for
// a database that already has a non-terminal operation (single active mutating
// operation per database; ADR-0003).
var ErrOperationInProgress = errors.New("a mutating operation is already in progress for this database")

// ErrOperationNotFound is returned when an operation ID does not exist.
var ErrOperationNotFound = errors.New("operation not found")

// OperationStore persists Operation records to the PVC so a manager or operator
// restart can re-poll by ID / idempotency key and reconstruct state from durable
// facts (ADR-0003). All mutations are serialized and written atomically
// (temp file + rename).
type OperationStore struct {
	dir string
	mu  sync.Mutex
}

// NewOperationStore opens (creating if needed) a durable store under dir and
// reconciles any non-terminal records left by a previous process: an operation
// that was mid-flight when the manager died is marked Failed, never silently
// Completed and never blindly restarted (ADR-0003).
func NewOperationStore(dir string) (*OperationStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create operation store dir: %w", err)
	}
	s := &OperationStore{dir: dir}
	if err := s.reconcileOnStart(); err != nil {
		return nil, err
	}
	return s, nil
}

// HashRequest returns a stable hex digest of a request body for idempotency.
func HashRequest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// FindOrCreate returns an existing operation for idempotencyKey when the request
// matches, or creates a new Pending operation. The three outcomes an ADR-0003
// idempotent endpoint must distinguish:
//   - existing op, same request  -> (op, existed=true, nil)
//   - existing op, diff request  -> ErrIdempotencyConflict
//   - a different in-flight op on the same database -> ErrOperationInProgress
//   - none                       -> (newly created Pending op, existed=false, nil)
func (s *OperationStore) FindOrCreate(kind OperationKind, idempotencyKey, requestHash, database string) (*Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ops, err := s.listLocked()
	if err != nil {
		return nil, false, err
	}

	for _, op := range ops {
		if op.IdempotencyKey == idempotencyKey && op.Kind == kind {
			if op.RequestHash != requestHash {
				return nil, false, ErrIdempotencyConflict
			}
			return op, true, nil
		}
	}

	// No matching key: refuse if another non-terminal op holds this database.
	for _, op := range ops {
		if op.Database == database && !op.State.IsTerminal() {
			return nil, false, ErrOperationInProgress
		}
	}

	id, err := newOperationID()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	op := &Operation{
		ID:             id,
		Kind:           kind,
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash,
		Database:       database,
		State:          OpPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.writeLocked(op); err != nil {
		return nil, false, err
	}
	return op, false, nil
}

// Get returns the operation with the given ID.
func (s *OperationStore) Get(id string) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isValidOperationID(id) {
		return nil, ErrOperationNotFound
	}
	return s.readLocked(id)
}

// Update applies mutate to the stored operation and persists it atomically.
func (s *OperationStore) Update(id string, mutate func(*Operation)) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	mutate(op)
	op.UpdatedAt = time.Now().UTC()
	if err := s.writeLocked(op); err != nil {
		return nil, err
	}
	return op, nil
}

func (s *OperationStore) reconcileOnStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.listLocked()
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.State.IsTerminal() {
			continue
		}
		op.State = OpFailed
		op.FailureReason = "manager restarted while operation was in progress"
		op.UpdatedAt = time.Now().UTC()
		if err := s.writeLocked(op); err != nil {
			return err
		}
	}
	return nil
}

func (s *OperationStore) listLocked() ([]*Operation, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read operation store: %w", err)
	}
	ops := make([]*Operation, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		op, err := s.readLocked(name[:len(name)-len(".json")])
		if err != nil {
			continue
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (s *OperationStore) readLocked(id string) (*Operation, error) {
	if !isValidOperationID(id) {
		return nil, ErrOperationNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrOperationNotFound
		}
		return nil, fmt.Errorf("read operation %s: %w", id, err)
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return nil, fmt.Errorf("decode operation %s: %w", id, err)
	}
	return &op, nil
}

// writeLocked persists op atomically: write a temp file then rename over the
// target so a crash mid-write never leaves a partial record.
func (s *OperationStore) writeLocked(op *Operation) error {
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("encode operation: %w", err)
	}
	final := filepath.Join(s.dir, op.ID+".json")
	tmp, err := os.CreateTemp(s.dir, op.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp operation file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp operation file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync temp operation file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp operation file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename operation file: %w", err)
	}
	return nil
}

// newOperationID returns a server-generated hex ID. Because the ID is never
// caller-supplied it is always a safe filesystem name (no path traversal).
func newOperationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate operation id: %w", err)
	}
	return "op-" + hex.EncodeToString(b), nil
}

// isValidOperationID rejects anything that is not a server-generated ID, so a
// caller-supplied {id} path segment can never escape the store directory.
func isValidOperationID(id string) bool {
	if len(id) != len("op-")+32 || id[:3] != "op-" {
		return false
	}
	for _, r := range id[3:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
