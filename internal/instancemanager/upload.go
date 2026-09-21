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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// UploadSpec describes where a staged backup lives locally and where its objects
// go in object storage (ADR-0007).
type UploadSpec struct {
	// StagingDir is the local directory holding the `backupdb` output files.
	StagingDir string
	// Bucket / Prefix locate the artifact in object storage. Objects land under
	// <prefix>/ and the completion marker is <prefix>/manifest.json.
	Bucket string
	Prefix string
	// Manifest carries the backup metadata; its Objects list is filled in here.
	Manifest BackupManifest
}

// UploadResult reports the completed artifact (ADR-0007).
type UploadResult struct {
	ManifestURI    string
	ManifestDigest string
	SizeBytes      int64
}

// UploadBackup stages-then-uploads a completed local backup to object storage
// and writes manifest.json LAST as the atomic completion marker (ADR-0007).
// It uploads every regular file under StagingDir, records each object's size +
// SHA-256 in the manifest, verifies each upload landed (StatSize), then uploads
// the manifest. The manifest is NEVER written before all data objects succeed,
// so a partial upload can never be mistaken for a complete backup.
func UploadBackup(ctx context.Context, store ObjectStore, spec UploadSpec) (UploadResult, error) {
	files, err := stagedFiles(spec.StagingDir)
	if err != nil {
		return UploadResult{}, err
	}
	if len(files) == 0 {
		return UploadResult{}, fmt.Errorf("no backup files staged in %s", spec.StagingDir)
	}

	manifest := spec.Manifest
	manifest.ManifestVersion = ManifestVersion
	manifest.Objects = make([]ManifestObject, 0, len(files))

	for _, rel := range files {
		full := filepath.Join(spec.StagingDir, rel)
		sum, size, err := hashFile(full)
		if err != nil {
			return UploadResult{}, err
		}
		key := path.Join(spec.Prefix, "backup", rel)
		if err := putFile(ctx, store, spec.Bucket, key, full, size); err != nil {
			return UploadResult{}, err
		}
		// Verify the object landed with the expected size (ADR-0007 stage-then-
		// verify); a missing/short object must fail the whole backup.
		if got, err := store.StatSize(ctx, spec.Bucket, key); err != nil || got != size {
			return UploadResult{}, fmt.Errorf("upload verification failed for %s (size got=%d want=%d err=%v)", key, got, size, err)
		}
		manifest.Objects = append(manifest.Objects, ManifestObject{
			Key:       path.Join("backup", rel),
			SizeBytes: size,
			SHA256:    sum,
		})
	}

	// Write manifest.json LAST — the atomic completion marker (ADR-0007).
	data, err := manifest.Marshal()
	if err != nil {
		return UploadResult{}, err
	}
	manifestKey := path.Join(spec.Prefix, ManifestObjectName)
	if _, err := store.Put(ctx, spec.Bucket, manifestKey, strings.NewReader(string(data)), int64(len(data)), "application/json"); err != nil {
		return UploadResult{}, fmt.Errorf("upload manifest: %w", err)
	}

	digest, err := manifest.Digest()
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		ManifestURI:    fmt.Sprintf("s3://%s/%s", spec.Bucket, manifestKey),
		ManifestDigest: digest,
		SizeBytes:      manifest.TotalSize(),
	}, nil
}

// stagedFiles returns the regular files under dir as sorted relative paths.
func stagedFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan staging dir: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p) // #nosec G304 -- p is a staged backup file under a manager-owned dir
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func putFile(ctx context.Context, store ObjectStore, bucket, key, p string, size int64) error {
	f, err := os.Open(p) // #nosec G304 -- p is a staged backup file under a manager-owned dir
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := store.Put(ctx, bucket, key, f, size, "application/octet-stream"); err != nil {
		return err
	}
	return nil
}
