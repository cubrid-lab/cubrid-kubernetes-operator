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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// readCountStore counts the reads of every object and can answer a later read
// of one key with other bytes, as a store that changed in between would.
type readCountStore struct {
	*fakeObjectStore
	gets        map[string]int
	changeAfter string // a key whose second and later reads return changed bytes
}

func (c *readCountStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if c.gets == nil {
		c.gets = map[string]int{}
	}
	c.gets[key]++
	if c.changeAfter != "" && strings.HasSuffix(key, c.changeAfter) && c.gets[key] > 1 {
		return io.NopCloser(bytes.NewReader([]byte("changed-after-verification"))), nil
	}
	return c.fakeObjectStore.Get(ctx, bucket, key)
}

// putManifest stores a manifest that lists objects under the given keys, each
// with the correct size and checksum of content, and stores the objects where
// the restore would read them. It is what a wrong or hostile manifest in the
// bucket looks like: internally consistent, with keys of its author's choice.
func putManifest(t *testing.T, store *fakeObjectStore, bucket, prefix, content string, keys ...string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	m := BackupManifest{
		ManifestVersion: ManifestVersion, Database: dbName, CubridVersion: testCubridVersion,
		SourceRole: string(RoleSlave),
	}
	for _, key := range keys {
		m.Objects = append(m.Objects, ManifestObject{Key: key, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
		store.objects[bucket+"/"+path.Join(prefix, key)] = []byte(content)
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	store.objects[bucket+"/"+path.Join(prefix, ManifestObjectName)] = data
}

// A key in the manifest must not be able to place a file outside the staging
// directory (#195).
func TestRestore_RejectsManifestKeysOutsideTheBackupDirectory(t *testing.T) {
	for _, key := range []string{
		"../../victim",        // leaves the staging directory
		"backup/../../victim", // the same, behind a valid-looking start
		"/etc/victim",         // absolute
		"elsewhere/file",      // not below backup/
		"backup//file",        // not normalised
		"backup/./file",       // not normalised
		"backup",              // the directory itself
		"backup/",             // no file name
		"",                    // empty
	} {
		t.Run(key, func(t *testing.T) {
			const bucket, prefix = "cubrid-backups", "prod/example/uid-1"
			store := newFakeStore()
			req := baseRestoreRequest(t, bucket, prefix)
			// The file a traversal out of <base>/restore-staging/op would hit.
			victim := filepath.Join(filepath.Dir(req.TargetDir), "victim")
			if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			putManifest(t, store, bucket, prefix, "overwritten", key)
			cli := &restoreCLI{}

			if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
				t.Fatal("a manifest with this key was restored")
			}
			if data, _ := os.ReadFile(victim); string(data) != "original" {
				t.Errorf("a file outside the staging directory was changed to %q", data)
			}
			if len(cli.calls) != 0 {
				t.Errorf("restoredb ran: %v", cli.calls)
			}
			if got := databasesTxtEntry(t, req.TargetDir, dbName); got != nil {
				t.Errorf("the database was registered: %q", got)
			}
			if _, err := os.Stat(filepath.Join(req.TargetDir, dbName)); !os.IsNotExist(err) {
				t.Errorf("the database directory was created (err=%v)", err)
			}
		})
	}
}

func TestRestore_RejectsDuplicateManifestKeys(t *testing.T) {
	const bucket, prefix = "cubrid-backups", "prod/example/uid-1"
	store := newFakeStore()
	req := baseRestoreRequest(t, bucket, prefix)
	putManifest(t, store, bucket, prefix, backupContent, "backup/vol", "backup/vol")
	cli := &restoreCLI{}
	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
		t.Fatal("a manifest that lists a key twice was restored")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb ran: %v", cli.calls)
	}
}

// A symbolic link inside the staging directory must not lead a write outside.
func TestRestore_DoesNotFollowALinkOutOfTheStagingDirectory(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	req := baseRestoreRequest(t, bucket, prefix)
	outside := filepath.Join(filepath.Dir(req.TargetDir), "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(req.StagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(req.StagingDir, "backup")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	cli := &restoreCLI{}
	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
		t.Fatal("the restore wrote through a link that leaves the staging directory")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files were written outside the staging directory: %v", entries)
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb ran: %v", cli.calls)
	}
}

// The bytes that were checked are the bytes that are restored: every object is
// read once, and what is staged has the manifest's size and checksum.
func TestRestore_RestoresExactlyTheBytesItVerified(t *testing.T) {
	inner, bucket, prefix := stageUploadedArtifact(t)
	store := &readCountStore{fakeObjectStore: inner, changeAfter: stagedFileName}
	req := baseRestoreRequest(t, bucket, prefix)
	staged := filepath.Join(req.StagingDir, "backup", stagedFileName)
	var atRestore string
	cli := &restoreCLI{}
	cli.onRun = func() {
		data, _ := os.ReadFile(staged)
		atRestore = string(data)
	}

	if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if atRestore != backupContent {
		t.Errorf("restoredb was given %q, want the verified content %q", atRestore, backupContent)
	}
	for key, n := range store.gets {
		if n != 1 {
			t.Errorf("object %s was read %d times, want once", key, n)
		}
	}
}

// A staged file that does not match the manifest never reaches restoredb and
// is not left behind.
func TestRestore_MismatchingObjectIsNotStaged(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	store.corruptKey = stagedFileName
	req := baseRestoreRequest(t, bucket, prefix)
	cli := &restoreCLI{}
	_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err == nil {
		t.Fatal("a corrupted object was restored")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb ran: %v", cli.calls)
	}
	if _, statErr := os.Stat(filepath.Join(req.StagingDir, "backup", stagedFileName)); !os.IsNotExist(statErr) {
		t.Errorf("the mismatching file is still staged (err=%v)", statErr)
	}
	if got := databasesTxtEntry(t, req.TargetDir, dbName); got != nil {
		t.Errorf("the database was registered: %q", got)
	}
}
