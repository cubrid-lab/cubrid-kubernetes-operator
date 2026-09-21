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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const backupContent = "backupdata"
const stagedFileName = "pocdb_bk0v000"

// fakeObjectStore records uploads in memory and can inject failures.
type fakeObjectStore struct {
	objects    map[string][]byte
	putErrKey  string // if a key contains this substring, Put fails
	shortKey   string // if a key contains this substring, StatSize under-reports
	corruptKey string // if a key contains this substring, Get corrupts the bytes
}

func newFakeStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

func (f *fakeObjectStore) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ string) (int64, error) {
	if f.putErrKey != "" && strings.Contains(key, f.putErrKey) {
		return 0, io.ErrClosedPipe
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.objects[bucket+"/"+key] = data
	return int64(len(data)), nil
}

func (f *fakeObjectStore) StatSize(_ context.Context, bucket, key string) (int64, error) {
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return 0, os.ErrNotExist
	}
	if f.shortKey != "" && strings.Contains(key, f.shortKey) {
		return int64(len(data)) - 1, nil
	}
	return int64(len(data)), nil
}

func (f *fakeObjectStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if f.corruptKey != "" && strings.Contains(key, f.corruptKey) {
		data = append([]byte("x"), data...)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeObjectStore) keys() []string {
	ks := make([]string, 0, len(f.objects))
	for k := range f.objects {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func stageBackup(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseSpec(staging string) UploadSpec {
	return UploadSpec{
		StagingDir: staging,
		Bucket:     "cubrid-backups",
		Prefix:     "prod/example/uid-1",
		Manifest: BackupManifest{
			Database:       dbName,
			ClusterUID:     "cluster-uid",
			CubridVersion:  testCubridVersion,
			Level:          0,
			SourceInstance: "appdb-1",
			SourceRole:     string(RoleSlave),
			CreatedAt:      "2026-09-21T00:00:00Z",
		},
	}
}

func TestUploadBackup_ManifestWrittenLast(t *testing.T) {
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	store := newFakeStore()

	res, err := UploadBackup(context.Background(), store, baseSpec(staging))
	if err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	if res.ManifestURI != "s3://cubrid-backups/prod/example/uid-1/manifest.json" {
		t.Errorf("manifest URI = %s", res.ManifestURI)
	}

	// manifest.json must exist and validate its listed objects.
	manifestData, ok := store.objects["cubrid-backups/prod/example/uid-1/manifest.json"]
	if !ok {
		t.Fatal("manifest.json was not uploaded")
	}
	m, err := ParseManifest(manifestData)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Objects) != 1 || m.Objects[0].Key != "backup/"+stagedFileName {
		t.Errorf("manifest objects = %+v", m.Objects)
	}
	if m.Objects[0].SizeBytes != int64(len(backupContent)) {
		t.Errorf("object size = %d", m.Objects[0].SizeBytes)
	}
	// The data object must be present.
	if _, ok := store.objects["cubrid-backups/prod/example/uid-1/backup/pocdb_bk0v000"]; !ok {
		t.Error("data object was not uploaded")
	}
}

func TestUploadBackup_NoManifestOnDataUploadFailure(t *testing.T) {
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	store := newFakeStore()
	store.putErrKey = "pocdb_bk0v000" // data upload fails

	_, err := UploadBackup(context.Background(), store, baseSpec(staging))
	if err == nil {
		t.Fatal("expected error when a data object upload fails")
	}
	// No manifest.json => the artifact is not a completion marker.
	for _, k := range store.keys() {
		if strings.HasSuffix(k, "manifest.json") {
			t.Errorf("manifest.json must NOT be written when a data upload fails: %s", k)
		}
	}
}

func TestUploadBackup_FailsOnVerificationMismatch(t *testing.T) {
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	store := newFakeStore()
	store.shortKey = "pocdb_bk0v000" // StatSize under-reports => verification fails

	_, err := UploadBackup(context.Background(), store, baseSpec(staging))
	if err == nil {
		t.Fatal("expected error when upload verification size mismatches")
	}
	for _, k := range store.keys() {
		if strings.HasSuffix(k, "manifest.json") {
			t.Errorf("manifest.json must NOT be written when verification fails: %s", k)
		}
	}
}

func TestUploadBackup_EmptyStagingFails(t *testing.T) {
	staging := t.TempDir()
	if _, err := UploadBackup(context.Background(), newFakeStore(), baseSpec(staging)); err == nil {
		t.Error("expected error for empty staging dir")
	}
}

func TestBackupManifest_RoundTripAndDigest(t *testing.T) {
	m := BackupManifest{
		ManifestVersion: ManifestVersion,
		Database:        dbName,
		Objects:         []ManifestObject{{Key: "backup/x", SizeBytes: 3, SHA256: "abc"}},
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Database != dbName || len(got.Objects) != 1 {
		t.Errorf("round trip = %+v", got)
	}
	d1, _ := m.Marshal()
	d2, _ := got.Marshal()
	if !bytes.Equal(d1, d2) {
		t.Error("round-trip manifest is not byte-stable")
	}
	if dg, err := m.Digest(); err != nil || dg == "" {
		t.Errorf("digest = %q err=%v", dg, err)
	}
}

func TestParseManifest_RejectsUnknownVersion(t *testing.T) {
	if _, err := ParseManifest([]byte(`{"manifestVersion":999}`)); err == nil {
		t.Error("expected rejection of unknown manifest version")
	}
}
