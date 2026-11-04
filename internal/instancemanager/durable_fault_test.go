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

var errInjected = errors.New("injected: input/output error")

const (
	faultRename = "rename"
	// volumeContent stands for a database volume's bytes.
	volumeContent = "volume"
)

// failRenameOf makes the rename of a metadata write into name fail, as a
// full or failing file system fails it.
func failRenameOf(t *testing.T, name string) {
	t.Helper()
	renameMetadata = func(root *os.Root, oldname, newname string) error {
		if newname == name {
			return errInjected
		}
		return root.Rename(oldname, newname)
	}
	t.Cleanup(func() {
		renameMetadata = func(root *os.Root, oldname, newname string) error { return root.Rename(oldname, newname) }
	})
}

// failSyncAfterRenameOf makes the directory sync of dir fail once a metadata
// write has renamed its new content into name, as an I/O error at fsync
// does: the new content is in place but not known to survive a power loss.
// Syncs before that rename, of other writes, succeed.
func failSyncAfterRenameOf(t *testing.T, name, dir string) {
	t.Helper()
	renamed := false
	renameMetadata = func(root *os.Root, oldname, newname string) error {
		err := root.Rename(oldname, newname)
		if err == nil && newname == name {
			renamed = true
		}
		return err
	}
	syncMetadataDir = func(root *os.Root, d string) error {
		if renamed && d == dir {
			return errInjected
		}
		return syncDir(root, d)
	}
	t.Cleanup(func() {
		renameMetadata = func(root *os.Root, oldname, newname string) error { return root.Rename(oldname, newname) }
		syncMetadataDir = syncDir
	})
}

func wantNoTemporaryFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path + ".im-tmp"); !os.IsNotExist(err) {
		t.Errorf("the temporary file of %s is left (err %v)", filepath.Base(path), err)
	}
}

// A registration whose write of databases.txt fails, at the rename or at the
// directory sync after it, leaves every other database registered and takes
// back only what it made itself: its own line and the empty directory it
// created (#298).
func TestRegisterRestoreTarget_AFailedMetadataWriteKeepsOtherRegistrations(t *testing.T) {
	for _, fault := range []string{faultRename, "directory sync"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			before := registrations(dir)
			writeFile(t, filepath.Join(dir, databasesTxt), before)
			if fault == faultRename {
				failRenameOf(t, databasesTxt)
			} else {
				failSyncAfterRenameOf(t, databasesTxt, ".")
			}
			if _, err := registerRestoreTarget(dir, crashDatabase, "host-0", crashNewOwner); !errors.Is(err, errInjected) {
				t.Fatalf("registerRestoreTarget = %v, want the injected error", err)
			}
			if got := readFile(t, filepath.Join(dir, databasesTxt)); got != before {
				t.Errorf("databases.txt =\n%q\nwant the other registrations only\n%q", got, before)
			}
			if _, err := os.Lstat(filepath.Join(dir, crashDatabase)); !os.IsNotExist(err) {
				t.Errorf("the directory the registration created is left (err %v)", err)
			}
			wantNoTemporaryFile(t, filepath.Join(dir, databasesTxt))
		})
	}
}

// An unregistration whose write fails reports it and keeps every other
// database registered: at the rename the file is as it was, and after it the
// file holds the other registrations without the removed one.
func TestUnregisterDatabase_AFailedMetadataWriteKeepsOtherRegistrations(t *testing.T) {
	for _, fault := range []string{faultRename, "directory sync"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			before := registrations(dir)
			writeFile(t, filepath.Join(dir, databasesTxt), before)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if fault == faultRename {
				failRenameOf(t, databasesTxt)
			} else {
				failSyncAfterRenameOf(t, databasesTxt, ".")
			}
			if err := unregisterDatabase(root, "db2"); !errors.Is(err, errInjected) {
				t.Fatalf("unregisterDatabase = %v, want the injected error", err)
			}
			got := readFile(t, filepath.Join(dir, databasesTxt))
			if !strings.Contains(got, "db1\t") || !strings.Contains(got, "db3\t") {
				t.Errorf("databases.txt = %q, lost another database's registration", got)
			}
			if fault == faultRename && got != before {
				t.Errorf("databases.txt after a failed rename = %q, want it unchanged", got)
			}
			wantNoTemporaryFile(t, filepath.Join(dir, databasesTxt))
		})
	}
}

// Handing a marked database to another operation whose marker write fails
// reports it and removes nothing: the database's files stay, and after a
// failed rename the marker still names the earlier owner. A marker that may
// name either owner is never a reason to remove the data.
func TestMarkOwned_AFailedMetadataWriteRemovesNothing(t *testing.T) {
	for _, fault := range []string{faultRename, "marker directory sync", "database root sync"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, crashMarked, ownershipMarker)
			volume := filepath.Join(dir, crashMarked, crashMarked)
			writeFile(t, marker, crashOldOwner+"\n")
			writeFile(t, volume, volumeContent)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			switch fault {
			case faultRename:
				failRenameOf(t, markerPath(crashMarked))
			case "marker directory sync":
				failSyncAfterRenameOf(t, markerPath(crashMarked), crashMarked)
			default:
				failSyncAfterRenameOf(t, markerPath(crashMarked), ".")
			}
			if err := markOwned(root, crashMarked, crashNewOwner); !errors.Is(err, errInjected) {
				t.Fatalf("markOwned = %v, want the injected error", err)
			}
			if got := readFile(t, volume); got != volumeContent {
				t.Errorf("the database's volume = %q, want it kept", got)
			}
			got := readFile(t, marker)
			if fault == faultRename && got != crashOldOwner+"\n" {
				t.Errorf("marker after a failed rename = %q, want the earlier owner", got)
			}
			if got != crashOldOwner+"\n" && got != crashNewOwner+"\n" {
				t.Errorf("marker = %q, want one of the two owners", got)
			}
			wantNoTemporaryFile(t, marker)
		})
	}
}
