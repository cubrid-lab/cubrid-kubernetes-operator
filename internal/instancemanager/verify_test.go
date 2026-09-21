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
	"testing"
)

const testCubridVersion = "11.4.6"

var (
	level0 = 0
	level1 = 1
)

func goodManifestExpectation() ManifestExpectation {
	return ManifestExpectation{Database: dbName, CubridVersion: testCubridVersion, Level: &level0}
}

func TestManifest_Verify(t *testing.T) {
	base := BackupManifest{
		ManifestVersion: ManifestVersion,
		Database:        dbName,
		CubridVersion:   testCubridVersion,
		Level:           0,
		SourceRole:      string(RoleSlave),
		Objects:         []ManifestObject{{Key: "backup/x", SizeBytes: 3, SHA256: "abc"}},
	}

	t.Run("accepts a matching manifest", func(t *testing.T) {
		if err := base.Verify(goodManifestExpectation()); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})
	t.Run("rejects wrong database", func(t *testing.T) {
		exp := goodManifestExpectation()
		exp.Database = "other"
		if err := base.Verify(exp); err == nil {
			t.Error("expected rejection for wrong database")
		}
	})
	t.Run("rejects wrong cubrid version", func(t *testing.T) {
		exp := goodManifestExpectation()
		exp.CubridVersion = "11.5.0"
		if err := base.Verify(exp); err == nil {
			t.Error("expected rejection for wrong cubrid version")
		}
	})
	t.Run("rejects wrong level", func(t *testing.T) {
		exp := goodManifestExpectation()
		exp.Level = &level1
		if err := base.Verify(exp); err == nil {
			t.Error("expected rejection for wrong level")
		}
	})
	t.Run("rejects wrong source role", func(t *testing.T) {
		exp := goodManifestExpectation()
		exp.RequireSourceRole = "master"
		if err := base.Verify(exp); err == nil {
			t.Error("expected rejection for wrong source role")
		}
	})
	t.Run("rejects a manifest with no objects", func(t *testing.T) {
		m := base
		m.Objects = nil
		if err := m.Verify(goodManifestExpectation()); err == nil {
			t.Error("expected rejection for empty object list")
		}
	})
}

// stageUploadedArtifact uploads a valid backup + manifest to a fake store and
// returns the store, bucket, and prefix, mirroring what UploadBackup produces.
func stageUploadedArtifact(t *testing.T) (*fakeObjectStore, string, string) {
	t.Helper()
	staging := stageBackup(t, map[string]string{stagedFileName: backupContent})
	store := newFakeStore()
	spec := baseSpec(staging)
	spec.Manifest.CubridVersion = testCubridVersion
	if _, err := UploadBackup(context.Background(), store, spec); err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	return store, spec.Bucket, spec.Prefix
}

func TestVerifyArtifact_HappyPath(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	m, err := VerifyArtifact(context.Background(), store, bucket, prefix, goodManifestExpectation())
	if err != nil {
		t.Fatalf("VerifyArtifact: %v", err)
	}
	if m.Database != dbName || len(m.Objects) != 1 {
		t.Errorf("manifest = %+v", m)
	}
}

func TestVerifyArtifact_RejectsMetadataMismatch(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	exp := goodManifestExpectation()
	exp.Database = "wrongdb"
	if _, err := VerifyArtifact(context.Background(), store, bucket, prefix, exp); err == nil {
		t.Error("expected rejection when the expected database does not match the manifest")
	}
}

func TestVerifyArtifact_RejectsChecksumDrift(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	store.corruptKey = stagedFileName // Get returns tampered bytes for the data object
	if _, err := VerifyArtifact(context.Background(), store, bucket, prefix, goodManifestExpectation()); err == nil {
		t.Error("expected rejection when a data object's checksum drifts")
	}
}

func TestVerifyArtifact_RejectsMissingObject(t *testing.T) {
	store, bucket, prefix := stageUploadedArtifact(t)
	for k := range store.objects {
		if k != bucket+"/"+prefix+"/"+ManifestObjectName {
			delete(store.objects, k)
		}
	}
	if _, err := VerifyArtifact(context.Background(), store, bucket, prefix, goodManifestExpectation()); err == nil {
		t.Error("expected rejection when a manifest-listed object is missing")
	}
}

func TestVerifyArtifact_RejectsMissingManifest(t *testing.T) {
	store := newFakeStore()
	if _, err := VerifyArtifact(context.Background(), store, "bucket", "prefix", goodManifestExpectation()); err == nil {
		t.Error("expected rejection when the manifest is absent")
	}
}
