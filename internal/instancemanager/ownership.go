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
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ownershipMarker is a file in a database directory that names the operation
// which is creating that database. It is written before `createdb` or
// `restoredb` and removed when `createdb` has succeeded, or when a restore
// has been recorded as Completed, so its presence means "this directory is
// the unfinished work of that operation".
//
// It is what lets a later attempt remove a half-made database: only a
// directory that carries the marker of an operation this manager recorded as
// Failed, before it recorded the data as restored, is ever removed. A
// directory without the marker is someone's data and is never touched
// (ADR-0006, ADR-0008). The image's entrypoint writes the same file around
// the createdb of a standalone member, with a value that is not an operation
// ID; this manager therefore never takes that directory for its own.
const ownershipMarker = ".im-operation"

func markerPath(database string) string { return filepath.Join(database, ownershipMarker) }

// markOwned records opID as the operation creating <targetDir>/<database>.
// An empty opID (a caller without a durable operation) writes nothing. The
// marker is replaced whole and made durable together with the database
// directory that holds it: a crash leaves the earlier owner or the new one,
// never an unreadable marker or a directory without one.
func markOwned(root *os.Root, database, opID string) error {
	if opID == "" {
		return nil
	}
	if err := replaceFile(root, markerPath(database), []byte(opID+"\n")); err != nil {
		return fmt.Errorf("mark %s as owned by %s: %w", database, opID, err)
	}
	if err := syncDir(root, filepath.Dir(database)); err != nil {
		return fmt.Errorf("mark %s as owned by %s: %w", database, opID, err)
	}
	return nil
}

// clearOwned removes the marker: the database is complete.
func clearOwned(targetDir, database string) error {
	root, err := os.OpenRoot(targetDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := removeFile(root, markerPath(database)); err != nil {
		return fmt.Errorf("clear the ownership marker of %s: %w", database, err)
	}
	return nil
}

// UnfinishedState says what becomes of a database that carries the
// ownership marker.
type UnfinishedState string

const (
	// UnfinishedWaiting: the marker names an operation of this manager that
	// is still running, or whose leftover the next restore or HA bootstrap
	// of the database clears or removes.
	UnfinishedWaiting UnfinishedState = "WaitingForOperation"
	// UnfinishedRefused: no operation of this manager removes or creates over
	// the directory. The marker is unreadable, was written by the image's
	// entrypoint, names an operation this manager does not know or cannot
	// read, or names a restore that recorded its data as restored.
	UnfinishedRefused UnfinishedState = "Refused"
)

// UnfinishedDatabase is what the role answer reports of a database that
// carries the ownership marker. The image's entrypoint neither creates nor
// starts such a database.
type UnfinishedDatabase struct {
	Database string `json:"database"`
	// Operation is the operation ID the marker names; empty when it names
	// none.
	Operation string          `json:"operation,omitempty"`
	State     UnfinishedState `json:"state"`
	// Reason says why the database is in State.
	Reason string `json:"reason"`
}

// unfinishedDatabase reports the member's database when it carries the
// ownership marker, judged as reclaimIncomplete judges it. It only reads: it
// removes, clears and starts nothing. It is nil for a database without the
// marker and for a member that was not told its database.
func (s *Server) unfinishedDatabase() *UnfinishedDatabase {
	database := s.standaloneDB
	if database == "" {
		database = s.replicationDB
	}
	if !databaseNamePattern.MatchString(database) {
		return nil
	}
	root, err := os.OpenRoot(s.restoreRoots.Target)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(markerPath(database))
	if os.IsNotExist(err) {
		return nil
	}
	u := &UnfinishedDatabase{Database: database, State: UnfinishedRefused}
	if err != nil {
		u.Reason = fmt.Sprintf("the ownership marker could not be read: %v", err)
		return u
	}
	id := strings.TrimSpace(string(data))
	switch {
	case id == entrypointMarker:
		u.Reason = "the image's entrypoint did not complete its createdb; no operation of the Instance Manager removes it"
		return u
	case !isValidOperationID(id):
		u.Reason = "the ownership marker is unreadable: it names no operation"
		return u
	case s.store == nil:
		u.Operation = id
		u.Reason = "the marker names operation " + id + ", which this manager does not know: it has no operation store"
		return u
	}
	u.Operation = id
	op, err := s.store.Get(id)
	if errors.Is(err, ErrOperationNotFound) {
		u.Reason = "the marker names operation " + id + ", which this manager does not know"
		return u
	}
	if err != nil {
		u.Reason = fmt.Sprintf("the record of operation %s could not be read: %v", id, err)
		return u
	}
	switch {
	case op.State == OpCompleted:
		u.State, u.Reason = UnfinishedWaiting,
			"operation "+id+" completed and left its marker; the next restore or HA bootstrap of the database clears it"
	case op.State == OpFailed && op.Restored != nil:
		u.Reason = "operation " + id + " recorded its data as restored before it failed; " +
			"only a restore of the same request takes it over, and no operation removes it"
	case op.State == OpFailed:
		u.State, u.Reason = UnfinishedWaiting,
			"operation "+id+" failed; the next restore or HA bootstrap of the database removes what it left"
	default:
		u.State, u.Reason = UnfinishedWaiting, "operation "+id+" is "+string(op.State)
	}
	return u
}

// entrypointMarker is the value the image's entrypoint writes into the
// marker around its own createdb (build/instance-manager/entrypoint.sh).
const entrypointMarker = "entrypoint-createdb"

// reclaimIncomplete removes what an earlier, failed operation of this manager
// left of database, so that the next attempt starts from an empty target. It
// does nothing when the directory carries no marker; whether such a directory
// may be used is the caller's guard to decide.
//
// It refuses, and leaves everything in place, when the marker cannot be tied
// to a Failed operation in the store: an unreadable marker, an unknown
// operation, or one that is still running. It also refuses for a restore that
// recorded its data as restored: that database may have been started and
// served clients, so it is only ever started again (adoptRestored).
func (s *Server) reclaimIncomplete(database string) error {
	if !databaseNamePattern.MatchString(database) {
		return fmt.Errorf("database name %q is not a plain identifier", database)
	}
	target := s.restoreRoots.Target
	root, err := os.OpenRoot(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open the database root: %w", err)
	}
	defer func() { _ = root.Close() }()

	data, err := root.ReadFile(markerPath(database))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the ownership marker of %s: %w", database, err)
	}
	id := strings.TrimSpace(string(data))
	where := filepath.Join(target, database)
	if !isValidOperationID(id) {
		return fmt.Errorf("%s carries an unreadable ownership marker; refusing to touch it", where)
	}
	op, err := s.store.Get(id)
	if errors.Is(err, ErrOperationNotFound) {
		return fmt.Errorf("%s is marked by operation %s, which this manager does not know; refusing to touch it", where, id)
	}
	if err != nil {
		return err
	}
	switch op.State {
	case OpCompleted:
		// The command succeeded and only the marker was left: the data is whole.
		return clearOwned(target, database)
	case OpFailed:
		if op.Restored != nil {
			return fmt.Errorf("%s holds the data operation %s restored, which may have been started; refusing to remove it", where, id)
		}
	default:
		return fmt.Errorf("%s belongs to operation %s, which is %s; refusing to touch it", where, id, op.State)
	}

	if err := root.RemoveAll(database); err != nil {
		return fmt.Errorf("remove the unfinished %s: %w", where, err)
	}
	return unregisterDatabase(root, database)
}

// adoptRestored takes over, for the restore operation id, the data a failed
// restore of the same request left after restoredb had succeeded: id records
// the same restored artifact and then marks the directory as its own, in that
// order, so that the data is never without a durable owner. It returns the
// restored artifact, or nil when there is nothing to take over; anything that
// is in the way is then reclaimIncomplete's to judge.
func (s *Server) adoptRestored(id, database string) (*OperationArtifact, error) {
	if !databaseNamePattern.MatchString(database) {
		return nil, fmt.Errorf("database name %q is not a plain identifier", database)
	}
	root, err := os.OpenRoot(s.restoreRoots.Target)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the database root: %w", err)
	}
	defer func() { _ = root.Close() }()

	data, err := root.ReadFile(markerPath(database))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the ownership marker of %s: %w", database, err)
	}
	previous, err := s.store.Get(strings.TrimSpace(string(data)))
	if errors.Is(err, ErrOperationNotFound) {
		// An unknown or unreadable marker is reclaimIncomplete's to refuse.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	if previous.Kind != OpRestore || previous.State != OpFailed || previous.Restored == nil ||
		previous.RequestHash != current.RequestHash {
		return nil, nil
	}
	restored := *previous.Restored
	if _, err := s.store.Update(id, func(op *Operation) { op.Restored = &restored }); err != nil {
		return nil, err
	}
	if err := markOwned(root, database, id); err != nil {
		return nil, err
	}
	return &restored, nil
}

// unregisterDatabase removes database's line from databases.txt and leaves
// every other line as it is.
func unregisterDatabase(root *os.Root, database string) error {
	databasesTxtMu.Lock()
	defer databasesTxtMu.Unlock()
	return unregisterDatabaseLocked(root, database, false)
}

// unregisterDatabaseLocked is unregisterDatabase with databasesTxtMu held.
// With removeEmpty, a databases.txt that registers nothing else is removed.
func unregisterDatabaseLocked(root *os.Root, database string, removeEmpty bool) error {
	data, err := root.ReadFile(databasesTxt)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	var kept strings.Builder
	removed := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == database {
			removed = true
			continue
		}
		kept.WriteString(line)
		kept.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", databasesTxt, err)
	}
	if removeEmpty && strings.TrimSpace(kept.String()) == "" {
		if err := removeFile(root, databasesTxt); err != nil {
			return fmt.Errorf("unregister %s from %s: %w", database, databasesTxt, err)
		}
		return nil
	}
	if !removed {
		return nil
	}
	if err := replaceFile(root, databasesTxt, []byte(kept.String())); err != nil {
		return fmt.Errorf("unregister %s from %s: %w", database, databasesTxt, err)
	}
	return nil
}
