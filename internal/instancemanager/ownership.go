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
// (ADR-0006, ADR-0008).
const ownershipMarker = ".im-operation"

func markerPath(database string) string { return filepath.Join(database, ownershipMarker) }

// markOwned records opID as the operation creating <targetDir>/<database>.
// An empty opID (a caller without a durable operation) writes nothing.
func markOwned(root *os.Root, database, opID string) error {
	if opID == "" {
		return nil
	}
	if err := root.WriteFile(markerPath(database), []byte(opID+"\n"), 0o600); err != nil {
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
	if err := root.Remove(markerPath(database)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear the ownership marker of %s: %w", database, err)
	}
	return nil
}

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
	if !removed {
		return nil
	}
	if err := root.WriteFile(databasesTxt, []byte(kept.String()), 0o600); err != nil {
		return fmt.Errorf("unregister %s from %s: %w", database, databasesTxt, err)
	}
	return nil
}
