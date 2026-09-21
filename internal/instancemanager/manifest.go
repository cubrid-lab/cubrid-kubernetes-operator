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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ManifestVersion is the schema version of BackupManifest. It is versioned from
// day one because restore/rebuild (ADR-0008/0006) consume this manifest as an
// API: a future field/shape change must be detectable, and an unknown version
// must be rejected rather than silently mis-read.
const ManifestVersion = 1

// ManifestObjectName is the manifest's object key within a backup prefix. It is
// written LAST and is the atomic completion marker: a backup is trustworthy only
// if this object exists and validates every listed object (ADR-0007).
const ManifestObjectName = "manifest.json"

// BackupManifest is the durable, self-describing record of one backup artifact
// (ADR-0007). Restore/rebuild MUST reject a manifest whose database, clusterUID,
// cubridVersion, level, or source-role policy does not match the intended
// operation, in addition to per-file checksum validation.
type BackupManifest struct {
	// ManifestVersion is the schema version; consumers reject unknown versions.
	ManifestVersion int `json:"manifestVersion"`
	// Database is the backed-up CUBRID database name.
	Database string `json:"database"`
	// ClusterUID is the source CubridCluster UID (artifact-trust check).
	ClusterUID string `json:"clusterUID"`
	// CubridVersion is the engine version that produced the backup.
	CubridVersion string `json:"cubridVersion"`
	// Level is the CUBRID backup level (0 = full).
	Level int `json:"level"`
	// SourceInstance is the pod that ran the backup.
	SourceInstance string `json:"sourceInstance"`
	// SourceRole is the ADR-0005 role of the source at backup start (master or
	// slave); rebuild policy depends on this (ADR-0006).
	SourceRole string `json:"sourceRole"`
	// CreatedAt is the backup completion time (RFC3339).
	CreatedAt string `json:"createdAt"`
	// Objects lists every backup object with its checksum for validation.
	Objects []ManifestObject `json:"objects"`
}

// ManifestObject is one uploaded backup object with its integrity metadata.
type ManifestObject struct {
	// Key is the object key relative to the backup prefix.
	Key string `json:"key"`
	// SizeBytes is the object size.
	SizeBytes int64 `json:"sizeBytes"`
	// SHA256 is the lowercase hex checksum of the object contents.
	SHA256 string `json:"sha256"`
}

// TotalSize returns the sum of all object sizes.
func (m BackupManifest) TotalSize() int64 {
	var n int64
	for _, o := range m.Objects {
		n += o.SizeBytes
	}
	return n
}

// Marshal returns the canonical JSON encoding of the manifest.
func (m BackupManifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Digest returns the SHA-256 hex digest of the manifest's canonical JSON, used
// as the manifest's own integrity marker in status.
func (m BackupManifest) Digest() (string, error) {
	data, err := m.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ParseManifest decodes a manifest and rejects an unknown schema version so a
// consumer never silently mis-reads a future format (ADR-0007/0008).
func ParseManifest(data []byte) (BackupManifest, error) {
	var m BackupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return BackupManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if m.ManifestVersion != ManifestVersion {
		return BackupManifest{}, fmt.Errorf("unsupported manifest version %d (want %d)", m.ManifestVersion, ManifestVersion)
	}
	return m, nil
}
