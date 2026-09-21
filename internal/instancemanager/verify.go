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
	"path"
)

// VerifyArtifact downloads and validates a backup artifact before a restore or
// rebuild trusts it (ADR-0007/0008). It fetches manifest.json under prefix,
// checks its identity against exp, then re-downloads every listed object and
// verifies its SHA-256. Any mismatch — wrong metadata, missing object, or
// checksum drift — fails; a partial/tampered artifact is never trusted.
func VerifyArtifact(ctx context.Context, store ObjectStore, bucket, prefix string, exp ManifestExpectation) (BackupManifest, error) {
	manifestKey := path.Join(prefix, ManifestObjectName)
	data, err := readObject(ctx, store, bucket, manifestKey)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return BackupManifest{}, err
	}
	if err := m.Verify(exp); err != nil {
		return BackupManifest{}, err
	}

	for _, obj := range m.Objects {
		key := path.Join(prefix, obj.Key)
		sum, size, err := hashObject(ctx, store, bucket, key)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("verify object %s: %w", obj.Key, err)
		}
		if size != obj.SizeBytes {
			return BackupManifest{}, fmt.Errorf("object %s size %d does not match manifest %d", obj.Key, size, obj.SizeBytes)
		}
		if sum != obj.SHA256 {
			return BackupManifest{}, fmt.Errorf("object %s checksum mismatch", obj.Key)
		}
	}
	return m, nil
}

func readObject(ctx context.Context, store ObjectStore, bucket, key string) ([]byte, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func hashObject(ctx context.Context, store ObjectStore, bucket, key string) (string, int64, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
