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
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A later failed upload must preserve the whole previously published artifact,
// including the bytes named by its manifest, not just the completion marker.
func TestUploadBackup_PreservesCompletedArtifactAfterFailedRetry(t *testing.T) {
	for _, failure := range []string{"data-put", "verification", "manifest"} {
		t.Run(failure, func(t *testing.T) {
			store := newFakeStore()
			spec := baseSpec(stageBackup(t, map[string]string{stagedFileName: "original-backup"}))
			first, err := UploadBackup(context.Background(), store, spec)
			if err != nil {
				t.Fatal(err)
			}
			prefix := uploadedPrefix(t, first.ManifestURI, spec.Bucket)
			original := make(map[string][]byte, len(store.objects))
			for key, data := range store.objects {
				original[key] = bytes.Clone(data)
			}
			switch failure {
			case "data-put":
				store.putErrKey = stagedFileName
			case "verification":
				store.shortKey = stagedFileName
			case "manifest":
				store.putErrKey = ManifestObjectName
			}
			retry := baseSpec(stageBackup(t, map[string]string{stagedFileName: "replaced-by-new"}))
			if _, err := UploadBackup(context.Background(), store, retry); err == nil {
				t.Fatal("the failed retry was reported as completed")
			}
			for key, data := range original {
				if !bytes.Equal(store.objects[key], data) {
					t.Errorf("completed artifact changed after %s failure: key=%s before=%q after=%q",
						failure, key, data, store.objects[key])
				}
			}
			store.shortKey = ""
			if _, err := VerifyArtifact(context.Background(), store, spec.Bucket, prefix,
				goodManifestExpectation()); err != nil {
				t.Errorf("original artifact no longer verifies: %v", err)
			}
		})
	}
}

// synchronizedStore protects the existing deterministic store while two
// independent uploaders interleave before either publishes its manifest.
type synchronizedStore struct {
	*fakeObjectStore
	mu sync.Mutex
}

func (s *synchronizedStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeObjectStore.Put(ctx, bucket, key, r, size, contentType)
}
func (s *synchronizedStore) StatSize(ctx context.Context, bucket, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeObjectStore.StatSize(ctx, bucket, key)
}
func (s *synchronizedStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeObjectStore.Get(ctx, bucket, key)
}

type manifestBarrierStore struct {
	ObjectStore
	arrived chan struct{}
	release chan struct{}
}

func (s *manifestBarrierStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) (int64, error) {
	if strings.HasSuffix(key, "/"+ManifestObjectName) {
		select {
		case s.arrived <- struct{}{}:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return s.ObjectStore.Put(ctx, bucket, key, r, size, contentType)
}

func TestUploadBackup_ConcurrentAttemptsDoNotSharePublishedObjects(t *testing.T) {
	testConcurrentUploads(t, &synchronizedStore{fakeObjectStore: newFakeStore()}, "cubrid-backups")
}

func testConcurrentUploads(t *testing.T, objects ObjectStore, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barrier := &manifestBarrierStore{ObjectStore: objects, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	type result struct {
		artifact UploadResult
		err      error
	}
	results := make(chan result, 2)
	content := []string{"first snapshot", "other snapshot"}
	for _, data := range content {
		spec := baseSpec(stageBackup(t, map[string]string{stagedFileName: data}))
		spec.Bucket = bucket
		// Even a reused operation ID after store loss must not reuse data keys.
		spec.Manifest.OperationID = "op-reused-after-store-loss"
		go func() { res, err := UploadBackup(ctx, barrier, spec); results <- result{res, err} }()
	}
	for range 2 {
		select {
		case <-barrier.arrived:
		case <-ctx.Done():
			t.Fatal("both attempts did not reach publication")
		}
	}
	close(barrier.release)
	uris := map[string]bool{}
	seen := map[string]bool{}
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if uris[got.artifact.ManifestURI] {
			t.Fatal("concurrent attempts published one shared manifest URI")
		}
		uris[got.artifact.ManifestURI] = true
		prefix := uploadedPrefix(t, got.artifact.ManifestURI, bucket)
		m, err := VerifyArtifact(ctx, objects, bucket, prefix, goodManifestExpectation())
		if err != nil {
			t.Fatal(err)
		}
		if m.OperationID != "op-reused-after-store-loss" {
			t.Fatalf("operation identity changed: %q", m.OperationID)
		}
		data, err := readObject(ctx, objects, bucket, path.Join(prefix, m.Objects[0].Key))
		if err != nil {
			t.Fatal(err)
		}
		seen[string(data)] = true
		staging := t.TempDir()
		if _, err := stageArtifact(ctx, objects, bucket, prefix, goodManifestExpectation(), staging); err != nil {
			t.Fatal(err)
		}
		local, err := os.ReadFile(filepath.Join(staging, "backup", stagedFileName))
		if err != nil || !bytes.Equal(local, data) {
			t.Fatalf("original restore filename/data changed: %q %v", local, err)
		}
		t.Logf("verified isolated attempt %s digest=%s", got.artifact.ManifestURI, got.artifact.ManifestDigest)
	}
	for _, data := range content {
		if !seen[data] {
			t.Fatalf("snapshot %q was overwritten", data)
		}
	}
}

func TestUploadBackup_SeedPurposeIsExplicitAndConfined(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, role string
		valid              bool
	}{
		{"first seed", "prod/cluster-uid/seed/appdb", string(RoleMaster), true},
		{"later seed", "prod/cluster-uid/seed-peer-from-master-r1/appdb", string(RoleMaster), true},
		{"ordinary backup", "prod/cluster-uid/appdb/backup-uid", string(RoleMaster), false},
		{"other cluster", "prod/other/seed/appdb", string(RoleMaster), false},
		{"other database", "prod/cluster-uid/seed/otherdb", string(RoleMaster), false},
		{"not master", "prod/cluster-uid/seed/appdb", string(RoleSlave), false},
		{"empty seed artifact", "prod/cluster-uid/seed-/appdb", string(RoleMaster), false},
		{"escape", "../cluster-uid/seed/appdb", string(RoleMaster), false},
		{"normalization", "prod/x/../cluster-uid/seed/appdb", string(RoleMaster), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec(stageBackup(t, map[string]string{stagedFileName: backupContent}))
			spec.Prefix, spec.Manifest.SourceRole, spec.Seed = tc.prefix, tc.role, true
			store := newFakeStore()
			got, err := UploadBackup(context.Background(), store, spec)
			if (err == nil) != tc.valid {
				t.Fatalf("seed validity: %v", err)
			}
			if !tc.valid {
				if len(store.objects) != 0 {
					t.Fatal("invalid seed mutated object storage")
				}
				return
			}
			if got.ManifestURI != "s3://"+spec.Bucket+"/"+spec.Prefix+"/manifest.json" {
				t.Fatalf("seed location changed: %s", got.ManifestURI)
			}
			// No inferred purpose: an ordinary upload with the same prefix is
			// isolated and does not replace the explicitly published seed.
			original := bytes.Clone(store.objects[spec.Bucket+"/"+spec.Prefix+"/manifest.json"])
			spec.Seed = false
			if _, err := UploadBackup(context.Background(), store, spec); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, store.objects[spec.Bucket+"/"+spec.Prefix+"/manifest.json"]) {
				t.Fatal("implicit seed replacement")
			}
		})
	}
}

func TestServer_Backup_StoreLossDoesNotReplaceCompletedArtifact(t *testing.T) {
	objects := newFakeStore()
	root := t.TempDir()
	body := backupBodyOf(dbName, filepath.Join(root, "backup"), "prod/backup-uid")
	var previous map[string][]byte
	var previousURI string
	for range 2 {
		store, err := NewOperationStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(newWritingCLI(), "tok").WithOperationStore(store).WithObjectStore(objects).WithBackupStagingRoot(root)
		h := srv.Handler()
		rr := postBackup(t, h, "same-backup-idempotency-key", body)
		var op Operation
		if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		final := pollUntilSettled(t, srv, h, op.ID)
		if final.State != OpCompleted || final.Artifact == nil {
			t.Fatalf("backup did not complete: %+v", final)
		}
		if final.Artifact.ManifestURI == previousURI {
			t.Fatal("empty operation store reused the completed URI")
		}
		for key, data := range previous {
			if !bytes.Equal(data, objects.objects[key]) {
				t.Fatalf("store-loss retry changed %s", key)
			}
		}
		previousURI = final.Artifact.ManifestURI
		previous = make(map[string][]byte, len(objects.objects))
		for key, data := range objects.objects {
			previous[key] = bytes.Clone(data)
		}
	}
}

func TestUploadBackup_PreservesLegacyCompletedArtifact(t *testing.T) {
	store := newFakeStore()
	spec := baseSpec(stageBackup(t, map[string]string{stagedFileName: "new snapshot"}))
	putManifest(t, store, spec.Bucket, spec.Prefix, "legacy snapshot", "backup/"+stagedFileName)
	original := make(map[string][]byte, len(store.objects))
	for key, data := range store.objects {
		original[key] = bytes.Clone(data)
	}
	res, err := UploadBackup(context.Background(), store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if uploadedPrefix(t, res.ManifestURI, spec.Bucket) == spec.Prefix {
		t.Fatal("legacy completed URI reused")
	}
	for key, data := range original {
		if !bytes.Equal(data, store.objects[key]) {
			t.Fatalf("legacy artifact changed: %s", key)
		}
	}
	if _, err := VerifyArtifact(context.Background(), store, spec.Bucket, spec.Prefix, goodManifestExpectation()); err != nil {
		t.Fatal(err)
	}
}

type manifestResponseLostStore struct {
	ObjectStore
	published string
}

func (s *manifestResponseLostStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) (int64, error) {
	n, err := s.ObjectStore.Put(ctx, bucket, key, r, size, contentType)
	if err == nil && strings.HasSuffix(key, "/"+ManifestObjectName) {
		s.published = key
		return 0, context.Canceled
	}
	return n, err
}
func testLostManifestResponse(t *testing.T, objects ObjectStore, bucket string) {
	t.Helper()
	spec := baseSpec(stageBackup(t, map[string]string{stagedFileName: "original snapshot"}))
	spec.Bucket = bucket
	lost := &manifestResponseLostStore{ObjectStore: objects}
	if _, err := UploadBackup(context.Background(), lost, spec); err == nil {
		t.Fatal("lost publication response passed")
	}
	if lost.published == "" {
		t.Fatal("failure did not occur after manifest storage")
	}
	original, err := readObject(context.Background(), objects, bucket, lost.published)
	if err != nil {
		t.Fatal(err)
	}
	spec.StagingDir = stageBackup(t, map[string]string{stagedFileName: "retried snapshot"})
	if _, err := UploadBackup(context.Background(), objects, spec); err != nil {
		t.Fatal(err)
	}
	after, err := readObject(context.Background(), objects, bucket, lost.published)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("ambiguous completion changed on retry: %v", err)
	}
	if _, err := VerifyArtifact(context.Background(), objects, bucket, path.Dir(lost.published), goodManifestExpectation()); err != nil {
		t.Fatal(err)
	}
	t.Logf("ambiguous completion remains intact at s3://%s/%s", bucket, lost.published)
}
func TestUploadBackup_LostManifestResponseDoesNotPermitReplacement(t *testing.T) {
	testLostManifestResponse(t, newFakeStore(), "cubrid-backups")
}
