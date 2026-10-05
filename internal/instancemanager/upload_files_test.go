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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// uploadWithin runs UploadBackup and fails the test when it does not return in
// time, which is what opening a named pipe without a writer would cause.
func uploadWithin(t *testing.T, store ObjectStore, spec UploadSpec) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := UploadBackup(context.Background(), store, spec)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the upload is still waiting after 5 seconds")
		return nil
	}
}

// A backup uploads ordinary files only. A symbolic link would upload whatever
// it points at, also outside the staging directory (#203).
func TestUploadBackup_RejectsASymbolicLink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	if err := os.Symlink(outside, filepath.Join(staging, "linked")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	store := newFakeStore()

	err := uploadWithin(t, store, baseSpec(staging))
	if err == nil {
		t.Fatal("a staging directory with a symbolic link was uploaded")
	}
	if !strings.Contains(err.Error(), "linked") || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("the error does not name the entry and the reason: %v", err)
	}
	for _, key := range store.keys() {
		if strings.Contains(key, "linked") || string(store.objects[key]) == "not a backup" {
			t.Errorf("the link's target was uploaded as %s", key)
		}
		if strings.HasSuffix(key, ManifestObjectName) {
			t.Errorf("a manifest was written for a rejected backup: %s", key)
		}
	}
}

// A named pipe would make the upload wait for a writer that never comes.
func TestUploadBackup_RejectsANamedPipe(t *testing.T) {
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	if err := syscall.Mkfifo(filepath.Join(staging, "pipe"), 0o600); err != nil {
		t.Skipf("cannot create a named pipe here: %v", err)
	}
	store := newFakeStore()

	err := uploadWithin(t, store, baseSpec(staging))
	if err == nil {
		t.Fatal("a staging directory with a named pipe was uploaded")
	}
	if !strings.Contains(err.Error(), "pipe") || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("the error does not name the entry and the reason: %v", err)
	}
}

// A link to a directory is not followed either.
func TestUploadBackup_RejectsALinkToADirectory(t *testing.T) {
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "inner"), []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	if err := os.Symlink(elsewhere, filepath.Join(staging, "dirlink")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	store := newFakeStore()
	if err := uploadWithin(t, store, baseSpec(staging)); err == nil {
		t.Fatal("a staging directory with a link to a directory was uploaded")
	}
	for _, key := range store.keys() {
		if strings.Contains(key, "inner") {
			t.Errorf("a file behind a directory link was uploaded as %s", key)
		}
	}
}

// Ordinary files, also in a subdirectory, upload as before.
func TestUploadBackup_RegularFilesStillUpload(t *testing.T) {
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent, "sub/vol_1": "second"})
	store := newFakeStore()
	if err := uploadWithin(t, store, baseSpec(staging)); err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	spec := baseSpec(staging)
	for _, rel := range []string{stagedFileName, "sub/vol_1"} {
		key := spec.Bucket + "/" + spec.Prefix + "/backup/" + rel
		if _, ok := store.objects[key]; !ok {
			t.Errorf("%s was not uploaded; have %v", key, store.keys())
		}
	}
}
