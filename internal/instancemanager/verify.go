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
	"math"
	"path"
)

// VerifyArtifact downloads and validates a backup artifact before a restore or
// rebuild trusts it (ADR-0007/0008). It fetches manifest.json under prefix,
// checks its identity against exp, then re-downloads every listed object and
// verifies its SHA-256. Any mismatch — wrong metadata, missing object, or
// checksum drift — fails; a partial/tampered artifact is never trusted.
func VerifyArtifact(ctx context.Context, store ObjectStore, bucket, prefix string, exp ManifestExpectation) (BackupManifest, error) {
	manifestKey := path.Join(prefix, ManifestObjectName)
	data, err := readObject(ctx, store, bucket, manifestKey, MaxManifestBytes)
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
	if err := m.ValidateObjectKeys(); err != nil {
		return BackupManifest{}, err
	}

	for _, obj := range m.Objects {
		key := path.Join(prefix, obj.Key)
		sum, size, err := hashObject(ctx, store, bucket, key, obj.SizeBytes)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("verify object %s: %w", obj.Key, err)
		}
		if size > obj.SizeBytes {
			return BackupManifest{}, fmt.Errorf("object %s is larger than its manifest size %d", obj.Key, obj.SizeBytes)
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

// readObject reads an object of at most limit bytes into memory. It reads at
// most one byte more than limit, so a longer or endless body is refused
// without being read to its end; the error does not quote the body.
func readObject(ctx context.Context, store ObjectStore, bucket, key string, limit int64) ([]byte, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, beyond(limit)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, limit)
	}
	return data, nil
}

// beyond returns the number of bytes to read of a stream that must hold at
// most limit bytes: one more than limit, so that a longer stream shows, and
// limit itself where one more does not fit in an int64.
func beyond(limit int64) int64 {
	if limit == math.MaxInt64 {
		return limit
	}
	return limit + 1
}

// hashObject returns the SHA-256 and length of an object that the manifest
// says holds size bytes. It reads at most one byte past size, so a returned
// length above size means the object is longer, and the rest of its body is
// not read.
func hashObject(ctx context.Context, store ObjectStore, bucket, key string, size int64) (string, int64, error) {
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, beyond(size)))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
