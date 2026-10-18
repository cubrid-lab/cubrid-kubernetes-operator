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
	"fmt"
	"os"
	"path/filepath"
)

// Backup staging layout (ADR-0007). A request names a destination directly
// below the staging root; the operation stages in a directory of its own
// below it, named after its operation ID:
//
//	<staging root>/<destination>/<operation id>/
//
// The operation creates that directory empty, backupdb writes only there,
// the upload reads only there, and the operation removes only that
// directory (and the destination once it is empty). An operation therefore
// never uploads or removes a file that another operation wrote.
//
// An entry of a destination is owned by a live operation when it is named
// after an operation that the store records as not ended, or whose record
// cannot be read, or when a process holds a file in it (a backupdb -C that
// cub_server still writes after its operation failed, POC-27). Every other
// entry (a directory of an operation that has ended or that the store does
// not know, a file staged directly in the destination by an earlier manager)
// is abandoned and may be removed.

// ErrBackupStagingInUse refuses a backup whose staging destination is owned
// by another operation that is still running. Nothing is recorded, run or
// removed for the refused request.
var ErrBackupStagingInUse = errors.New("the backup staging destination is owned by another operation that is still running")

// stagingOwnerLive reports whether the entry name of the destination belongs
// to a live operation. A record that cannot be read counts as live: what it
// owns cannot be shown to be abandoned. That keeps the destination blocked
// until the record is repaired, so it is logged as a warning. An entry that
// a process holds a file in, or whose holders cannot be told, counts as live
// too, under its own name: it is neither removed nor reused while something
// may still write it, and is logged as a warning.
func (s *Server) stagingOwnerLive(destination, name string) (*Operation, bool) {
	if op, live := s.stagingRecordLive(destination, name); live {
		return op, true
	}
	held, err := stagingHolders(filepath.Join(s.backupStagingRoot, destination, name))
	if err != nil || len(held) > 0 {
		s.log().Warn("Kept backup staging that a process holds open; the destination is kept",
			"event", "backup_staging_held", "destination", destination, "entry", name,
			"holders", describeHolders(held, err))
		return &Operation{ID: name}, true
	}
	return nil, false
}

// stagingRecordLive is stagingOwnerLive by the operation record alone.
func (s *Server) stagingRecordLive(destination, name string) (*Operation, bool) {
	if !isValidOperationID(name) {
		return nil, false
	}
	op, err := s.store.Get(name)
	if errors.Is(err, ErrOperationNotFound) {
		return nil, false
	}
	if err != nil {
		s.log().Warn("Could not read the operation record that owns backup staging; the destination is kept",
			"destination", destination, "operationID", name, "err", err)
		return &Operation{ID: name}, true
	}
	return op, !op.State.IsTerminal()
}

// liveStagingOwners returns the live operations that own an entry of the
// destination name below the staging root; none when it does not exist.
func (s *Server) liveStagingOwners(root *os.Root, name string) ([]*Operation, []os.DirEntry, error) {
	dir, err := root.Open(name)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open the backup staging destination: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, nil, fmt.Errorf("read the backup staging destination: %w", err)
	}
	var live []*Operation
	for _, e := range entries {
		if op, ok := s.stagingOwnerLive(name, e.Name()); ok {
			live = append(live, op)
		}
	}
	return live, entries, nil
}

// recordBackup records the operation of an asynchronous backup request and
// claims its staging directory. It runs under stagingMu, so that no other
// request takes the destination between the check and the claim.
//
// A destination that a live operation of another request owns is refused
// with ErrBackupStagingInUse before anything is recorded. A repeat of a
// recorded request returns its operation and an empty staging path, as does
// a new operation whose directory could not be claimed: it is recorded as
// Failed. Otherwise staging is the new operation's own, empty directory.
func (s *Server) recordBackup(key, requestHash string, req BackupRequest) (op *Operation, staging string, err error) {
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()

	root, err := s.openStagingRoot()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(req.Destination)
	live, _, err := s.liveStagingOwners(root, name)
	if err != nil {
		return nil, "", err
	}
	for _, owner := range live {
		// An owner whose record cannot be read may be this request's
		// operation.
		if owner.Kind == "" {
			if _, gerr := s.store.Get(owner.ID); gerr != nil && !errors.Is(gerr, ErrOperationNotFound) {
				return nil, "", fmt.Errorf("%w: operation %s owns an entry of %s: %w", ErrOperationRecordUnreadable, owner.ID, name, gerr)
			}
		}
		if owner.Kind != OpBackup || owner.IdempotencyKey != key {
			return nil, "", fmt.Errorf("%w: %s is owned by operation %s", ErrBackupStagingInUse, name, owner.ID)
		}
	}

	op, existed, err := s.store.FindOrCreate(OpBackup, key, requestHash, req.Database)
	if err != nil || existed {
		return op, "", err
	}
	staging, err = s.claimBackupStaging(root, name, op.ID)
	if err == nil {
		_, err = s.store.Update(op.ID, func(o *Operation) { o.StagingDir = staging })
	}
	if err != nil {
		if staging != "" {
			_ = root.RemoveAll(filepath.Join(name, op.ID))
		}
		reason := "claim the backup staging directory: " + err.Error()
		// An end that cannot be written is kept and written later (Settle);
		// until then the request is answered with the error, not with a
		// Pending operation that never runs.
		if serr := s.store.Settle(op.ID, func(o *Operation) {
			o.State = OpFailed
			o.FailureReason = reason
		}); serr != nil {
			return nil, "", fmt.Errorf("%s; recording that the operation failed: %w", reason, serr)
		}
		failed, gerr := s.store.Get(op.ID)
		if gerr != nil {
			return nil, "", gerr
		}
		return failed, "", nil
	}
	return op, staging, nil
}

// claimBackupStaging creates the destination name if needed, removes what
// abandoned operations left in it, and creates the operation's own empty
// directory in it. The caller has checked, under stagingMu, that no live
// operation owns an entry of the destination. The directory is created
// exclusively: an existing one is an error, never reused.
func (s *Server) claimBackupStaging(root *os.Root, name, id string) (string, error) {
	if err := root.Mkdir(name, 0o750); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create the backup staging destination: %w", err)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("inspect the backup staging destination: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("the backup staging destination %s is not a directory", name)
	}
	free, err := s.reclaimDestination(root, name)
	if err != nil {
		return "", err
	}
	if !free {
		return "", fmt.Errorf("%w: %s", ErrBackupStagingInUse, name)
	}
	if err := root.Mkdir(filepath.Join(name, id), 0o750); err != nil {
		return "", fmt.Errorf("create the operation's backup staging directory: %w", err)
	}
	return filepath.Join(s.backupStagingRoot, name, id), nil
}

// reclaimDestination removes every entry of the destination name that no
// live operation owns, unless one does: a destination with a live owner is
// left as it is. Links are removed, never followed. It reports whether the
// destination had no live owner; a destination it cannot read is an error,
// and nothing in it is removed.
func (s *Server) reclaimDestination(root *os.Root, name string) (bool, error) {
	live, entries, err := s.liveStagingOwners(root, name)
	if err != nil {
		return false, err
	}
	if len(live) > 0 {
		return false, nil
	}
	for _, e := range entries {
		if err := root.RemoveAll(filepath.Join(name, e.Name())); err != nil {
			s.log().Warn("Could not remove abandoned backup staging", "destination", name, "entry", e.Name(), "err", err)
		}
	}
	return true, nil
}

// reclaimBackupStaging removes, when the manager starts, the backup staging
// that operations which have ended, or that the store does not know, left
// behind: the staging of an operation that a restart interrupted, which no
// operation will remove. It is bounded to the destinations directly below
// the staging root, does not follow links, and leaves a destination that a
// live operation owns, or whose owner cannot be judged, untouched. Without
// an operation store nothing can be judged and nothing is removed.
func (s *Server) reclaimBackupStaging() {
	if s.store == nil || s.backupStagingRoot == "" {
		return
	}
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	entries, err := os.ReadDir(s.backupStagingRoot)
	if err != nil {
		return
	}
	root, err := os.OpenRoot(s.backupStagingRoot)
	if err != nil {
		s.log().Warn("Could not open the backup staging root", "err", err)
		return
	}
	defer func() { _ = root.Close() }()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !stagingNamePattern.MatchString(name) {
			continue
		}
		free, err := s.reclaimDestination(root, name)
		if err != nil {
			s.log().Warn("Could not read backup staging; the destination is kept", "destination", name, "err", err)
			continue
		}
		if free {
			if err := root.Remove(name); err == nil {
				s.log().Info("Removed abandoned backup staging", "destination", name)
			}
		}
	}
}

// openStagingRoot creates the staging root when the data volume does not
// have it yet, and opens it: every path below it is reached through the
// handle, so none lands elsewhere.
func (s *Server) openStagingRoot() (*os.Root, error) {
	if err := os.MkdirAll(s.backupStagingRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create the backup staging root: %w", err)
	}
	root, err := os.OpenRoot(s.backupStagingRoot)
	if err != nil {
		return nil, fmt.Errorf("open the backup staging root: %w", err)
	}
	return root, nil
}

// stagingRel returns dir relative to the staging root: one destination, or
// an operation's directory below one.
func (s *Server) stagingRel(dir string) (string, error) {
	rel, err := filepath.Rel(s.backupStagingRoot, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s is not below the backup staging root", dir)
	}
	return rel, nil
}
