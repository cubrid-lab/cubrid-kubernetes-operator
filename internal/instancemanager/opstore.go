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
	// referenced returns the IDs of the operations that something outside
	// the store still names (SetReferenced). Records are pruned only while
	// it is set (pruneLocked).
	referenced func() (map[string]bool, error)
	// clock is the time pruning judges by; tests replace it.
	clock func() time.Time
	// startedAt is when this store was opened. Every record counts as read
	// then: a read by a process before a restart is not known.
	startedAt time.Time
	// lastRead holds, by operation ID, when a caller last read the record
	// (markRead). The Operator reads a finished record by its ID for as long
	// as it waits for the step after it, which may be held for any time.
	lastRead map[string]time.Time
}

// now is the time by the store's clock.
func (s *OperationStore) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// markRead records that a caller has just read the record of operation id,
// so that it is kept for at least operationRetention from now.
func (s *OperationStore) markRead(id string) {
	if s.lastRead == nil {
		s.lastRead = map[string]time.Time{}
	}
	s.lastRead[id] = s.now()
}

// SetReferenced turns on the pruning of expired records (pruneLocked).
// referenced returns the operation IDs that something outside the store
// still names, such as the ownership marker of a database directory; their
// records are never pruned. An error from it prunes nothing. It is called
// with the store's lock held and must not call the store.
func (s *OperationStore) SetReferenced(referenced func() (map[string]bool, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.referenced = referenced
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
	s.startedAt = s.now()
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
			s.markRead(op.ID)
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

	// Only now, after the key was looked up: an expired record under the
	// request's own key is answered above, never pruned and then re-run.
	s.pruneLocked(ops, s.now())

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
		// The record may be in place although its directory could not be
		// synced: removed, it holds neither the key nor the database for an
		// operation that nothing runs. One that a crash brings back is
		// failed at the next start (reconcileOnStart).
		_ = os.Remove(filepath.Join(s.dir, op.ID+".json"))
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
			s.markRead(op.ID)
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
	op, err := s.readLocked(id)
	if err == nil {
		s.markRead(id)
	}
	return op, err
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

// UnwrittenEnds tries again to write every end kept in memory (Settle) and
// returns the operations whose end still is not on record, sorted.
func (s *OperationStore) UnwrittenEnds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.unsettled {
		_ = s.settleLocked(id, true)
	}
	ids := make([]string, 0, len(s.unsettled))
	for id := range s.unsettled {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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

// operationRetention is the idempotency horizon of the store: how long a
// record stays after its operation ended (Completed or Failed) and after it
// was last read, before it may be pruned. The horizon is counted from the
// latest of the record's UpdatedAt, the last read of the record by a caller
// (Get, Peek, FindByKey, or FindOrCreate answering its key) and the time the
// store was opened. Within it, a repeat of the same idempotency key is
// answered with the recorded operation and never runs it again, and a read
// by operation ID finds the record, also across a restart.
//
// The Operator uses a record in two ways. It sends a key until the
// resource records the operation's ID; it stops waiting for an operation
// it started or cannot find after backupTargetWait,
// backupOperationMissingWait and recoveryOperationMissingWait (30 minutes
// each, internal/controller). It also reads a finished record by its ID for
// as long as it waits for the step after it: the seed backup of an HA
// bootstrap and the backup of a member rebuild are read on every pass while
// their restore may be held for any time (an engine series hold, a
// capability hold, the wait for the only active master). Backups leave no
// ownership marker, so such a record is kept by those reads: it expires only
// once nothing has read it for the whole horizon. Thirty days also leave
// room for an Operator that was down, or a member that could not be
// reached, for weeks; a restart of the manager starts the horizon again for
// every record. A record is removed whole rather than replaced by a smaller
// tombstone: a tombstone that answers a retried key with the original
// outcome keeps most of the record, and would itself need a horizon.
const operationRetention = 30 * 24 * time.Hour

// removeOperationRecord removes one record file; tests replace it to stop a
// pruning partway, as a crash would.
var removeOperationRecord = os.Remove

// pruneLocked removes the records of ops whose operation ended, and that no
// caller read, more than operationRetention before now (see there), and
// keeps every other record:
//   - one that has not ended, or whose end is kept in memory only (Settle);
//   - one without a usable UpdatedAt;
//   - one that referenced names, such as the operation an ownership marker
//     names: a database directory, a completed restore that is started again
//     after a restart, or a member rebuild that waits for its release, is
//     judged by that record, and without it is refused as unknown.
//
// Nothing is pruned when referenced is not set or fails, or when now is
// before the UpdatedAt of a record: the clock then went back, and ages
// measured by it are not known. A clock that jumps forward is not detected;
// it can only shorten the horizon by the size of the jump. Records the
// latest listing could not read are not in ops and are never pruned; one
// the start could not read has been failed and written again (readLocked)
// before it is in ops, which makes it young.
//
// Each record is removed with one unlink, then the directory is synced. A
// crash at any point leaves every record either whole or absent, never
// partly written; a record whose removal was not yet durable may come back,
// and is pruned again later. No step writes another record, so a crash never
// makes a key that is still within the horizon look new. A failure is logged
// and stops the pruning; it never fails the request.
func (s *OperationStore) pruneLocked(ops []*Operation, now time.Time) {
	if s.referenced == nil {
		return
	}
	var expired []*Operation
	for _, op := range ops {
		if now.Before(op.UpdatedAt) {
			s.logPruneSkipped(fmt.Errorf("the clock (%s) is before the last change of operation %s (%s)",
				now.Format(time.RFC3339), op.ID, op.UpdatedAt.Format(time.RFC3339)),
				"operationID", op.ID, "updatedAt", op.UpdatedAt.Format(time.RFC3339))
			return
		}
	}
	for _, op := range ops {
		_, unwritten := s.unsettled[op.ID]
		if !op.State.IsTerminal() || unwritten || op.UpdatedAt.IsZero() {
			continue
		}
		last := op.UpdatedAt
		for _, t := range []time.Time{s.startedAt, s.lastRead[op.ID]} {
			if t.After(last) {
				last = t
			}
		}
		if now.Sub(last) <= operationRetention {
			continue
		}
		expired = append(expired, op)
	}
	if len(expired) == 0 {
		return
	}
	referenced, err := s.referenced()
	if err != nil {
		s.logPruneSkipped(err)
		return
	}
	removed := false
	for _, op := range expired {
		if referenced[op.ID] {
			continue
		}
		if err := removeOperationRecord(filepath.Join(s.dir, op.ID+".json")); err != nil && !os.IsNotExist(err) {
			s.logPruneSkipped(fmt.Errorf("remove the record of operation %s: %w", op.ID, err))
			break
		}
		removed = true
		delete(s.lastRead, op.ID)
		if s.logger != nil {
			s.logger.Info("Removed the record of an operation that ended before the idempotency horizon",
				"event", eventRecordPruned, "operationID", op.ID, "kind", string(op.Kind),
				"state", string(op.State), "updatedAt", op.UpdatedAt.Format(time.RFC3339))
		}
	}
	if removed {
		if err := syncOperationDir(s.dir); err != nil {
			s.logPruneSkipped(fmt.Errorf("sync the operation directory: %w", err))
		}
	}
}

// logPruneSkipped records why expired records were kept for now, with
// attrs naming the record that stopped the pruning, when one did.
func (s *OperationStore) logPruneSkipped(err error, attrs ...any) {
	if s.logger == nil {
		return
	}
	s.logger.With(attrs...).Warn("Kept expired operation records; the next request tries again",
		"event", eventPruneSkipped, "err", err.Error())
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

// Peek returns the operation with the given ID as the next Get will see it,
// and writes nothing: an end that could not be written (Settle) is applied
// to the returned copy only, and a record the start could not read that has
// not ended is returned Failed, as its first read records it. The record on
// disk, the kept end and the remembered interruption are left as they are.
func (s *OperationStore) Peek(id string) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.decodeLocked(id)
	if err != nil {
		return nil, err
	}
	s.markRead(id)
	if mutate, ok := s.unsettled[id]; ok {
		mutate(op)
		return op, nil
	}
	if s.interrupted[id] && !op.State.IsTerminal() {
		op.State, op.FailureReason = OpFailed, reasonRestarted
	}
	return op, nil
}

// readLocked returns the record of operation id, and fails it first when the
// start could not read it and it has not ended (failInterruptedLocked).
func (s *OperationStore) readLocked(id string) (*Operation, error) {
	op, err := s.decodeLocked(id)
	if err != nil {
		return nil, err
	}
	if s.interrupted[id] {
		if err := s.failInterruptedLocked(op); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// decodeLocked reads and decodes the record of operation id as it is on
// disk.
func (s *OperationStore) decodeLocked(id string) (*Operation, error) {
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
	// matches on, is no more usable than one that does not decode. Its
	// database is the one admission keeps to one operation at a time, so a
	// record whose database is not a name a request may carry could hold
	// any database (#865).
	if op.ID != id || op.Kind == "" || op.IdempotencyKey == "" || !databaseNamePattern.MatchString(op.Database) {
		return nil, fmt.Errorf("decode operation %s: the record has ID %q, kind %q, an idempotency key of %d bytes and database %q",
			id, op.ID, op.Kind, len(op.IdempotencyKey), op.Database)
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
// target so a crash mid-write never leaves a partial record, and sync the
// directory so that the renamed record survives a power loss.
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
	if err := syncOperationDir(s.dir); err != nil {
		return fmt.Errorf("sync the operation directory: %w", err)
	}
	return nil
}

// syncOperationDir makes a renamed operation record durable; tests replace
// it to fail after the rename.
var syncOperationDir = syncDirPath

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
