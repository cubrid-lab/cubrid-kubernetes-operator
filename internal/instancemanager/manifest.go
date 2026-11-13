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
	"math"
	"path"
	"strings"
)

// ManifestVersion is the schema version of BackupManifest. It is versioned from
// day one because restore/rebuild (ADR-0008/0006) consume this manifest as an
// API: a future field/shape change must be detectable, and an unknown version
// must be rejected rather than silently mis-read.
//
// Version 2 is version 1 with the meanings this build gives its fields:
// cubridVersion is the engine version the member read from cubrid_rel (#621)
// and sourceRole the role the member reported (#756). A version 1 manifest is
// still read (manifestVersionRequestedRole), with the meaning it was written
// with (docs/governance.md, #810).
const ManifestVersion = 2

// manifestVersionRequestedRole is the version of the manifests written before
// #756 and #621: their sourceRole is the role the backup request named, which
// a demoted master's backup could name as master, and their cubridVersion may
// be the declared spec.version. Verify refuses such a manifest where the
// source role must have been observed, and checks its cubridVersion by engine
// series only, which a declared version can answer.
const manifestVersionRequestedRole = 1

// readableManifestVersion reports whether a manifest of version v can be read.
func readableManifestVersion(v int) bool {
	return v == ManifestVersion || v == manifestVersionRequestedRole
}

// MaxManifestBytes is the largest manifest.json a restore or verification
// reads. The manifest comes from the bucket, so it is read through a reader
// that stops one byte past this limit, and a longer one is refused before it
// is decoded: a wrong or hostile store cannot fill the Instance Manager's
// memory with it. UploadBackup writes an entry of a few hundred bytes per
// object (key, size, SHA-256 and indentation), so 16 MiB holds
// MaxManifestObjects entries with keys of up to about 1.5 KiB each.
const MaxManifestBytes = 16 << 20

// MaxManifestObjects is the largest number of objects a manifest may list.
// backupdb writes one volume per database volume and backup level and a few
// small files beside them, so a real backup lists tens of objects, not
// thousands.
const MaxManifestObjects = 10000

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
	// CubridVersion is the engine version that produced the backup, as the
	// member that ran backupdb read it from cubrid_rel (ADR-0011). A version 1
	// manifest may carry the declared spec.version instead.
	CubridVersion string `json:"cubridVersion"`
	// Level is the CUBRID backup level (0 = full).
	Level int `json:"level"`
	// SourceInstance is the pod that ran the backup.
	SourceInstance string `json:"sourceInstance"`
	// SourceRole is the role the source member itself reported from `cubrid
	// heartbeat status` when the backup started: master, slave or replica, or
	// unknown for a member with no HA role (a standalone server). A request
	// that named another role fails instead. After backupdb the role is read
	// again: a master or a seed source whose role differs then is not
	// uploaded, and a slave or replica that CUBRID promoted meanwhile is
	// uploaded with the role it had at the start. The role is not held for
	// the whole backup, and a change after the last reading is not excluded.
	// Rebuild policy depends on this (ADR-0006). A version 1 manifest carries
	// the role the request named, so Verify refuses it where a role is
	// required.
	SourceRole string `json:"sourceRole"`
	// CreatedAt is the backup completion time (RFC3339).
	CreatedAt string `json:"createdAt"`
	// OperationID is the Instance Manager operation that produced the backup;
	// its record names the staging directory the objects were read from.
	// Consumers do not check it; it is empty in manifests written before it.
	OperationID string `json:"operationID,omitempty"`
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
// consumer never silently mis-reads a future format (ADR-0007/0008). It also
// rejects a manifest longer than MaxManifestBytes, and an object list no
// backup can have, before any object is downloaded: more than
// MaxManifestObjects objects, a negative size, sizes whose sum does not fit in
// an int64, or a checksum that is not a lowercase hex SHA-256.
func ParseManifest(data []byte) (BackupManifest, error) {
	if len(data) > MaxManifestBytes {
		return BackupManifest{}, fmt.Errorf("manifest is larger than %d bytes", MaxManifestBytes)
	}
	var m BackupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return BackupManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if !readableManifestVersion(m.ManifestVersion) {
		return BackupManifest{}, fmt.Errorf("unsupported manifest version %d (want %d or %d)",
			m.ManifestVersion, ManifestVersion, manifestVersionRequestedRole)
	}
	if err := m.validateObjectList(); err != nil {
		return BackupManifest{}, err
	}
	return m, nil
}

// validateObjectList checks the count, sizes and checksums of the listed
// objects. Object keys are checked separately by ValidateObjectKeys.
func (m BackupManifest) validateObjectList() error {
	if len(m.Objects) > MaxManifestObjects {
		return fmt.Errorf("manifest lists %d objects, more than the %d allowed", len(m.Objects), MaxManifestObjects)
	}
	var total int64
	for _, obj := range m.Objects {
		if obj.SizeBytes < 0 {
			return fmt.Errorf("manifest object %q has negative size %d", obj.Key, obj.SizeBytes)
		}
		if total > math.MaxInt64-obj.SizeBytes {
			return fmt.Errorf("manifest object sizes add up to more than %d bytes", int64(math.MaxInt64))
		}
		total += obj.SizeBytes
		if !isSHA256Hex(obj.SHA256) {
			return fmt.Errorf("manifest object %q has a checksum that is not a lowercase hex SHA-256", obj.Key)
		}
	}
	return nil
}

// isSHA256Hex reports whether s is a SHA-256 digest in lowercase hex, the
// form UploadBackup writes and the form a staged file's digest is compared in.
func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ManifestExpectation is what a restore/rebuild consumer requires of a manifest
// before trusting it (ADR-0008 validation gate). Empty fields are not checked,
// so a caller opts into each guard it needs.
type ManifestExpectation struct {
	// Database, when set, must equal the manifest's database.
	Database string
	// CubridVersion, when set, must be of the manifest's engine series: no
	// restore across series, which is an engine upgrade (ADR-0009, ADR-0011).
	// A manifest version that cannot be reduced to a series never matches.
	CubridVersion string
	// Level, when set (non-nil), must equal the manifest's backup level.
	Level *int
	// RequireSourceRole, when non-empty, must equal the manifest's source role
	// (ADR-0006 rebuild seeds only from the resolved master).
	RequireSourceRole string
	// SourceInstance, when set, must equal the member the backup ran on.
	SourceInstance string
	// ClusterUID, when set, must equal the cluster the backup was taken in.
	ClusterUID string
}

// Verify rejects a manifest whose identity does not match the intended
// operation (ADR-0007/0008 artifact trust). It checks metadata only; per-object
// checksum validation is the caller's separate step.
func (m BackupManifest) Verify(exp ManifestExpectation) error {
	if !readableManifestVersion(m.ManifestVersion) {
		return fmt.Errorf("manifest version %d does not match supported %d or %d",
			m.ManifestVersion, ManifestVersion, manifestVersionRequestedRole)
	}
	if exp.Database != "" && m.Database != exp.Database {
		return fmt.Errorf("manifest database %q does not match expected %q", m.Database, exp.Database)
	}
	if exp.CubridVersion != "" {
		if series := EngineSeries(m.CubridVersion); series == "" || series != EngineSeries(exp.CubridVersion) {
			return fmt.Errorf("manifest cubridVersion %q is not of the series of %q", m.CubridVersion, exp.CubridVersion)
		}
	}
	if exp.Level != nil && m.Level != *exp.Level {
		return fmt.Errorf("manifest level %d does not match expected %d", m.Level, *exp.Level)
	}
	if exp.RequireSourceRole != "" && m.ManifestVersion == manifestVersionRequestedRole {
		return fmt.Errorf("manifest version %d records the role the backup request named, not the role "+
			"the member reported, so it cannot show the backup was taken on a %s", m.ManifestVersion, exp.RequireSourceRole)
	}
	if exp.RequireSourceRole != "" && m.SourceRole != exp.RequireSourceRole {
		return fmt.Errorf("manifest sourceRole %q does not match required %q", m.SourceRole, exp.RequireSourceRole)
	}
	if exp.SourceInstance != "" && m.SourceInstance != exp.SourceInstance {
		return fmt.Errorf("manifest sourceInstance %q does not match expected %q", m.SourceInstance, exp.SourceInstance)
	}
	if exp.ClusterUID != "" && m.ClusterUID != exp.ClusterUID {
		return fmt.Errorf("manifest clusterUID %q does not match expected %q", m.ClusterUID, exp.ClusterUID)
	}
	if len(m.Objects) == 0 {
		return fmt.Errorf("manifest lists no backup objects")
	}
	return nil
}

// objectKeyRoot is the directory, relative to an artifact's prefix, that
// holds its backup objects (UploadBackup writes them there).
const objectKeyRoot = "backup"

// ValidateObjectKeys checks that every object key is a clean, relative path
// strictly below backup/ and that no key appears twice. The manifest is read
// from the bucket: a key decides where a file is written on restore and which
// object is read, so it must not be able to name anything outside the
// artifact.
func (m BackupManifest) ValidateObjectKeys() error {
	seen := make(map[string]bool, len(m.Objects))
	for _, obj := range m.Objects {
		key := obj.Key
		rest, below := strings.CutPrefix(key, objectKeyRoot+"/")
		switch {
		case key == "" || strings.ContainsRune(key, 0) || strings.Contains(key, "\\"):
			return fmt.Errorf("manifest object key %q is not a usable path", key)
		case path.IsAbs(key) || path.Clean(key) != key:
			return fmt.Errorf("manifest object key %q is not a clean relative path", key)
		case !below || rest == "":
			return fmt.Errorf("manifest object key %q is not below %s/", key, objectKeyRoot)
		}
		for element := range strings.SplitSeq(key, "/") {
			if element == ".." || element == "." {
				return fmt.Errorf("manifest object key %q is not a clean relative path", key)
			}
		}
		if seen[key] {
			return fmt.Errorf("manifest lists object key %q twice", key)
		}
		seen[key] = true
	}
	return nil
}
