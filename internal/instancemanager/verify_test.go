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
	"fmt"
	"strings"
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
	res, err := UploadBackup(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	return store, spec.Bucket, uploadedPrefix(t, res.ManifestURI, spec.Bucket)
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

// A manifest of version 1 was written before #756. Its sourceRole is the role
// the backup request named, not the role the member reported, so a backup a
// demoted master took can say master. It cannot show that a seed artifact was
// taken on a master, and a seed restore refuses it. A restore into a new
// cluster, which requires no role, still takes it: its other fields keep their
// meaning, and its cubridVersion, the declared spec.version before #621, is
// checked by engine series only (#810).
func TestManifest_VerifyAVersion1Manifest(t *testing.T) {
	v1 := BackupManifest{
		ManifestVersion: 1,
		Database:        dbName,
		CubridVersion:   declaredSeries,
		Level:           0,
		SourceRole:      string(RoleMaster),
		SourceInstance:  "m-0",
		Objects:         []ManifestObject{{Key: "backup/x", SizeBytes: 3, SHA256: strings.Repeat("0", 64)}},
	}
	if ManifestVersion == 1 {
		t.Fatal("ManifestVersion is still 1: a manifest of this build cannot be told from one written before #756")
	}
	if err := v1.Verify(goodManifestExpectation()); err != nil {
		t.Errorf("a restore into a new cluster refused a version 1 manifest: %v", err)
	}
	seed := goodManifestExpectation()
	seed.RequireSourceRole, seed.SourceInstance = string(RoleMaster), "m-0"
	if err := v1.Verify(seed); err == nil {
		t.Error("a seed restore accepted a version 1 manifest, whose sourceRole is only the role the request named")
	}
	current := v1
	current.ManifestVersion = ManifestVersion
	if err := current.Verify(seed); err != nil {
		t.Errorf("a seed restore refused a current manifest of a master: %v", err)
	}
}

func TestParseManifest_ReadsVersion1AndTheCurrentVersion(t *testing.T) {
	for _, v := range []int{1, ManifestVersion} {
		m, err := ParseManifest(fmt.Appendf(nil, `{"manifestVersion":%d,"database":%q}`, v, dbName))
		if err != nil || m.ManifestVersion != v {
			t.Errorf("version %d: manifest %+v, err %v", v, m, err)
		}
	}
	for _, v := range []int{0, ManifestVersion + 1} {
		if _, err := ParseManifest(fmt.Appendf(nil, `{"manifestVersion":%d}`, v)); err == nil {
			t.Errorf("version %d was accepted", v)
		}
	}
}
