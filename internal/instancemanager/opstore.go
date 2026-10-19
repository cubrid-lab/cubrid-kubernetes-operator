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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// ErrOperationRecordUnreadable is returned when a new operation is asked for
// while the store holds a record it cannot read or decode. A record is named
// by its random operation ID; the idempotency key, kind and database are only
// inside it. It may therefore be the operation the request names, or one that
// holds the database, and no operation is started beside it (#427).
var ErrOperationRecordUnreadable = errors.New("an operation record cannot be read")

// OperationStore persists Operation records to the PVC so a manager or operator
// restart can re-poll by ID / idempotency key and reconstruct state from durable
// facts (ADR-0003). All mutations are serialized and written atomically
// (temp file + rename).
type OperationStore struct {
	dir string
	mu  sync.Mutex
	// logger records state changes; nil records nothing.
	logger *slog.Logger
	// unsettled holds, by operation ID, the end of an operation that could
	// not be written (Settle).
	unsettled map[string]func(*Operation)
	// unreadable holds, by operation ID, the records the latest listing
	// could not read.
	unreadable map[string]*unreadableRecord
	// interrupted holds the IDs of the records reconcileOnStart could not
	// read. The first successful read of one that has not ended fails it,
	// as reconcileOnStart would have.
	interrupted map[string]bool
}

// unreadableRecord is a record that could not be read, why, and whether
// that has been logged.
type unreadableRecord struct {
	err    error
	logged bool
}

// SetLogger makes the store log every operation it creates and every change
// of an operation's state. A record found unreadable before, as at start, is
// logged now.
func (s *OperationStore) SetLogger(logger *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
	s.logUnreadableLocked()
}

// logState records one change of an operation's state. A failure is a
// warning and carries its reason.
func (s *OperationStore) logState(op *Operation, previous OperationState) {
	if s.logger == nil {
		return
	}
	attrs := []any{"event", eventOperationState, "operationID", op.ID, "kind", string(op.Kind),
		"state", string(op.State), "previousState", string(previous)}
	if op.State == OpFailed {
		s.logger.Warn("Operation failed", append(attrs, "reason", op.FailureReason)...)
		return
	}
	s.logger.Info("Operation changed state", attrs...)
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
//   - a record that cannot be read -> ErrOperationRecordUnreadable
//   - none                       -> (newly created Pending op, existed=false, nil)
//
// A record that cannot be read may hold the key or the database, so while
// one exists no operation is created, of any key, kind or database. A
// readable record under the key is still answered: that starts nothing.
func (s *OperationStore) FindOrCreate(kind OperationKind, idempotencyKey, requestHash, database string) (*Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An end that could not be written is tried again first. While it stays
	// unwritten its operation counts as running, so nothing is started
	// beside it.
	for id := range s.unsettled {
		_ = s.settleLocked(id, true)
	}
	ops, err := s.listLocked()
	if err != nil {
		return nil, false, err
	}

	for _, op := range ops {
		if op.IdempotencyKey == idempotencyKey && op.Kind == kind {
			if op.RequestHash != requestHash {
				return nil, false, ErrIdempotencyConflict
			}
			// The record still says that the operation runs.
			if _, unwritten := s.unsettled[op.ID]; unwritten {
				return nil, false, errUnwrittenEnd(op.ID, nil)
			}
			return op, true, nil
		}
	}

	// No readable record under the key: one that cannot be read may be it.
	if err := s.unreadableErrLocked(); err != nil {
		return nil, false, err
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
		s.logWriteFailure(op, err, slog.LevelError)
		return nil, false, err
	}
	s.logState(op, "")
	return op, false, nil
}

// FindByKey returns the operations recorded under idempotencyKey, of any
// kind. It only reads the store. Unlike listLocked it fails on a record it
// cannot read: that record may be the one asked for, so "none" would not be
// known. An end that could not be written (Settle) is tried again first; while
// one of the operations asked for stays unwritten FindByKey fails, as Get
// does, rather than return a record that still says the operation runs.
func (s *OperationStore) FindByKey(idempotencyKey string) ([]*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.unsettled {
		_ = s.settleLocked(id, true)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read operation store: %w", err)
	}
	found := []*Operation{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		op, err := s.readLocked(name[:len(name)-len(".json")])
		if errors.Is(err, ErrOperationNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if op.IdempotencyKey == idempotencyKey {
			if _, unwritten := s.unsettled[op.ID]; unwritten {
				return nil, errUnwrittenEnd(op.ID, nil)
			}
			found = append(found, op)
		}
	}
	return found, nil
}

// Get returns the operation with the given ID. An operation whose end could
// not be written (Settle) is written first; while that fails Get returns the
// error and not the record, which still says that the operation runs.
func (s *OperationStore) Get(id string) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isValidOperationID(id) {
		return nil, ErrOperationNotFound
	}
	if err := s.settleLocked(id, true); err != nil {
		return nil, errUnwrittenEnd(id, err)
	}
	return s.readLocked(id)
}

// Update applies mutate to the stored operation and persists it atomically.
// A record that could not be written is logged as an error.
func (s *OperationStore) Update(id string, mutate func(*Operation)) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(id, mutate, slog.LevelError)
}

// updateLocked is Update under s.mu. A write that fails is logged at level.
func (s *OperationStore) updateLocked(id string, mutate func(*Operation), level slog.Level) (*Operation, error) {
	op, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	previous := op.State
	mutate(op)
	op.UpdatedAt = time.Now().UTC()
	if err := s.writeLocked(op); err != nil {
		s.logWriteFailure(op, err, level)
		return nil, err
	}
	if op.State != previous {
		s.logState(op, previous)
	}
	return op, nil
}

// Settle records how an operation ended. When the record cannot be written,
// the end is kept in memory and written by the next Get of the operation, the
// next FindByKey or the next FindOrCreate; it is never reported as recorded
// before it is, and until then those three answer an error for it rather
// than the record, which still says that the operation runs. The first
// failed write is logged as an error, the retries at debug level. A
// manager that restarts meanwhile finds the operation unfinished and fails
// it (reconcileOnStart): an end that was only in memory does not count.
func (s *OperationStore) Settle(id string, mutate func(*Operation)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsettled == nil {
		s.unsettled = map[string]func(*Operation){}
	}
	s.unsettled[id] = mutate
	return s.settleLocked(id, false)
}

// settleLocked writes the kept end of operation id, if there is one. A retry
// that fails is logged at debug level: the first failure was an error.
func (s *OperationStore) settleLocked(id string, retry bool) error {
	mutate, ok := s.unsettled[id]
	if !ok {
		return nil
	}
	level := slog.LevelError
	if retry {
		level = slog.LevelDebug
	}
	_, err := s.updateLocked(id, mutate, level)
	if err == nil || errors.Is(err, ErrOperationNotFound) {
		delete(s.unsettled, id)
	}
	return err
}

// errUnwrittenEnd is the error for operation id while its end is kept in
// memory only (Settle); cause is the failed write, when there is one.
func errUnwrittenEnd(id string, cause error) error {
	if cause == nil {
		return fmt.Errorf("operation %s has ended, and how it ended could not be recorded yet", id)
	}
	return fmt.Errorf("operation %s has ended, and how it ended could not be recorded yet: %w", id, cause)
}

// logWriteFailure records at level that an operation's record could not be
// written, with the state that is therefore not on disk.
func (s *OperationStore) logWriteFailure(op *Operation, err error, level slog.Level) {
	if s.logger == nil {
		return
	}
	s.logger.Log(context.Background(), level, "Could not write the operation record", "event", eventStoreWriteFailed,
		"operationID", op.ID, "kind", string(op.Kind), "state", string(op.State), "err", err.Error())
}

// reasonRestarted is the failure reason of an operation that was running
// when the manager stopped.
const reasonRestarted = "manager restarted while operation was in progress"

// reconcileOnStart fails every readable record that has not ended. A record
// it cannot read is left as it is: it may be an operation that was running,
// and it cannot be failed now, so FindOrCreate admits nothing while it stays
// unreadable, and SetLogger logs it. It is remembered, and failed at its
// first successful read if it has not ended (readLocked).
func (s *OperationStore) reconcileOnStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.listLocked()
	if err != nil {
		return err
	}
	for id := range s.unreadable {
		if s.interrupted == nil {
			s.interrupted = map[string]bool{}
		}
		s.interrupted[id] = true
	}
	for _, op := range ops {
		if op.State.IsTerminal() {
			continue
		}
		op.State = OpFailed
		op.FailureReason = reasonRestarted
		op.UpdatedAt = time.Now().UTC()
		if err := s.writeLocked(op); err != nil {
			return err
		}
	}
	return nil
}

// listLocked returns the records it can read. Those it cannot read are kept
// in s.unreadable, replacing the previous listing's, and each is logged once
// while it stays unreadable.
func (s *OperationStore) listLocked() ([]*Operation, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read operation store: %w", err)
	}
	ops := make([]*Operation, 0, len(entries))
	unreadable := map[string]*unreadableRecord{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		op, err := s.readLocked(id)
		if errors.Is(err, ErrOperationNotFound) {
			continue
		}
		if err != nil {
			rec := &unreadableRecord{err: err}
			if known, ok := s.unreadable[id]; ok {
				rec.logged = known.logged
			}
			unreadable[id] = rec
			continue
		}
		ops = append(ops, op)
	}
	s.unreadable = unreadable
	s.logUnreadableLocked()
	return ops, nil
}

// logUnreadableLocked logs each unreadable record that has not been logged.
func (s *OperationStore) logUnreadableLocked() {
	if s.logger == nil {
		return
	}
	for _, id := range slices.Sorted(maps.Keys(s.unreadable)) {
		rec := s.unreadable[id]
		if rec.logged {
			continue
		}
		s.logger.Error("Could not read the operation record", "event", eventRecordUnreadable,
			"operationID", id, "err", rec.err.Error())
		rec.logged = true
	}
}

// unreadableErrLocked is ErrOperationRecordUnreadable for the records the
// latest listing could not read, or nil when there are none.
func (s *OperationStore) unreadableErrLocked() error {
	if len(s.unreadable) == 0 {
		return nil
	}
	ids := slices.Sorted(maps.Keys(s.unreadable))
	more := ""
	if len(ids) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(ids)-1)
	}
	return fmt.Errorf("%w%s: %w; no operation is started while it may be the one asked for or one that holds the database",
		ErrOperationRecordUnreadable, more, s.unreadable[ids[0]].err)
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
		// The error names the operation; the path is the store's.
		if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
			err = pathErr.Err
		}
		return nil, fmt.Errorf("read operation %s: %w", id, err)
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return nil, fmt.Errorf("decode operation %s: %w", id, err)
	}
	// A record that is not this operation's, or lacks what admission
	// matches on, is no more usable than one that does not decode.
	if op.ID != id || op.Kind == "" || op.IdempotencyKey == "" {
		return nil, fmt.Errorf("decode operation %s: the record has ID %q, kind %q and an idempotency key of %d bytes",
			id, op.ID, op.Kind, len(op.IdempotencyKey))
	}
	if s.interrupted[id] {
		if err := s.failInterruptedLocked(&op); err != nil {
			return nil, err
		}
	}
	return &op, nil
}

// failInterruptedLocked fails op, a record reconcileOnStart could not read,
// at its first successful read if it has not ended, as reconcileOnStart
// would have. A failure that cannot be written leaves it remembered and is
// returned, so the record is not answered as running.
func (s *OperationStore) failInterruptedLocked(op *Operation) error {
	if !op.State.IsTerminal() {
		previous := op.State
		op.State = OpFailed
		op.FailureReason = reasonRestarted
		op.UpdatedAt = time.Now().UTC()
		if err := s.writeLocked(op); err != nil {
			s.logWriteFailure(op, err, slog.LevelError)
			return fmt.Errorf("record that operation %s was interrupted by a restart: %w", op.ID, err)
		}
		s.logState(op, previous)
	}
	delete(s.interrupted, op.ID)
	return nil
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
